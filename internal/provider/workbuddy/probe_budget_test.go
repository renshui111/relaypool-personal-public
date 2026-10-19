package workbuddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/provider"
)

// 短消息测试只有推理内容时不得扩大预算；原聊天的补发行为继续保留。
func TestProbeKeepsStrictBudgetOnReasoningOnly(t *testing.T) {
	for _, strict := range []bool{true, false} {
		name := "普通聊天保留补发"
		if strict {
			name = "账号测试不扩大预算"
		}
		t.Run(name, func(t *testing.T) {
			c := New(Config{})
			calls := 0
			c.http.Transport = checkinBusinessTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if calls == 1 && body["max_tokens"] != float64(128) {
					t.Fatal("初始预算不符")
				}
				text := `{"choices":[{"delta":{"reasoning_content":"测试推理"},"finish_reason":"length"}]}`
				if calls > 1 {
					text = `{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + text + "\n\ndata: [DONE]\n\n"))}, nil
			})
			c.streamer.Transport = c.http.Transport
			limit := 128
			cred := provider.Credential{Raw: []byte(`{"access_token":"synthetic-token","uid":"synthetic-user","domain":"copilot.tencent.com"}`)}
			result, err := c.ChatNonStream(context.Background(), cred, &provider.ChatRequest{Model: "auto", Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"请只回复 OK"`)}}, MaxTokens: &limit, StrictTokenLimit: strict})
			if strict && (err == nil || calls != 1) {
				t.Fatalf("检查扩大了预算或误判空回答：调用=%d，错误=%v", calls, err)
			}
			if !strict && (err != nil || calls != 2 || result.Message.ContentText() != "OK") {
				t.Fatalf("普通聊天补发被改变：调用=%d，错误=%v", calls, err)
			}
		})
	}
}
