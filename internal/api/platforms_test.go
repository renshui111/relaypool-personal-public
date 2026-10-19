package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/armor"
	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/compatible"
	"github.com/relaypool/relaypool/internal/provider/native"
	"github.com/relaypool/relaypool/internal/store"
)

func createPlatformAccount(t *testing.T, s *Server, family string, credential map[string]any) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": "测试渠道", "provider": family, "region": "global", "credential": string(mustJSON(credential)), "drop_system_prompt": false, "quota_scope": "account"})
	result := callComplete(t, s, "POST", "/admin/accounts", string(body))
	if result.Code != 201 {
		t.Fatal(result.Body.String())
	}
	var out map[string]any
	json.Unmarshal(result.Body.Bytes(), &out)
	return out["id"].(string)
}
func mustJSON(value any) []byte { encoded, _ := json.Marshal(value); return encoded }

func TestPlatformsRegisteredAndProtected(t *testing.T) {
	s := newTestServerNoAdapter(t)
	response := callComplete(t, s, "GET", "/admin/providers", "")
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &out) != nil || len(out.Data) != 9+len(compatible.Profiles()) {
		t.Fatal("平台能力清单未齐全")
	}
	unauthorized := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauthorized, httptest.NewRequest("GET", "/admin/providers", nil))
	if unauthorized.Code != 401 {
		t.Fatal("渠道管理缺少鉴权")
	}
}

