package native

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

func testCredential(t *testing.T, c *Client, input map[string]any) provider.Credential {
	t.Helper()
	cred, err := c.DecodeCredential(marshal(input))
	if err != nil {
		t.Fatal(err)
	}
	cred.AccountID = "synthetic-account"
	return cred
}
func event(value string) string { return "data: " + value + "\n\n" }

func TestNativeGoogleRefreshFormAndConfigurationIsolation(t *testing.T) {
	calls := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("client_id") != "synthetic-client" || r.Form.Get("client_secret") != "synthetic-secret" || r.Form.Get("grant_type") != "refresh_token" {
			t.Error("Google 刷新配置未使用用户提供的客户端")
		}
		io.WriteString(w, `{"access_token":"new-token","refresh_token":"new-refresh","expires_in":3600,"base_url":"https://malicious.invalid","models":["modified-model"]}`)
	}))
	defer server.Close()
	client := NewGemini(Config{TokenURL: server.URL})
	input := map[string]any{"access_token": "old-token", "refresh_token": "old-refresh", "project_id": "user-project", "mode": "code_assist", "models": []string{"actual-model"}}
	missing := testCredential(t, client, input)
	if _, err := client.Refresh(context.Background(), missing); err == nil || calls.Load() != 0 {
		t.Fatal("缺少客户端配置时不应发起刷新")
	}
	input["client_id"] = "synthetic-client"
	input["client_secret"] = "synthetic-secret"
	cred := testCredential(t, client, input)
	updated, err := client.Refresh(context.Background(), cred)
	if err != nil || updated.Field("access_token") != "new-token" || updated.Field("base_url") != cred.Field("base_url") || updated.Field("models") != cred.Field("models") || updated.Field("project_id") != "user-project" {
		t.Fatal("刷新改变了用户配置", err)
	}
}

func TestNativeRefreshDifferentAccountsRunIndependently(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		io.WriteString(w, `{"access_token":"updated","expires_in":3600}`)
	}))
	defer server.Close()
	client := NewClaude(Config{TokenURL: server.URL})
	finished := make(chan error, 2)
	for _, id := range []string{"first", "second"} {
		cred := testCredential(t, client, map[string]any{"access_token": "old", "refresh_token": "refresh"})
		cred.AccountID = id
		go func() { _, err := client.Refresh(context.Background(), cred); finished <- err }()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("不同账号被串行阻塞")
		}
	}
	close(release)
	for range 2 {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
}

