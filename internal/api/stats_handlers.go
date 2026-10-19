package api

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/store"
)

// Stats windows the console can request, as (label, duration, bucket duration).
//
// The bucket width is chosen per window so the chart keeps a readable number of
// points: a 24h window at 1-minute resolution would be 1440 points, which is
// both slow to render and impossible to read.
var statsWindows = map[string]struct {
	Duration time.Duration
	Bucket   time.Duration
	Buckets  int
	Label    string
}{
	"1h":  {Duration: time.Hour, Bucket: time.Minute, Buckets: 60, Label: "1 小时"},
	"24h": {Duration: 24 * time.Hour, Bucket: time.Hour, Buckets: 24, Label: "24 小时"},
	"7d":  {Duration: 7 * 24 * time.Hour, Bucket: 6 * time.Hour, Buckets: 28, Label: "7 天"},
	"30d": {Duration: 30 * 24 * time.Hour, Bucket: 24 * time.Hour, Buckets: 30, Label: "30 天"},
}

// defaultStatsWindow is used when the request omits one.
const defaultStatsWindow = "24h"

// handleStats reports the aggregated overview numbers.
//
// Everything the overview page needs arrives in one response so the cards and
// their chart cannot disagree: computing them from separate requests would let
// a request land between two of them and produce a total that does not match
// the series it is drawn next to.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	key := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("window")))
	if key == "" {
		key = defaultStatsWindow
	}
	spec, ok := statsWindows[key]
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"window must be one of 1h, 24h, 7d, 30d")
		return
	}

	// Allow the caller to shrink the bucket count for a small screen, but keep
	// it inside a range that still produces a meaningful chart.
	buckets := spec.Buckets
	if raw := strings.TrimSpace(r.URL.Query().Get("buckets")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 4 && n <= 240 {
			buckets = n
		}
	}

	since := time.Now().UTC().Add(-spec.Duration)
	// 缩减图表分段时仍覆盖整个窗口，不能只显示末尾几分钟。
	stats, err := s.store.OverviewStats(r.Context(), since, buckets, spec.Duration/time.Duration(buckets))
	if err != nil {
		log.Printf("admin: stats: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to compute statistics")
		return
	}
	stats.Window = key

	// The stored aggregate cannot know how many accounts exist NOW: the log only
	// records account IDs that appeared in some request, so a DELETED account
	// keeps inflating the count forever (a live deployment reported 8 while 3
	// existed). The pool is the authoritative tally, so it overrides. The
	// "unattributed" bucket is not a channel and has no accounts of its own.
	liveAccounts := map[string]int64{}
	for _, item := range s.pool.Items() {
		liveAccounts[item.Provider]++
	}
	for i := range stats.Provider {
		if stats.Provider[i].Provider == store.UnattributedProvider {
			stats.Provider[i].Accounts = 0
			continue
		}
		stats.Provider[i].Accounts = liveAccounts[stats.Provider[i].Provider]
	}

	// The window options are advertised so the console renders the switcher from
	// the server's own list rather than hard-coding a second copy.
	windows := make([]map[string]any, 0, len(statsWindows))
	for name, w := range statsWindows {
		windows = append(windows, map[string]any{
			"key": name, "label": w.Label, "bucket": w.Bucket.String(),
		})
	}
	// Deterministic order for a stable UI.
	order := []string{"1h", "24h", "7d", "30d"}
	ordered := make([]map[string]any, 0, len(windows))
	for _, name := range order {
		for _, item := range windows {
			if item["key"] == name {
				ordered = append(ordered, item)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"window":       key,
		"window_label": spec.Label,
		"since":        stats.Since,
		"until":        stats.Until,
		"bucket":       stats.Bucketed,
		"windows":      ordered,
		"summary":      stats.Summary,
		"series":       stats.Series,
		"models":       stats.Models,
		"providers":    stats.Provider,
		"errors":       stats.Errors,
		// 请求日志是可清理的记录，不是永久用量账本。
		"retention_days":  s.cfg.Logging.RetentionDays,
		"logging_enabled": s.cfg.Logging.RequestLogEnabled,
		// Live pool state, so the overview does not need a second call for the
		// account cards it already shows.
		"health": s.liveHealth(),
	})
}

// liveHealth summarizes the in-memory pool for the overview.
//
// It reads the pool rather than the database for the same reason the account
// list does: cooldowns and in-flight counts change every second, and the stored
// copy lags by the state writer's flush interval.
func (s *Server) liveHealth() map[string]any {
	items := s.pool.Items()

	var enabled, ready, cooling, offline, inFlight int
	models := map[string]struct{}{}
	providerAccounts := map[string]int{}

	for _, item := range items {
		if !item.Enabled {
			continue
		}
		enabled++
		inFlight += item.InFlight
		for _, m := range item.Models {
			if m != "" {
				models[m] = struct{}{}
			}
		}
		providerAccounts[item.Provider]++
		switch {
		case item.DownUntil.After(time.Now()):
			cooling++
		case item.Ready != nil && !*item.Ready:
			offline++
		default:
			ready++
		}
	}

	return map[string]any{
		"accounts_total":   len(items),
		"accounts_enabled": enabled,
		"accounts_ready":   ready,
		"accounts_cooling": cooling,
		"accounts_offline": offline,
		"in_flight":        inFlight,
		"models":           len(models),
		"providers":        providerAccounts,
		"routing":          s.pool.Strategy(),
		"session_affinity": s.sessions.Stats(),
	}
}
