package api

import (
	"net/http"
	"time"
)

// 只公开管理员需要的执行计划，不返回其他部署配置或任何凭据。
func (s *Server) handleCheckinSchedule(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":    s.cfg.Schedule.CheckinEnabled,
		"hours":      append([]int{}, s.cfg.Schedule.CheckinHours...),
		"timezone":   now.Location().String(),
		"utc_offset": now.Format("-07:00"),
	})
}