var fixtures = map[string]string{
	"claude": event(`{"type":"message_start","message":{"id":"msg-test","usage":{"input_tokens":3,"cache_read_input_tokens":2}}}`) + event(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" a "}}`) + event(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-test","name":"weather","input":{}}}`) + event(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"left "}}`) + event(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"right\"}"}}`) + event(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`) + event(`{"type":"message_stop"}`),
	"gemini": event(`{"responseId":"gem-test","candidates":[{"content":{"parts":[{"text":" a "},{"functionCall":{"id":"call-test","name":"weather","args":{"city":"left right"}},"thoughtSignature":"opaque-signature"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"thoughtsTokenCount":1,"totalTokenCount":9}}`),
	"openai": event(`{"type":"response.created","response":{"id":"resp-test"}}`) + event(`{"type":"response.output_text.delta","delta":" a "}`) + event(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call-test","name":"weather","arguments":""}}`) + event(`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"city\":\"left "}`) + event(`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"right\"}"}`) + event(`{"type":"response.completed","response":{"id":"resp-test","usage":{"input_tokens":5,"output_tokens":4,"total_tokens":9}}}`),
}

func TestNativePlatformHTTPAndStream(t *testing.T) {
	for _, family := range []string{"claude", "gemini", "openai", "codex"} {
		t.Run(family, func(t *testing.T) {
			var streams atomic.Int32
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.RawQuery, "secret") {
					t.Error("密钥进入 URL")
				}
				switch family {
				case "claude":
					if r.Header.Get("x-api-key") != "synthetic-secret" || r.Header.Get("anthropic-version") == "" {
						t.Error("Claude 鉴权错误")
					}
				case "gemini":
					if r.Header.Get("x-goog-api-key") != "synthetic-secret" {
						t.Error("Gemini 鉴权错误")
					}
				case "codex":
					if r.Header.Get("Authorization") != "Bearer synthetic-access" || r.Header.Get("ChatGPT-Account-Id") != "synthetic-account" {
						t.Error("Codex 鉴权错误")
					}
				default:
					if r.Header.Get("Authorization") != "Bearer synthetic-secret" {
						t.Error("OpenAI 鉴权错误")
					}
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				streaming, _ := body["stream"].(bool)
				if family == "gemini" {
					streaming = strings.Contains(r.URL.Path, "streamGenerateContent")
				}
				switch family {
				case "claude":
					if r.URL.Path != "/v1/messages" || len(array(body["system"])) != 1 || len(array(body["tools"])) != 1 {
						t.Error("Claude 请求转换错误")
					}
				case "gemini":
					if len(array(object(body["systemInstruction"])["parts"])) != 1 || len(array(body["tools"])) != 1 {
						t.Error("Gemini 请求转换错误")
					}
				default:
					if r.URL.Path != "/v1/responses" || str(body, "instructions") != "system-test" || len(array(body["input"])) != 1 {
						t.Error("Responses 请求转换错误")
					}
					if family == "codex" && body["store"] != false {
						t.Error("Codex 没有关闭存储")
					}
				}
				if streaming {
					streams.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					fixture := family
					if fixture == "codex" {
						fixture = "openai"
					}
					io.WriteString(w, fixtures[fixture])
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch family {
				case "claude":
					io.WriteString(w, `{"type":"message","id":"msg-test","model":"model-test","content":[{"type":"text","text":" a "},{"type":"tool_use","id":"call-test","name":"weather","input":{"city":"left right"}}],"stop_reason":"tool_use","usage":{"input_tokens":3,"cache_read_input_tokens":2,"output_tokens":4}}`)
				case "gemini":
					io.WriteString(w, `{"responseId":"gem-test","candidates":[{"content":{"parts":[{"text":" a "},{"functionCall":{"id":"call-test","name":"weather","args":{"city":"left right"}},"thoughtSignature":"opaque-signature"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"thoughtsTokenCount":1,"totalTokenCount":9}}`)
				default:
					io.WriteString(w, `{"id":"resp-test","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":" a "}]},{"type":"function_call","call_id":"call-test","name":"weather","arguments":"{\"city\":\"left right\"}"}],"usage":{"input_tokens":5,"output_tokens":4,"total_tokens":9}}`)
				}
			}))
			defer fake.Close()
			c := newClient(family, Config{Timeout: time.Second, IdleTimeout: time.Second})
			input := map[string]any{"base_url": fake.URL + "/v1", "api_key": "synthetic-secret"}
			if family == "codex" {
				delete(input, "api_key")
				input["access_token"] = "synthetic-access"
				input["account_id"] = "synthetic-account"
			}
			cred := testCredential(t, c, input)
			req := &provider.ChatRequest{Model: "model-test", Messages: []provider.Message{{Role: "system", Content: marshal("system-test")}, {Role: "user", Content: marshal("hello")}}, Tools: json.RawMessage(`[{"type":"function","function":{"name":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}},"strict":true}}]`)}
			result, err := c.ChatNonStream(context.Background(), cred, req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Message.ContentText() != " a " || result.FinishReason != "tool_calls" || result.Usage.TotalTokens != 9 {
				t.Fatalf("完成转换丢失文本、工具或用量：%+v", result)
			}
			var tool []call
			if json.Unmarshal(result.Message.ToolCalls, &tool) != nil || len(tool) != 1 || !strings.Contains(tool[0].Function.Arguments, "left right") {
				t.Fatal("工具参数错误")
			}
			stream, err := c.ChatStream(context.Background(), cred, req)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			collected, err := collectStream(stream.Body, req.Model)
			if err != nil {
				t.Fatal(err)
			}
			if collected.Message.ContentText() != " a " || collected.Usage.TotalTokens != 9 || collected.FinishReason != "tool_calls" {
				t.Fatalf("流转换错误：%+v", collected)
			}
			if !strings.Contains(string(collected.Message.ToolCalls), "left right") {
				t.Fatal("流工具参数中的空格丢失")
			}
			if streams.Load() < 1 {
				t.Fatal("没有走真实 HTTP 流")
			}
		})
	}
}
func TestNativeCredentialImportsAndValidation(t *testing.T) {
	c := NewClaude(Config{})
	cred := testCredential(t, c, map[string]any{"claudeAiOauth": map[string]any{"accessToken": "synthetic-access", "refreshToken": "synthetic-refresh", "expiresAt": float64(2000000000000)}})
	if cred.Field("access_token") != "synthetic-access" || cred.ExpiresAt.UnixMilli() != 2000000000000 {
		t.Fatal("Claude 导入格式错误")
	}
	codex := NewCodex(Config{})
	cred = testCredential(t, codex, map[string]any{"tokens": map[string]any{"access_token": "synthetic-access", "refresh_token": "synthetic-refresh", "account_id": "synthetic-account"}})
	if cred.Field("account_id") != "synthetic-account" {
		t.Fatal("Codex auth.json 导入失败")
	}
	for _, input := range []map[string]any{{"api_key": "bad\nkey"}, {"api_key": "key", "base_url": "https://user:password@example.com"}, {"api_key": "key", "base_url": "https://example.com?key=secret"}, {"api_key": "key", "models": []any{1}}, {"api_key": "key", "access_token": "mixed"}} {
		if _, err := c.DecodeCredential(marshal(input)); err == nil {
			t.Fatal("非法凭证被接受")
		}
	}
	if _, err := NewGemini(Config{}).DecodeCredential(marshal(map[string]any{"mode": "code_assist", "access_token": "test"})); err == nil {
		t.Fatal("缺少 Code Assist 项目 ID 未拒绝")
	}
	if _, err := codex.DecodeCredential(marshal(map[string]any{"api_key": "test"})); err == nil {
		t.Fatal("API 密钥冒充 Codex 登录态")
	}
}
func TestNativeTruncatedAndErrorStreams(t *testing.T) {
	for _, family := range []string{"claude", "gemini", "codex"} {
		stream := newTranslatedStream(io.NopCloser(strings.NewReader(event(`{"type":"ping"}`))), family, "model", provider.Credential{})
		if _, err := io.ReadAll(stream); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("%s 断流被误报成功：%v", family, err)
		}
	}
	cred := provider.Credential{Fields: map[string]string{"access_token": "synthetic-secret"}}
	stream := newTranslatedStream(io.NopCloser(strings.NewReader(event(`{"error":{"message":"synthetic-secret"}}`))), "claude", "model", cred)
	_, err := io.ReadAll(stream)
	failure, ok := provider.AsFailure(err)
	if !ok || strings.Contains(failure.Body, "synthetic-secret") {
		t.Fatal("流错误未遮蔽凭证")
	}
}
func TestNativeRefreshDeduplicationAndCancellation(t *testing.T) {
	var count atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
	}))
	defer fake.Close()
	c := NewClaude(Config{TokenURL: fake.URL})
	cred := testCredential(t, c, map[string]any{"access_token": "old-access", "refresh_token": "old-refresh", "models": []string{"original-model"}})
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	wg.Add(1)
	go func() { defer wg.Done(); _, err := c.Refresh(context.Background(), cred); failures <- err }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if _, err := c.Refresh(ctx, cred); !errors.Is(err, context.Canceled) || time.Since(started) > 200*time.Millisecond {
		t.Fatal("刷新等待无法取消")
	}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			updated, err := c.Refresh(context.Background(), cred)
			if err == nil && (updated.Field("access_token") != "new-access" || !strings.Contains(updated.Field("models"), "original-model")) {
				err = errors.New("刷新丢失令牌或模型配置")
			}
			failures <- err
		}()
	}
	close(release)
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count.Load() != 1 {
		t.Fatalf("旋转型令牌被刷新 %d 次", count.Load())
	}
}
func TestNativeCatalogPaginationAndRedirectIsolation(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect/models" {
			http.Redirect(w, r, target.URL, 302)
			return
		}
		if r.URL.Query().Get("after_id") == "" {
			io.WriteString(w, `{"data":[{"id":"first"}],"has_more":true,"last_id":"first"}`)
		} else {
			io.WriteString(w, `{"data":[{"id":"second"}],"has_more":false}`)
		}
	}))
	defer fake.Close()
	c := NewClaude(Config{})
	cred := testCredential(t, c, map[string]any{"api_key": "secret", "base_url": fake.URL})
	models, err := c.FetchCatalog(context.Background(), cred)
	if err != nil || len(models) != 2 {
		t.Fatalf("模型分页失败：%v %+v", err, models)
	}
	cred = testCredential(t, c, map[string]any{"api_key": "secret", "base_url": fake.URL + "/redirect"})
	if _, err := c.FetchCatalog(context.Background(), cred); err == nil || leaked.Load() != 0 {
		t.Fatal("重定向泄露鉴权或误报成功")
	}
}
func TestNativeGoogleToolSignatureAndCodeAssistWrapper(t *testing.T) {
	c := NewGemini(Config{})
	cred := testCredential(t, c, map[string]any{"mode": "code_assist", "access_token": "access", "project_id": "project-test"})
	req := &provider.ChatRequest{Model: "gemini-test", Messages: []provider.Message{{Role: "assistant", Content: marshal(""), ToolCalls: json.RawMessage(`[{"id":"call-test","type":"function","function":{"name":"weather","arguments":"{}"},"extra_content":{"google":{"thought_signature":"signature-test"}}}]`)}, {Role: "tool", ToolCallID: "call-test", Content: marshal("sunny")}}}
	body, endpoint, err := c.chatRequest(cred, req, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(endpoint, "/v1internal:streamGenerateContent?alt=sse") || !strings.Contains(string(body), `"thoughtSignature":"signature-test"`) || !strings.Contains(string(body), `"project":"project-test"`) || !strings.Contains(string(body), `"functionResponse"`) {
		t.Fatal("Code Assist 包装或工具签名丢失")
	}
	req.Model = "../bad"
	if _, _, err := c.chatRequest(cred, req, true); err == nil {
		t.Fatal("模型名路径注入未拒绝")
	}
}
func TestNativeClaudePreservesOriginalContentBlocks(t *testing.T) {
	req := &provider.ChatRequest{Model: "claude-test", Messages: []provider.Message{{Role: "user", Content: marshal("converted"), NativeContent: json.RawMessage(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}},{"type":"tool_result","tool_use_id":"a","content":"first"},{"type":"tool_result","tool_use_id":"b","content":"second"}]`)}}}
	body, err := claudeBody(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	json.Unmarshal(body, &parsed)
	parts := array(object(array(parsed["messages"])[0])["content"])
	if len(parts) != 3 || str(object(parts[0]), "type") != "image" || str(object(parts[2]), "tool_use_id") != "b" {
		t.Fatal("原生图片或多个工具结果丢失")
	}
}
