package executor

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
)

// 捕获每次选号后的请求，验证真正发送给渠道的内容，而非只验证数据库开关。
type promptCaptureProvider struct {
	*fakeProvider
	requests   []*provider.ChatRequest
	accountIDs []string
	failFirst  bool
}

func (p *promptCaptureProvider) ChatNonStream(_ context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.ChatResult, error) {
	p.requests = append(p.requests, req)
	p.accountIDs = append(p.accountIDs, cred.AccountID)
	if p.failFirst && len(p.requests) == 1 {
		return nil, &provider.Failure{Status: 503, KindHint: pool.KindUnavailable, Message: "模拟第一次调用失败"}
	}
	return &provider.ChatResult{Model: "test-model", Message: provider.Message{Role: "assistant", Content: json.RawMessage(`"ok"`)}}, nil
}

func (p *promptCaptureProvider) ChatStream(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.Stream, error) {
	_, err := p.ChatNonStream(ctx, cred, req)
	if err != nil {
		return nil, err
	}
	return &provider.Stream{Model: "test-model", Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
}

func TestAccountSystemPromptAppliedPerAttempt(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, preserve := range []bool{false, true} {
			t.Run(itoaBool(stream)+"/"+itoaBool(preserve), func(t *testing.T) {
				h := newHarness(t, "a", "b")
				a, _ := h.pool.ByID("a")
				a.DropSystemPrompt = true
				h.pool.Upsert(a)
				b, _ := h.pool.ByID("b")
				b.DropSystemPrompt = false
				h.pool.Upsert(b)
				capture := &promptCaptureProvider{fakeProvider: h.provider, failFirst: true}
				h.registry.Register(capture)
				chat := &provider.ChatRequest{Model: "test-model", Messages: []provider.Message{
					{Role: "system", Content: json.RawMessage(`"系统提示"`)},
					{Role: "developer", Content: json.RawMessage(`"开发者提示"`)},
					{Role: "user", Content: json.RawMessage(`"原始问题"`)},
					{Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"call_1"}]`), NativeContent: json.RawMessage(`[{"type":"thinking","signature":"keep"}]`)},
					{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"工具结果"`)},
				}, Raw: json.RawMessage(`{"messages":[],"system":"旧提示","instructions":"旧提示","input":[],"response_format":{"type":"json_object"},"max_tokens":32}`), NativeInput: json.RawMessage(`[{"role":"system","content":"旧提示"},{"role":"developer","content":"旧提示"},{"type":"reasoning","encrypted_content":"keep"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]`), Tools: json.RawMessage(`[{"type":"function"}]`)}
				before, _ := json.Marshal(chat)
				req := Request{Chat: chat, PreferAccount: "a", PreserveSystemPrompt: preserve}
				var result *Result
				var err error
				if stream {
					result, err = h.executor.ChatStream(context.Background(), req)
				} else {
					result, err = h.executor.ChatNonStream(context.Background(), req)
				}
				if err != nil {
					t.Fatal(err)
				}
				if result.Stream != nil {
					defer result.Stream.Close()
				}
				if !reflect.DeepEqual(capture.accountIDs, []string{"a", "b"}) {
					t.Fatal("故障换号没有发生")
				}
				first, second := capture.requests[0], capture.requests[1]
				if !preserve {
					if len(first.Messages) != 3 || first.Messages[0].Role != "user" || first.Messages[2].ToolCallID != "call_1" {
						t.Fatal("没有清理上层指令或破坏了工具轮次")
					}
					if string(first.Messages[1].NativeContent) != string(chat.Messages[3].NativeContent) || string(first.Tools) != string(chat.Tools) {
						t.Fatal("签名或工具定义丢失")
					}
					var raw map[string]json.RawMessage
					_ = json.Unmarshal(first.Raw, &raw)
					if len(raw) != 2 || raw["response_format"] == nil || raw["max_tokens"] == nil {
						t.Fatal("提示泄露或输出选项丢失")
					}
					var input []map[string]any
					_ = json.Unmarshal(first.NativeInput, &input)
					if len(input) != 2 || input[0]["encrypted_content"] != "keep" || input[1]["call_id"] != "call_1" {
						t.Fatal("原生输入提示泄露或加密/工具项丢失")
					}
				} else if len(first.Messages) != 5 {
					t.Fatal("网关注入的预设被清理")
				}
				if len(second.Messages) != 5 {
					t.Fatal("第一次清理污染了未开启清理的第二个账号")
				}
				after, _ := json.Marshal(chat)
				if string(before) != string(after) {
					t.Fatal("请求原件被修改")
				}
			})
		}
	}
}

func itoaBool(value bool) string {
	if value {
		return "开启"
	}
	return "关闭"
}
