package api

import (
	"context"
	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

type overviewStreamProvider struct {
	*stubProvider
	frames string
}

func (p *overviewStreamProvider) ChatStream(_ context.Context, _ provider.Credential, _ *provider.ChatRequest) (*provider.Stream, error) {
	return &provider.Stream{Body: io.NopCloser(strings.NewReader(p.frames)), Model: "fixture"}, nil
}

// 经过真实 HTTP 流式入口核验落库用量、首字耗时和最终结果。
func TestOverviewLogsActualStreamMetadata(t *testing.T) {
	cases := []struct {
		name, frames string
		reported     bool
		status       string
		total        int
	}{
		{"正常用量", `data: {"model":"fixture-native","choices":[{"delta":{"content":"完成"}}]}` + "\n\n" + `data: {"usage":{"prompt_tokens":30,"completion_tokens":10,"total_tokens":40},"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n", true, store.StatusOK, 40},
		{"明确零用量", `data: {"usage":{"total_tokens":0},"choices":[{"delta":{"content":"完成"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n", true, store.StatusOK, 0},
		{"未报告用量", `data: {"usage":{},"choices":[{"delta":{"content":"完成"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n", false, store.StatusOK, 0},
		{"后续空帧不抹掉计数", `data: {"usage":{"total_tokens":5},"choices":[{"delta":{"content":"完成"}}]}` + "\n\n" + `data: {"usage":{},"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n", true, store.StatusOK, 5},
		{"中途断流", `data: {"choices":[{"delta":{"content":"不完整"}}]}` + "\n\n", false, store.StatusError, 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			adapter := &overviewStreamProvider{stubProvider: newStubProvider(), frames: tt.frames}
			server := newTestServerNoAdapter(t)
			server.registry.Register(adapter)
			seedCredential(t, server.store, "acc_test", adapter.family)
			server.pool.Upsert(poolItem("acc_test", adapter.family))
			h := httptest.NewServer(server.Handler())
			t.Cleanup(h.Close)
			resp := workspacePost(t, h.URL, "/admin/accounts/acc_test/chat/completions", `{"model":"fixture","messages":[{"role":"user","content":"测试"}],"stream":true}`)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			rows, err := server.store.ListRequests(t.Context(), store.RequestLogFilter{Limit: 5})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("缺少流式调用记录：%d", len(rows))
			}
			row := rows[0]
			if row.Status != tt.status {
				t.Fatalf("最终状态错误：%s", row.Status)
			}
			if (row.TotalTokens != nil) != tt.reported || (tt.reported && *row.TotalTokens != tt.total) {
				t.Fatal("真实用量没有准确落库")
			}
			if row.TTFBMs == nil {
				t.Fatal("首段等待没有记录")
			}
		})
	}
}