func TestPlatformsConfiguredModelsAvailableImmediately(t *testing.T) {
	s := newTestServerNoAdapter(t)
	id := createPlatformAccount(t, s, "claude", map[string]any{"api_key": "synthetic", "base_url": "http://127.0.0.1:9/v1", "models": []string{"user-model"}})
	createPlatformAccount(t, s, "gemini", map[string]any{"api_key": "synthetic", "models": []string{"user-model"}})
	response := callComplete(t, s, "GET", "/v1/models", "")
	if !strings.Contains(response.Body.String(), `"claude/user-model"`) || !strings.Contains(response.Body.String(), `"gemini/user-model"`) {
		t.Fatal("建号后必须立即展示各平台的配置型号")
	}
	updated := callComplete(t, s, "PUT", "/admin/accounts/"+id+"/credential", `{"payload":{"api_key":"synthetic-new","models":["replacement-model"]}}`)
	if updated.Code != 200 {
		t.Fatal(updated.Body.String())
	}
	if !strings.Contains(callComplete(t, s, "GET", "/v1/models", "").Body.String(), `"claude/replacement-model"`) {
		t.Fatal("替换凭证后的型号配置未生效")
	}
}
func TestPlatformsHTTPRoutingAcrossClientProtocols(t *testing.T) {
	for _, family := range []string{"claude", "gemini", "openai", "codex", "deepseek"} {
		t.Run(family, func(t *testing.T) {
			s := newTestServerNoAdapter(t)
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if family != "gemini" && strings.Contains(body["model"].(string), "/") {
					t.Error("上游模型仍带渠道前缀")
				}
				if family == "codex" {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"response-test\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"平台成功\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch family {
				case "claude":
					io.WriteString(w, `{"type":"message","id":"msg-test","content":[{"type":"text","text":"平台成功"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
				case "gemini":
					io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"平台成功"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`)
				case "openai":
					io.WriteString(w, `{"id":"resp-test","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"平台成功"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
				default:
					io.WriteString(w, `{"id":"chat-test","choices":[{"message":{"role":"assistant","content":"平台成功"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
				}
			}))
			defer fake.Close()
			credential := map[string]any{"base_url": fake.URL, "api_key": "synthetic-key", "models": []string{"model-test"}}
			if family == "codex" {
				delete(credential, "api_key")
				credential["access_token"] = "synthetic-access"
				credential["account_id"] = "synthetic-account"
			}
			createPlatformAccount(t, s, family, credential)
			paths := []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"}
			for _, endpoint := range paths {
				request := map[string]any{"model": family + "/model-test", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
				if endpoint == "/v1/responses" {
					delete(request, "messages")
					request["input"] = "hello"
				}
				response := callComplete(t, s, "POST", endpoint, string(mustJSON(request)))
				if response.Code != 200 || !strings.Contains(response.Body.String(), "平台成功") {
					t.Fatalf("%s %s 转换失败：%d %s", family, endpoint, response.Code, response.Body.String())
				}
			}
		})
	}
}
func TestPlatformsNativeClaudeBlocksReachUpstream(t *testing.T) {
	s := newTestServerNoAdapter(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		encoded := string(mustJSON(body))
		if !strings.Contains(encoded, `"image"`) || !strings.Contains(encoded, `"tool_use_id":"one"`) || !strings.Contains(encoded, `"tool_use_id":"two"`) || !strings.Contains(encoded, `"tool_choice":{"type":"any"}`) {
			t.Errorf("原生块或选择丢失：%s", encoded)
		}
		io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"done"},{"type":"thinking","thinking":"upstream-output","signature":"opaque"}],"stop_reason":"end_turn"}`)
	}))
	defer fake.Close()
	createPlatformAccount(t, s, "claude", map[string]any{"api_key": "synthetic", "base_url": fake.URL})
	response := callComplete(t, s, "POST", "/v1/messages", `{"model":"claude/model-test","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}},{"type":"tool_result","tool_use_id":"one","content":"1"},{"type":"tool_result","tool_use_id":"two","content":"2"}]}],"tool_choice":{"type":"any"}}`)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"signature":"opaque"`) {
		t.Fatal("原生 Claude 回传丢失签名")
	}
}
func TestPlatformsResponsesToolResultsStrictAndPromptReplacement(t *testing.T) {
	s := newTestServerNoAdapter(t)
	s.armor.Register(armor.Preset{Name: "test-replacement", Body: "new-system", Family: "gpt"})
	s.armor.SetModelBindings(map[string]string{"gpt-test": "test-replacement"})
	setTestKeyArmor(t, s, true)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), `"strict":true`) || !strings.Contains(string(raw), `"output":"real-tool-result"`) || !strings.Contains(string(raw), `"encrypted_content":"opaque-replay"`) || strings.Contains(string(raw), "old-system") || strings.Contains(string(raw), "old-developer") || !strings.Contains(string(raw), "new-system") {
			t.Errorf("Responses 回放、严格模式或提示替换错误：%s", raw)
		}
		io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
	}))
	defer fake.Close()
	createPlatformAccount(t, s, "openai", map[string]any{"api_key": "synthetic", "base_url": fake.URL})
	response := callComplete(t, s, "POST", "/v1/responses", `{"model":"openai/gpt-test","input":[{"role":"system","content":"old-system"},{"role":"developer","content":"old-developer"},{"type":"reasoning","encrypted_content":"opaque-replay"},{"type":"function_call_output","call_id":"tool-one","output":"real-tool-result"}],"tools":[{"type":"function","name":"weather","parameters":{"type":"object"},"strict":true}]}`)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
}
func TestPlatformsCredentialRefreshRespectsManualReplacement(t *testing.T) {
	s := newTestServerNoAdapter(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, `{"access_token":"stale-refresh","refresh_token":"rotated","expires_in":3600}`)
	}))
	defer tokenServer.Close()
	s.registry.Register(native.NewClaude(native.Config{TokenURL: tokenServer.URL}))
	id := createPlatformAccount(t, s, "claude", map[string]any{"access_token": "old", "refresh_token": "refresh", "expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339)})
	source := credentialSource{store: s.store, registry: s.registry}
	finished := make(chan provider.Credential, 1)
	failures := make(chan error, 1)
	go func() { cred, err := source.CredentialForContext(t.Context(), id); finished <- cred; failures <- err }()
	<-entered
	if err := s.store.UpsertCredential(t.Context(), store.Credential{AccountID: id, Format: "test", Payload: []byte(`{"access_token":"manual-new"}`)}); err != nil {
		t.Fatal(err)
	}
	close(release)
	cred := <-finished
	if err := <-failures; err != nil || cred.Field("access_token") != "manual-new" {
		t.Fatal("请求使用了过时刷新结果")
	}
	record, _ := s.store.GetCredential(t.Context(), id)
	if strings.Contains(string(record.Payload), "stale-refresh") {
		t.Fatal("过时刷新落盘")
	}
}
