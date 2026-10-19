package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/store"
)

func validAccountQuotaScope(scope string) bool {
	return scope == "" || scope == "shared" || scope == "account" || scope == "model"
}

// 修改调度配置后同步号池，健康状态由号池保留，不被管理请求覆盖。
func (s *Server) handlePatchAccount(w http.ResponseWriter, r *http.Request) {
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	account, err := s.store.GetAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, 404, "not_found", "账号不存在")
		} else {
			writeError(w, 500, "internal_error", "读取账号失败")
		}
		return
	}
	var req struct {
		Name           *string    `json:"name"`
		MaxInflight    *int       `json:"max_inflight"`
		Priority       *int       `json:"priority"`
		Weight         *int       `json:"weight"`
		QuotaScope     *string    `json:"quota_scope"`
		QuotaGroup     *string    `json:"quota_group"`
		QuotaExpiresAt *time.Time `json:"quota_expires_at"`
		AutoCheckin    *bool      `json:"auto_checkin"`
		DropSystem     *bool      `json:"drop_system_prompt"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		writeError(w, 400, "invalid_request", "请求 JSON 无效")
		return
	}
	if req.Name != nil {
		account.Name = strings.TrimSpace(*req.Name)
		if account.Name == "" {
			writeError(w, 400, "invalid_request", "账号名称不能为空")
			return
		}
	}
	if req.MaxInflight != nil {
		account.MaxInflight = *req.MaxInflight
	}
	if req.Priority != nil {
		account.Priority = *req.Priority
	}
	if req.Weight != nil {
		account.Weight = *req.Weight
	}
	if req.QuotaScope != nil {
		account.QuotaScope = *req.QuotaScope
	}
	if req.QuotaGroup != nil {
		account.QuotaGroup = strings.TrimSpace(*req.QuotaGroup)
	}
	if req.QuotaExpiresAt != nil {
		account.QuotaExpiresAt = *req.QuotaExpiresAt
	}
	if len(account.QuotaGroup) > 128 {
		writeError(w, 400, "invalid_request", "额度组名称不能超过 128 字节")
		return
	}
	if req.AutoCheckin != nil {
		if *req.AutoCheckin && !accountCheckinCapability(account).Supported {
			writeError(w, 400, "unsupported_checkin", accountCheckinCapability(account).Note)
			return
		}
		account.AutoCheckin = *req.AutoCheckin
	}
	if req.DropSystem != nil {
		account.DropSystemPrompt = *req.DropSystem
	}
	if account.MaxInflight < 0 || account.Weight < 1 || account.Weight > 1000000 || account.QuotaScope == "" || !validAccountQuotaScope(account.QuotaScope) {
		writeError(w, 400, "invalid_request", "并发上限须为非负数，权重须为正数，额度范围须为 shared、account 或 model")
		return
	}
	if err := s.store.UpdateAccount(r.Context(), account); err != nil {
		writeError(w, 500, "internal_error", "保存账号配置失败")
		return
	}
	s.pool.Upsert(s.accountPoolItem(account))
	view := accountView(account)
	if live, ok := s.pool.ByID(account.ID); ok {
		applyLiveRuntime(view, live)
	}
	writeJSON(w, http.StatusOK, view)
}
