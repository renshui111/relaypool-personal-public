package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// openTestStore opens a store on a temporary database.
//
// This package previously had no tests at all (its coverage came indirectly
// through the API layer), so the helper lives here rather than being imported.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "relaypool.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedRequest inserts one finalized request row directly, so the aggregates can
// be tested without driving the whole HTTP path.
func seedRequest(t *testing.T, s *Store, r RequestLog) {
	t.Helper()
	ctx := context.Background()
	if err := s.StartRequest(ctx, r); err != nil {
		t.Fatalf("start request: %v", err)
	}
	r.FinishedAt = time.Now().UTC()
	if err := s.FinishRequest(ctx, r); err != nil {
		t.Fatalf("finish request: %v", err)
	}
}

func intp(v int) *int { return &v }

// The summary must count successes, failures and tokens correctly, and must
// report a success percentage rather than a raw ratio.
func TestOverviewStatsSummary(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()

	// 3 successes, 2 failures, with known latencies and token counts.
	seedRequest(t, st, RequestLog{
		ID: "req_ok_1", Status: StatusOK, Protocol: "chat.completions",
		RequestedModel: "glm-5.3", MappedModel: "glm-5.3", Provider: "workbuddy", AccountID: "acc_a",
		LatencyMs: intp(100), TotalTokens: intp(1000), PromptTokens: intp(900), CompletionTokens: intp(100),
	})
	seedRequest(t, st, RequestLog{
		ID: "req_ok_2", Status: StatusOK, Protocol: "chat.completions",
		RequestedModel: "glm-5.3", MappedModel: "glm-5.3", Provider: "workbuddy", AccountID: "acc_a",
		LatencyMs: intp(200), TotalTokens: intp(2000), PromptTokens: intp(1800), CompletionTokens: intp(200),
	})
	seedRequest(t, st, RequestLog{
		ID: "req_ok_3", Status: StatusOK, Protocol: "chat.completions",
		RequestedModel: "deepseek-v4", MappedModel: "deepseek-v4", Provider: "workbuddy", AccountID: "acc_b",
		LatencyMs: intp(300), TotalTokens: intp(3000),
	})
	seedRequest(t, st, RequestLog{
		ID: "req_err_1", Status: StatusError, Protocol: "chat.completions",
		RequestedModel: "glm-5.3", MappedModel: "glm-5.3", Provider: "workbuddy", AccountID: "acc_b",
		LatencyMs: intp(400), ErrorKind: "rate_limit", ErrorCode: "429",
	})
	seedRequest(t, st, RequestLog{
		ID: "req_err_2", Status: StatusError, Protocol: "chat.completions",
		RequestedModel: "kimi-k3", MappedModel: "kimi-k3", Provider: "workbuddy", AccountID: "acc_b",
		LatencyMs: intp(500), ErrorKind: "invalid_request", ErrorCode: "400",
	})

	since := time.Now().UTC().Add(-time.Hour)
	stats, err := st.OverviewStats(ctx, since, 24, time.Hour)
	if err != nil {
		t.Fatalf("overview stats: %v", err)
	}

	s := stats.Summary
	if s.Requests != 5 {
		t.Errorf("requests=%d, want 5", s.Requests)
	}
	if s.Successes != 3 {
		t.Errorf("successes=%d, want 3", s.Successes)
	}
	if s.Failures != 2 {
		t.Errorf("failures=%d, want 2", s.Failures)
	}
	if got := s.SuccessPct; got < 59.9 || got > 60.1 {
		t.Errorf("success_pct=%.2f, want 60", got)
	}
	if s.TotalTokens != 6000 {
		t.Errorf("total_tokens=%d, want 6000", s.TotalTokens)
	}
	// Only 3 of 5 rows reported usage; the console needs that to avoid reading
	// "0 tokens" as "no traffic".
	if s.TokensReported != 3 {
		t.Errorf("tokens_reported=%d, want 3", s.TokensReported)
	}
	if s.LatencyAvg < 299.9 || s.LatencyAvg > 300.1 {
		t.Errorf("latency_avg=%.2f, want 300", s.LatencyAvg)
	}
	// Sorted latencies are 100,200,300,400,500 -> p50=300, p95=500.
	if s.LatencyP50 < 299.9 || s.LatencyP50 > 300.1 {
		t.Errorf("latency_p50=%.2f, want 300", s.LatencyP50)
	}
	if s.LatencyP95 < 499.9 || s.LatencyP95 > 500.1 {
		t.Errorf("latency_p95=%.2f, want 500", s.LatencyP95)
	}
}

