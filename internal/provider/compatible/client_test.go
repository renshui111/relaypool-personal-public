package compatible

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

// DeepSeek 工具轮次需要原样回传思考内容，并保留独立的思考开关。
func TestDeepSeekThinkingAndReasoningReplay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["thinking"].(map[string]any)["type"] != "enabled" || body["messages"].([]any)[0].(map[string]any)["reasoning_content"] != " prior reasoning " {
			t.Fatal("DeepSeek 思考配置或回放被清除")
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done","reasoning_content":" current reasoning "},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	client := NewFor("deepseek", "https://api.deepseek.com", Config{Timeout: time.Second})
	cred, err := client.DecodeCredential([]byte(`{"api_key":"synthetic","base_url":"` + server.URL + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ChatNonStream(context.Background(), cred, &provider.ChatRequest{Model: "actual-model", Raw: json.RawMessage(`{"thinking":{"type":"enabled"}}`), Messages: []provider.Message{{Role: "assistant", Content: json.RawMessage(`""`), ReasoningContent: " prior reasoning "}}})
	if err != nil || result.Message.ReasoningContent != " current reasoning " {
		t.Fatal("DeepSeek 思考输出丢失", err)
	}
}

func TestCompleteCompatibleProtocol(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-key" {
			t.Error("鉴权头错误")
		}
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"test-model"}]}`)
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("请求 JSON 无效")
		}
		messages := body["messages"].([]any)
		if messages[0].(map[string]any)["content"] != "系统提示" {
			t.Error("中立消息变更未传给上游")
		}
		if _, ok := body["tools"]; !ok {
			t.Error("工具未传给上游")
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"完成\"}}]}\n\ndata: [DONE]\n\n")
			return
		}
		io.WriteString(w, `{"id":"c1","model":"test-model","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`)
	}))
	defer upstream.Close()
	c := New(Config{Timeout: time.Second, HeaderTimeout: time.Second, IdleTimeout: time.Second})
	cred, err := c.DecodeCredential([]byte(`{"api_key":"synthetic-key","base_url":"` + upstream.URL + `/v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := c.FetchCatalog(t.Context(), cred)
	if err != nil || len(catalog) != 1 || catalog[0].NativeModel != "test-model" {
		t.Fatal("目录读取失败")
	}
	req := &provider.ChatRequest{Model: "test-model", Raw: json.RawMessage(`{"messages":[{"role":"user","content":"原始"}]}`), Messages: []provider.Message{{Role: "system", Content: json.RawMessage(`"系统提示"`)}}, Tools: json.RawMessage(`[{"type":"function","function":{"name":"read"}}]`)}
	result, err := c.ChatNonStream(t.Context(), cred, req)
	if err != nil || result.FinishReason != "tool_calls" || result.Usage.TotalTokens != 7 || len(result.Message.ToolCalls) == 0 {
		t.Fatalf("工具完成响应错误：%v", err)
	}
	stream, err := c.ChatStream(t.Context(), cred, req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	raw, err := io.ReadAll(stream.Body)
	if err != nil || !strings.Contains(string(raw), "[DONE]") {
		t.Fatal("SSE 未完成")
	}
}

func TestCompleteCompatibleCredentialAndSecretProtection(t *testing.T) {
	c := New(Config{Timeout: time.Second})
	for _, bad := range []string{`{}`, `{"api_key":"k","base_url":"file:///x"}`, `{"api_key":"k","base_url":"https://u:p@example.com/v1"}`, `{"api_key":"k","base_url":"https://example.com/?token=secret"}`} {
		if _, err := c.DecodeCredential([]byte(bad)); err == nil {
			t.Fatal("接受了非法凭证地址")
		}
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"type":"insufficient_quota","message":"secret echoed"}}`)
	}))
	defer up.Close()
	cred, _ := c.DecodeCredential([]byte(`{"api_key":"secret","base_url":"` + up.URL + `"}`))
	_, err := c.ChatNonStream(t.Context(), cred, &provider.ChatRequest{Model: "m"})
	failure, ok := provider.AsFailure(err)
	if !ok || strings.Contains(failure.Body, "secret") || !strings.Contains(failure.Body, "insufficient_quota") {
		t.Fatal("错误日志未清理密钥或丢失分类信息")
	}
}

func TestCompleteCompatibleDoesNotFollowRedirect(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer target.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer up.Close()
	c := New(Config{Timeout: time.Second})
	cred, _ := c.DecodeCredential([]byte(`{"api_key":"secret","base_url":"` + up.URL + `"}`))
	if _, err := c.ChatNonStream(t.Context(), cred, &provider.ChatRequest{Model: "m"}); err == nil || called {
		t.Fatal("跟随重定向泄漏凭证")
	}
}

func TestCompatibleResponsesToolChoiceAndStrictSchema(t *testing.T) {
	req := &provider.ChatRequest{Model: "model", Tools: json.RawMessage(`[{"type":"function","name":"read","strict":true,"parameters":{"type":"object"}}]`), ToolChoice: json.RawMessage(`{"type":"function","name":"read"}`)}
	raw, err := chatBody(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.Unmarshal(raw, &body)
	choice := body["tool_choice"].(map[string]any)
	fn := choice["function"].(map[string]any)
	tool := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "read" || tool["strict"] != true {
		t.Fatal("Responses 工具选择或严格模式未转换")
	}
}
