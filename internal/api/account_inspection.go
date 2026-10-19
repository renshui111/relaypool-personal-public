package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

type quotaCacheEntry struct {
	key     string
	value   provider.QuotaSnapshot
	expires time.Time
	done    chan struct{}
}

func (s *Server) quotaSupported(family string) bool {
	adapter, err := s.registry.Lookup(family)
	if err != nil {
		return false
	}
	quota, ok := adapter.(provider.QuotaProvider)
	return ok && quota.SupportsQuota()
}

// 只缓存非敏感显示数据，凭证指纹参与校验；余额查询不调用健康报告或启停接口。
func (s *Server) queryQuota(ctx context.Context, id string, cred provider.Credential, adapter provider.QuotaProvider, force bool) provider.QuotaSnapshot {
	sum := sha256.Sum256(cred.Raw)
	key := cred.Provider + ":" + hex.EncodeToString(sum[:])
	for {
		s.inspectionMu.Lock()
		if s.quotaCache == nil {
			s.quotaCache = map[string]*quotaCacheEntry{}
		}
		entry := s.quotaCache[id]
		if entry != nil && entry.done != nil {
			done := entry.done
			s.inspectionMu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return provider.QuotaSnapshot{State: "error", Message: "额度查询已取消"}
			}
		}
		now := time.Now()
		if entry != nil && entry.key == key && entry.expires.After(now) && (!force || now.Sub(entry.value.CheckedAt) < 5*time.Second) {
			value := entry.value
			s.inspectionMu.Unlock()
			return value
		}
		// 有上限的缓存，已失效且没有在途查询的条目可安全移除。
		if len(s.quotaCache) >= 2048 && entry == nil {
			for other, cached := range s.quotaCache {
				if cached.done == nil {
					delete(s.quotaCache, other)
					break
				}
			}
			if len(s.quotaCache) >= 2048 {
				s.inspectionMu.Unlock()
				return provider.QuotaSnapshot{State: "error", Message: "额度查询较多，请稍后刷新"}
			}
		}
		entry = &quotaCacheEntry{key: key, done: make(chan struct{})}
		s.quotaCache[id] = entry
		s.inspectionMu.Unlock()
		value, err := adapter.FetchQuota(ctx, cred)
		value.CheckedAt = time.Now().UTC()
		if err != nil {
			value = provider.QuotaSnapshot{State: "error", CheckedAt: time.Now().UTC(), Message: "额度查询失败，请检查网络、登录状态或上游额度页面"}
		}
		ttl := time.Minute
		if value.State != "ok" {
			ttl = 15 * time.Second
		}
		s.inspectionMu.Lock()
		entry.value = value
		entry.expires = time.Now().Add(ttl)
		close(entry.done)
		entry.done = nil
		s.inspectionMu.Unlock()
		return value
	}
}

func (s *Server) handleAccountQuota(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	account, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "账号不存在")
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", "无法读取账号")
		}
		return
	}
	if !s.quotaSupported(account.Provider) {
		writeJSON(w, http.StatusOK, provider.QuotaSnapshot{State: "unsupported", Message: "此渠道尚未接入余额接口；调用用量不能代替剩余额度"})
		return
	}
	if !account.HasCredential {
		writeJSON(w, http.StatusOK, provider.QuotaSnapshot{State: "unknown", Message: "尚未配置凭证"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	// 余额读取不主动轮换登录令牌，不改变人工停用状态。
	cred, err := (credentialSource{store: s.store, registry: s.registry}).loadCredential(ctx, id, false)
	if err != nil {
		writeJSON(w, http.StatusOK, provider.QuotaSnapshot{State: "error", Message: "无法读取该账号凭证，请检查配置"})
		return
	}
	adapter, _ := s.registry.Lookup(account.Provider)
	quota := adapter.(provider.QuotaProvider)
	value := s.queryQuota(ctx, id, cred, quota, r.URL.Query().Get("refresh") == "1")
	current, currentErr := s.store.GetCredential(ctx, id)
	if currentErr != nil || !bytes.Equal(current.Payload, cred.Raw) {
		value = provider.QuotaSnapshot{State: "unknown", Message: "账号凭证已变化，请重新查询额度"}
	}
	writeJSON(w, http.StatusOK, value)
}

// 直接查询上游目录，不把手填型号伪装成上游返回；失败保留原目录与调度状态。
func (s *Server) handleAccountCatalog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	account, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "账号不存在")
		} else {
			writeError(w, http.StatusInternalServerError, "internal_error", "无法读取账号")
		}
		return
	}
	if !account.HasCredential {
		writeJSON(w, http.StatusOK, map[string]any{"state": "unknown", "message": "尚未配置凭证"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cred, err := (credentialSource{store: s.store, registry: s.registry}).loadCredential(ctx, id, false)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"state": "error", "message": "无法读取账号凭证"})
		return
	}
	if cred.Provider == "codex" || cred.Field("mode") == "code_assist" {
		writeJSON(w, http.StatusOK, map[string]any{"state": "unsupported", "message": "此订阅渠道没有已接入的上游目录接口，请填写本人实际可用的型号"})
		return
	}
	fields := make(map[string]string, len(cred.Fields))
	for k, v := range cred.Fields {
		fields[k] = v
	}
	fields["models"] = "[]"
	cred.Fields = fields
	adapter, lookupErr := s.registry.Lookup(account.Provider)
	if lookupErr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"state": "unsupported", "message": "此渠道没有可用的目录适配器"})
		return
	}
	models, err := adapter.FetchCatalog(ctx, cred)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"state": "error", "message": "上游目录查询失败，已保留原型号；此平台可能不开放目录接口"})
		return
	}
	names := []string{}
	seen := map[string]bool{}
	for _, m := range models {
		name := strings.TrimSpace(m.NativeModel)
		if name != "" && !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	if len(names) > 2048 {
		writeJSON(w, http.StatusOK, map[string]any{"state": "error", "message": "上游目录超过查询上限，已保留原型号"})
		return
	}
	if len(names) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"state": "unknown", "message": "上游未返回型号，已保留原目录"})
		return
	}
	// 网络请求期间允许修改账号；提交目录前确认仍为同一份凭证，避免旧结果覆盖新配置。
	s.accountMu.Lock()
	current, currentErr := s.store.GetCredential(ctx, id)
	if currentErr != nil || !bytes.Equal(current.Payload, cred.Raw) {
		s.accountMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"state": "unknown", "message": "账号凭证已变化，请重新查询上游型号"})
		return
	}
	s.pool.MergeModels(id, names)
	s.accountMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"state": "ok", "source": "upstream", "models": names, "checked_at": time.Now().UTC(), "message": "来自上游目录，渠道可能使用短时缓存；目录不等于本账号已获准调用全部型号"})
}