// The window must actually exclude older rows: an aggregate that silently
// ignores its own time range is worse than no aggregate.
func TestOverviewStatsHonoursTheWindow(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()

	// One row inside the window, one inserted with an explicitly old timestamp.
	seedRequest(t, st, RequestLog{
		ID: "req_now", Status: StatusOK, Protocol: "chat.completions",
		RequestedModel: "glm-5.3", MappedModel: "glm-5.3", Provider: "workbuddy",
		LatencyMs: intp(100), TotalTokens: intp(10),
	})
	old := time.Now().UTC().Add(-72 * time.Hour)
	if _, err := st.db.ExecContext(ctx, `UPDATE request_logs SET created_at = ? WHERE id = 'req_now'`, formatTime(old)); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	recent, err := st.OverviewStats(ctx, time.Now().UTC().Add(-time.Hour), 24, time.Hour)
	if err != nil {
		t.Fatalf("recent stats: %v", err)
	}
	if recent.Summary.Requests != 0 {
		t.Errorf("a 1h window counted %d requests, want 0 (the row is 72h old)", recent.Summary.Requests)
	}

	wide, err := st.OverviewStats(ctx, time.Now().UTC().Add(-7*24*time.Hour), 24, time.Hour)
	if err != nil {
		t.Fatalf("wide stats: %v", err)
	}
	if wide.Summary.Requests != 1 {
		t.Errorf("a 7d window counted %d requests, want 1", wide.Summary.Requests)
	}
}

// An empty database must produce zeroes and empty arrays, never null: the
// console iterates these and a null would throw.
func TestOverviewStatsOnEmptyDatabase(t *testing.T) {
	st := openTestStore(t)

	stats, err := st.OverviewStats(t.Context(), time.Now().UTC().Add(-time.Hour), 24, time.Hour)
	if err != nil {
		t.Fatalf("overview stats: %v", err)
	}
	if stats.Summary.Requests != 0 || stats.Summary.TotalTokens != 0 {
		t.Errorf("empty summary is not zeroed: %+v", stats.Summary)
	}
	if stats.Series == nil || stats.Models == nil || stats.Provider == nil || stats.Errors == nil {
		t.Errorf("aggregates must be empty slices, not null: series=%v models=%v providers=%v errors=%v",
			stats.Series, stats.Models, stats.Provider, stats.Errors)
	}
	if len(stats.Series) == 0 {
		t.Errorf("the series must always cover the full bucket grid so the chart has an x-axis")
	}
}

// The series must contain every bucket in the range, including empty ones, or
// the chart silently compresses quiet periods and misrepresents the shape.
func TestOverviewStatsSeriesIsContiguous(t *testing.T) {
	st := openTestStore(t)

	seedRequest(t, st, RequestLog{
		ID: "req_s", Status: StatusOK, Protocol: "chat.completions",
		RequestedModel: "glm-5.3", MappedModel: "glm-5.3", Provider: "workbuddy",
		LatencyMs: intp(50), TotalTokens: intp(7),
	})

	const buckets = 12
	stats, err := st.OverviewStats(t.Context(), time.Now().UTC().Add(-time.Hour), buckets, 5*time.Minute)
	if err != nil {
		t.Fatalf("overview stats: %v", err)
	}
	if len(stats.Series) != buckets {
		t.Fatalf("series has %d buckets, want %d", len(stats.Series), buckets)
	}
	// Buckets must be evenly spaced and monotonically increasing.
	for i := 1; i < len(stats.Series); i++ {
		gap := stats.Series[i].Start.Sub(stats.Series[i-1].Start)
		if gap != 5*time.Minute {
			t.Errorf("bucket %d is %s after the previous one, want 5m", i, gap)
		}
		if stats.Series[i].Label == "" {
			t.Errorf("bucket %d has no label", i)
		}
	}
	// The single request must land in exactly one bucket.
	var counted int64
	for _, b := range stats.Series {
		counted += b.Requests
	}
	if counted != 1 {
		t.Errorf("buckets total %d requests, want 1", counted)
	}
}

