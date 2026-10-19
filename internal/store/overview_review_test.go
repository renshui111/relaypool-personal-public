package store

import (
	"testing"
	"time"
)

// 进行中的请求不能伪造成功，摘要、渠道、型号和时段采用同一分类。
func TestOverviewSeparatesStreamingFromCompleted(t *testing.T) {
	st := openTestStore(t)
	for _, status := range []string{StatusOK, StatusError, StatusStreaming} {
		seedRequest(t, st, RequestLog{ID: "review_" + status, Status: status, Protocol: "chat.completions", RequestedModel: "fixture", Provider: "fixture"})
	}
	result, err := st.OverviewStats(t.Context(), time.Now().UTC().Add(-time.Hour), 12, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	s := result.Summary
	if s.Requests != 3 || s.Successes != 1 || s.Failures != 1 || s.InFlight != 1 || s.SuccessPct != 50 {
		t.Fatalf("请求分类不正确：%+v", s)
	}
	if result.Provider[0].SuccessPct != 50 || result.Provider[0].InFlight != 1 || result.Models[0].SuccessPct != 50 || result.Models[0].InFlight != 1 {
		t.Fatal("渠道或型号仍把进行中当作成功")
	}
	var successes, failures, inflight int64
	for _, b := range result.Series {
		successes += b.Successes
		failures += b.Failures
		inflight += b.InFlight
	}
	if successes != 1 || failures != 1 || inflight != 1 {
		t.Fatal("图表分类与摘要不一致")
	}
}

// 滚动窗口最早的请求仍须出现在图表中，窗口外的请求不能混入。
func TestOverviewChartCoversEntireRollingWindow(t *testing.T) {
	st := openTestStore(t)
	since := time.Now().UTC().Add(-24 * time.Hour)
	for _, id := range []string{"oldest", "outside", "current"} {
		seedRequest(t, st, RequestLog{ID: id, Status: StatusOK, Protocol: "chat.completions", RequestedModel: "fixture", TotalTokens: intp(11)})
	}
	for id, at := range map[string]time.Time{"oldest": since.Add(time.Second), "outside": since.Add(-time.Second)} {
		if _, err := st.db.ExecContext(t.Context(), "UPDATE request_logs SET created_at = ? WHERE id = ?", formatTime(at), id); err != nil {
			t.Fatal(err)
		}
	}
	result, err := st.OverviewStats(t.Context(), since, 24, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var requests, tokens int64
	for _, b := range result.Series {
		requests += b.Requests
		tokens += b.TotalTokens
	}
	if requests != 2 || requests != result.Summary.Requests || tokens != result.Summary.TotalTokens || result.Series[0].Requests != 1 {
		t.Fatal("图表漏掉窗口首段或混入范围外记录")
	}
}

// 不同渠道的同型号延迟按已上报样本合并，进行中不当作零耗时。
func TestOverviewMergedLatencyUsesActualSamples(t *testing.T) {
	st := openTestStore(t)
	seedRequest(t, st, RequestLog{ID: "latency_a", Status: StatusOK, Protocol: "chat.completions", RequestedModel: "fixture", Provider: "a", LatencyMs: intp(100), TTFBMs: intp(25), TotalTokens: intp(0)})
	seedRequest(t, st, RequestLog{ID: "latency_b", Status: StatusOK, Protocol: "chat.completions", RequestedModel: "fixture", Provider: "b", LatencyMs: intp(900)})
	for _, id := range []string{"running_a", "running_b", "running_c"} {
		seedRequest(t, st, RequestLog{ID: id, Status: StatusStreaming, Protocol: "chat.completions", RequestedModel: "fixture", Provider: "a"})
	}
	result, err := st.OverviewStats(t.Context(), time.Now().UTC().Add(-time.Hour), 12, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if result.Models[0].AvgLatencyMs != 500 || result.Models[0].LatencySamples != 2 {
		t.Fatal("型号延迟错误地按全部请求加权")
	}
	if result.Summary.TTFBSamples != 1 || result.Summary.LatencySamples != 2 || result.Summary.TokensReported != 1 {
		t.Fatal("真实零用量与未知用量未区分")
	}
	var tokenSamples, latencySamples int64
	for _, b := range result.Series {
		tokenSamples += b.TokensReported
		latencySamples += b.LatencySamples
	}
	if tokenSamples != 1 || latencySamples != 2 {
		t.Fatal("图表采样数量错误")
	}
}
