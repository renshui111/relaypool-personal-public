package api

import (
	"strings"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/store"
)

func TestRequestLogTracksRoutedAttempts(t *testing.T) {
	const body = `{"model":"glm-5.2","messages":[{"role":"user","content":"test"}]}`
	for _, tc := range []struct {
		name, path, failKind, lastStatus string
		cooling                          bool
		status, count                    int
	}{
		{"failover", "/v1/chat/completions", "unavailable", store.StatusOK, false, 200, 2},
		{"invalid", "/v1/chat/completions", "invalid_request", store.StatusError, false, 400, 1},
		{"pinned cooling", "/admin/accounts/acc_a/chat/completions", "", "", true, 429, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServerNoAdapter(t)
			p := newStubProvider()
			p.failuresBeforeSuccess, p.failKind = 1, tc.failKind
			s.registry.Register(p)
			addAccount(t, s, "acc_a", p.family)
			addAccount(t, s, "acc_b", p.family)
			if tc.cooling {
				s.pool.MarkClassified("acc_a", pool.Classified{Kind: pool.KindUnavailable, Model: "glm-5.2", Cooldown: time.Minute}, 3)
			}
			w := callComplete(t, s, "POST", tc.path, body)
			if w.Code != tc.status || p.calls != tc.count {
				t.Fatalf("status=%d calls=%d: %s", w.Code, p.calls, w.Body.String())
			}
			rows, err := s.store.ListRequests(t.Context(), store.RequestLogFilter{Limit: 1})
			if err != nil || len(rows) != 1 {
				t.Fatalf("logs=%+v err=%v", rows, err)
			}
			row := rows[0]
			detail, err := s.store.ListAttempts(t.Context(), row.ID)
			if err != nil || len(detail) != tc.count || row.AttemptCount != tc.count {
				t.Fatalf("row=%+v detail=%+v err=%v", row, detail, err)
			}
			if tc.cooling {
				if !strings.Contains(w.Body.String(), "all_accounts_cooling") || w.Header().Get("Retry-After") == "" || row.AccountID != "" {
					t.Fatalf("incorrect pinned cooldown: %+v %s", row, w.Body.String())
				}
				return
			}
			for index, a := range detail {
				if a.AttemptIndex != index || a.AccountID == "" || a.Provider != p.family || a.LatencyMs == nil {
					t.Fatal(a)
				}
			}
			last := detail[len(detail)-1]
			if last.Status != tc.lastStatus || row.AccountID != last.AccountID || row.Provider != last.Provider || row.Status != last.Status || detail[0].ErrorKind == "" {
				t.Fatalf("summary=%+v attempts=%+v", row, detail)
			}
			if tc.count == 2 && (detail[0].AccountID == last.AccountID || detail[0].Status != store.StatusError) {
				t.Fatal(detail)
			}
		})
	}
}
