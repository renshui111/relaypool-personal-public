package api

import (
	"errors"
	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/scheduler"
	"github.com/relaypool/relaypool/internal/store"
	"net/http"
)

func accountCheckinCapability(a store.Account) provider.CheckinCapability {
	return provider.CheckinFor(a.Provider, a.Region)
}

func (s *Server) handleAccountCheckin(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.GetAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "not_found", "账号不存在")
		return
	}
	if !a.Enabled {
		writeError(w, 400, "account_disabled", "请先启用账号再领取奖励")
		return
	}
	result, err := s.scheduler.Checkin(r.Context(), scheduler.AccountRef{ID: a.ID, Provider: a.Provider, Region: a.Region, Enabled: a.Enabled}, true)
	if err != nil {
		if errors.Is(err, provider.ErrUnsupported) {
			writeError(w, 400, "unsupported_checkin", accountCheckinCapability(a).Note)
			return
		}
		writeError(w, 502, "checkin_failed", "领取未成功，请检查登录态、领取资格和原设备信息；若正在领取，请稍后查看结果")
		return
	}
	writeJSON(w, 200, map[string]any{"status": result.Status, "message": result.Message})
}
