package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

type keyPermissionsContextKey struct{}

func keyFromRequest(r *http.Request) store.APIKey {
	k, _ := r.Context().Value(keyPermissionsContextKey{}).(store.APIKey)
	return k
}

func keyView(k store.APIKey) map[string]any {
	return map[string]any{"id": k.ID, "name": k.Name, "scope": k.Scope, "key_prefix": k.KeyPrefix, "enabled": k.Enabled, "created_at": k.CreatedAt, "last_used_at": k.LastUsed, "permissions": k.Permissions}
}

// 只允许管理者更改授权，模型名规范化与真实选号使用同一规则。
func (s *Server) validateKeyPermissions(ctx context.Context, p store.KeyPermissions) (store.KeyPermissions, error) {
	p, err := store.NormalizeKeyPermissions(p)
	if err != nil {
		return p, err
	}
	for _, id := range p.AccountIDs {
		if _, err := s.store.GetAccount(ctx, id); err != nil {
			return p, fmt.Errorf("所选账号不存在，请刷新账号列表")
		}
	}
	for i, model := range p.Models {
		native, channel := s.modelRoute(model)
		native = pool.CanonicalModelID(native)
		if native == "" || native == "auto" || native == "cn" || native == "global" || strings.Contains(native, "*") {
			return p, fmt.Errorf("模型白名单须填写具体型号，不能使用自动选型、区域别名或通配符")
		}
		if channel != "" {
			native = channel + "/" + native
		}
		p.Models[i] = native
	}
	return store.NormalizeKeyPermissions(p)
}

// 一个裸型号允许多个渠道，同名带渠道的型号只授权该渠道；不使用前缀匹配。
func (s *Server) keyAllowsModel(p store.KeyPermissions, model, channel string) bool {
	if p.AllModels {
		return true
	}
	model = pool.CanonicalModelID(model)
	if model == "" || model == "auto" {
		return false
	}
	for _, rule := range p.Models {
		native, provider := s.modelRoute(rule)
		if pool.CanonicalModelID(native) == model && (provider == "" || provider == channel) {
			return true
		}
	}
	return false
}

func (s *Server) handleCurrentKeyPermissions(w http.ResponseWriter, r *http.Request) {
	view := keyView(keyFromRequest(r))
	view["armor"] = s.keyArmorState(r)
	writeJSON(w, 200, view)
}

// handleKeyArmor 只接受开关；管理员可指定密钥，调用者只能控制自己的密钥。
func (s *Server) handleKeyArmor(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil || input.Enabled == nil {
		writeError(w, 400, "invalid_request", "请仅提供 enabled 布尔值")
		return
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeError(w, 400, "invalid_request", "请求只能包含一个 JSON 对象")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		id = keyFromRequest(r).ID
	}
	k, err := s.store.SetAPIKeyArmor(r.Context(), id, *input.Enabled)
	if errors.Is(err, store.ErrArmorNotAllowed) {
		writeError(w, 403, "key_permission_denied", err.Error())
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "not_found", "密钥不存在或已撤销")
		return
	}
	if err != nil {
		writeError(w, 500, "internal_error", "保存破甲开关失败")
		return
	}
	writeJSON(w, 200, keyView(k))
}

func (s *Server) handleKeyOptions(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		writeError(w, 500, "internal_error", "读取账号失败")
		return
	}
	data := []map[string]any{}
	for _, a := range accounts {
		data = append(data, map[string]any{"id": a.ID, "name": a.Name, "provider": a.Provider, "enabled": a.Enabled})
	}
	writeJSON(w, 200, map[string]any{"accounts": data, "models": s.modelCatalog(store.DefaultKeyPermissions())})
}

type keyWriteRequest struct {
	Name        *string               `json:"name"`
	Scope       *string               `json:"scope"`
	Permissions *store.KeyPermissions `json:"permissions"`
}

// 密钥授权接口拒绝未知字段、空修改及尾随 JSON，避免误保存宽松权限。
func decodeKeyWrite(w http.ResponseWriter, r *http.Request, req *keyWriteRequest) error {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(req); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("请求只能包含一个 JSON 对象")
	}
	if req.Name == nil && req.Scope == nil && req.Permissions == nil {
		return fmt.Errorf("请提供名称、权限或调用范围")
	}
	return nil
}

func (s *Server) handlePatchKey(w http.ResponseWriter, r *http.Request) {
	var req keyWriteRequest
	if err := decodeKeyWrite(w, r, &req); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if req.Name != nil {
		*req.Name = strings.TrimSpace(*req.Name)
	}
	if req.Permissions != nil {
		p, err := s.validateKeyPermissions(r.Context(), *req.Permissions)
		if err != nil {
			writeError(w, 400, "invalid_request", err.Error())
			return
		}
		req.Permissions = &p
	}
	k, err := s.store.PatchAPIKey(r.Context(), r.PathValue("id"), req.Name, req.Scope, req.Permissions)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "not_found", "密钥不存在或已撤销")
		return
	}
	if err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	writeJSON(w, 200, keyView(k))
}

// 在选号前确定账号与型号权限的交集，目录、固定账号、会话绑定和换号都使用这个集合。
func (s *Server) permittedRouteAccounts(w http.ResponseWriter, r *http.Request, model, channel string) ([]string, bool) {
	p := keyFromRequest(r).Permissions
	if p.AllAccounts && p.AllModels {
		return nil, true
	}
	ids := []string{}
	// 每次请求只构建一次白名单索引，不随账号数量重复解析全部模型规则。
	accounts := make(map[string]struct{}, len(p.AccountIDs))
	for _, id := range p.AccountIDs {
		accounts[id] = struct{}{}
	}
	allChannels := p.AllModels
	channels := map[string]bool{}
	if !p.AllModels {
		model = pool.CanonicalModelID(model)
		if model != "" && model != "auto" {
			for _, rule := range p.Models {
				native, route := s.modelRoute(rule)
				if pool.CanonicalModelID(native) == model {
					if route == "" {
						allChannels = true
						break
					}
					channels[route] = true
				}
			}
		}
	}
	for _, item := range s.pool.Items() {
		_, accountAllowed := accounts[item.ID]
		if (p.AllAccounts || accountAllowed) && (channel == "" || item.Provider == channel) && (allChannels || channels[item.Provider]) {
			ids = append(ids, item.ID)
		}
	}
	if len(ids) == 0 {
		writeError(w, 403, "key_permission_denied", "当前密钥未获准调用这个账号或模型")
		return nil, false
	}
	sort.Strings(ids)
	return ids, true
}

// 会话绑定按密钥隔离，权限修改后旧绑定仍须通过选号过滤。
func keySessionFor(r *http.Request, req *provider.ChatRequest) string {
	key := sessionKeyFor(r, req)
	if key == "" {
		return ""
	}
	return keyFromRequest(r).ID + ":" + key
}