// Errors must be grouped by classified kind, which is the axis an operator
// reasons about (it matches the scheduler's decision table).
func TestOverviewStatsErrorComposition(t *testing.T) {
	st := openTestStore(t)

	for i := 0; i < 3; i++ {
		seedRequest(t, st, RequestLog{
			ID: "req_er_" + string(rune('a'+i)), Status: StatusError, Protocol: "chat.completions",
			RequestedModel: "glm-5.3", MappedModel: "glm-5.3", Provider: "workbuddy",
			ErrorKind: "quota", ErrorCode: "403", ErrorMessage: "额度不足",
		})
	}
	seedRequest(t, st, RequestLog{
		ID: "req_er_x", Status: StatusError, Protocol: "chat.completions",
		RequestedModel: "glm-5.3", MappedModel: "glm-5.3", Provider: "workbuddy",
		ErrorKind: "rate_limit", ErrorCode: "429",
	})

	stats, err := st.OverviewStats(t.Context(), time.Now().UTC().Add(-time.Hour), 6, 10*time.Minute)
	if err != nil {
		t.Fatalf("overview stats: %v", err)
	}
	if len(stats.Errors) != 2 {
		t.Fatalf("error kinds=%d, want 2: %+v", len(stats.Errors), stats.Errors)
	}
	// Sorted by count desc, so quota (3) leads.
	if stats.Errors[0].Kind != "quota" || stats.Errors[0].Count != 3 {
		t.Errorf("first error row = %+v, want quota x3", stats.Errors[0])
	}
	if got := stats.Errors[0].Share; got < 74.9 || got > 75.1 {
		t.Errorf("quota share=%.2f, want 75", got)
	}
	if stats.Errors[1].Kind != "rate_limit" {
		t.Errorf("second error row = %+v, want rate_limit", stats.Errors[1])
	}
}

// A failure with no classification must be reported as "unknown" rather than
// dropped, or the error card would under-report the failure total.
func TestOverviewStatsUnclassifiedErrorIsReported(t *testing.T) {
	st := openTestStore(t)

	seedRequest(t, st, RequestLog{
		ID: "req_unclassified", Status: StatusError, Protocol: "chat.completions",
		RequestedModel: "glm-5.3", MappedModel: "glm-5.3", Provider: "workbuddy",
	})

	stats, err := st.OverviewStats(t.Context(), time.Now().UTC().Add(-time.Hour), 6, 10*time.Minute)
	if err != nil {
		t.Fatalf("overview stats: %v", err)
	}
	if len(stats.Errors) != 1 || stats.Errors[0].Kind != "unknown" {
		t.Errorf("an unclassified failure must appear as 'unknown': %+v", stats.Errors)
	}
}

// The model ranking must merge per-provider rows and rank by volume.
func TestOverviewStatsModelRanking(t *testing.T) {
	st := openTestStore(t)

	// glm-5.3 served by two providers -> must appear once, ranked first.
	seedRequest(t, st, RequestLog{
		ID: "req_m1", Status: StatusOK, Protocol: "chat.completions",
		RequestedModel: "glm-5.3", MappedModel: "glm-5.3", Provider: "workbuddy",
		LatencyMs: intp(100),
	})
	seedRequest(t, st, RequestLog{
		ID: "req_m2", Status: StatusOK, Protocol: "chat.completions",
		RequestedModel: "glm-5.3", MappedModel: "glm-5.3", Provider: "qoder",
		LatencyMs: intp(300),
	})
	seedRequest(t, st, RequestLog{
		ID: "req_m3", Status: StatusOK, Protocol: "chat.completions",
		RequestedModel: "kimi-k3", MappedModel: "kimi-k3", Provider: "workbuddy",
		LatencyMs: intp(500),
	})

	stats, err := st.OverviewStats(t.Context(), time.Now().UTC().Add(-time.Hour), 6, 10*time.Minute)
	if err != nil {
		t.Fatalf("overview stats: %v", err)
	}
	if len(stats.Models) != 2 {
		t.Fatalf("models=%d, want 2 (per-provider rows merged): %+v", len(stats.Models), stats.Models)
	}
	top := stats.Models[0]
	if top.Model != "glm-5.3" {
		t.Errorf("top model=%q, want glm-5.3 (2 requests vs 1)", top.Model)
	}
	if top.Requests != 2 {
		t.Errorf("top model requests=%d, want 2", top.Requests)
	}
	// Dominant provider breaks the 1-1 tie; both have 1, so it is alphabetical.
	if top.Provider == "" {
		t.Errorf("top model has no provider attributed: %+v", top)
	}
}

// A request that never reached the upstream must still be attributed to the
// model the client ASKED for, since mapped_model stays empty.
func TestOverviewStatsUsesRequestedModelWhenUnmapped(t *testing.T) {
	st := openTestStore(t)

	seedRequest(t, st, RequestLog{
		ID: "req_nomap", Status: StatusError, Protocol: "chat.completions",
		RequestedModel: "glm-5.3", MappedModel: "", Provider: "workbuddy",
		ErrorKind: "unavailable",
	})

	stats, err := st.OverviewStats(t.Context(), time.Now().UTC().Add(-time.Hour), 6, 10*time.Minute)
	if err != nil {
		t.Fatalf("overview stats: %v", err)
	}
	if len(stats.Models) != 1 || stats.Models[0].Model != "glm-5.3" {
		t.Errorf("an unmapped request must fall back to requested_model: %+v", stats.Models)
	}
}
