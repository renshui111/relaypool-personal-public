package compatible

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

// 各平台真实存在的协议差异分别验证，所有密钥都是合成值。
func TestOfficialProfilesAuthToolsAndThinking(t *testing.T) {
	for _, p := range Profiles() {
		t.Run(p.ID, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat/completions" {
					t.Error("请求路径错误")
				}
				if p.ID == "mimo" {
					if r.Header.Get("api-key") != "synthetic" || r.Header.Get("Authorization") != "" {
						t.Error("MiMo 鉴权错误")
					}
				} else if r.Header.Get("Authorization") != "Bearer synthetic" {
					t.Error("鉴权错误")
				}
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if body["messages"].([]any)[0].(map[string]any)["reasoning_content"] != " prior reasoning " {
					t.Error("历史思考内容被改写")
				}
				if supportsThinking(p.ID) && body["thinking"].(map[string]any)["type"] != "disabled" {
					t.Error("思考关闭未转换")
				}
				if p.ID == "qwen" && body["enable_thinking"] != false {
					t.Error("百炼思考开关未转换")
				}
				if body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["strict"] != true {
					t.Error("严格工具定义丢失")
				}
				if body["stream"] == true {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"完成\",\"tool_calls\":[{\"index\":0,\"id\":\"call\",\"type\":\"function\",\"function\":{\"name\":\"echo\",\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n\n")
					return
				}
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"完成","reasoning_content":" current reasoning ","tool_calls":[{"id":"call","type":"function","function":{"name":"echo","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
			}))
			defer up.Close()
			c := NewFor(p.ID, p.DefaultBaseURL, Config{Timeout: time.Second})
			cred, err := c.DecodeCredential([]byte(`{"api_key":"synthetic","base_url":"` + up.URL + `","models":["model-test"]}`))
			if err != nil {
				t.Fatal(err)
			}
			req := &provider.ChatRequest{Model: "model-test", ReasoningEffort: "none", Raw: json.RawMessage(`{"thinking":{"type":"enabled"}}`), Messages: []provider.Message{{Role: "assistant", Content: json.RawMessage(`""`), ReasoningContent: " prior reasoning "}}, Tools: json.RawMessage(`[{"type":"function","name":"echo","strict":true,"parameters":{"type":"object"}}]`)}
			result, err := c.ChatNonStream(t.Context(), cred, req)
			if err != nil || result.Usage.TotalTokens != 5 || result.Message.ReasoningContent != " current reasoning " || len(result.Message.ToolCalls) == 0 {
				t.Fatal("非流式转换失败", err)
			}
			stream, err := c.ChatStream(t.Context(), cred, req)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(stream.Body)
			stream.Close()
			if err != nil || !strings.Contains(string(raw), "[DONE]") || !strings.Contains(string(raw), "tool_calls") {
				t.Fatal("流式转换失败", err)
			}
		})
	}
}

func TestQwenCatalogUsesSameHostAndPagination(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v1/models" || r.Header.Get("Authorization") != "Bearer synthetic" || len(r.URL.Query()["capabilities"]) != 2 {
			t.Error("百炼目录路径、过滤或鉴权错误")
		}
		if r.URL.Query().Get("page_no") == "1" {
			io.WriteString(w, `{"success":true,"output":{"total":101,"models":[{"model":"qwen-test","name":"测试模型","features":["function-calling"]}]}}`)
		} else {
			io.WriteString(w, `{"success":true,"output":{"total":101,"models":[{"model":"qwen-next","features":[]}]}}`)
		}
	}))
	defer up.Close()
	c := NewFor("qwen", "", Config{Timeout: time.Second})
	cred, err := c.DecodeCredential([]byte(`{"api_key":"synthetic","base_url":"` + up.URL + `/compatible-mode/v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	models, err := c.FetchCatalog(t.Context(), cred)
	if err != nil || len(models) != 2 || calls != 2 || !models[0].Tools || models[1].Tools {
		t.Fatal("目录分页或能力错误", err)
	}
	if !strings.HasSuffix(cred.Field("base_url"), "/compatible-mode/v1") {
		t.Fatal("目录查询污染了推理地址")
	}
}

func TestOfficialProfileRejectsIncompleteCredential(t *testing.T) {
	for _, p := range Profiles() {
		c := NewFor(p.ID, p.DefaultBaseURL, Config{})
		_, err := c.DecodeCredential([]byte(`{"api_key":"synthetic"}`))
		if p.ModelsRequired != (err != nil) {
			t.Fatalf("%s 模型配置约束错误：%v", p.ID, err)
		}
	}
	c := NewFor("qwen", "", Config{})
	if _, err := c.DecodeCredential([]byte(`{"api_key":"synthetic","base_url":"https://{workspace_id}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1"}`)); err == nil {
		t.Fatal("未拒绝占位地址")
	}
}

func TestUnsupportedBuiltinToolsFailWithoutUpstreamRequest(t *testing.T) {
	for _, tool := range []string{`[{"type":"web_search"}]`, `[{"type":"code_interpreter"}]`, `[{"type":"function","function":{}}]`} {
		_, err := chatBody(&provider.ChatRequest{Tools: json.RawMessage(tool)}, false)
		f, ok := provider.AsFailure(err)
		if !ok || f.Status != 400 {
			t.Fatal("未明确拒绝无法转换的工具")
		}
	}
}

func TestCompatibleRejectsUpstreamStateAndBackgroundMode(t *testing.T) {
	for _, raw := range []string{`{"previous_response_id":"old-response"}`, `{"conversation":{"id":"old-conversation"}}`, `{"background":true}`} {
		_, err := chatBody(&provider.ChatRequest{Raw: json.RawMessage(raw)}, false)
		f, ok := provider.AsFailure(err)
		if !ok || f.Status != 400 {
			t.Fatal("上游会话状态或异步任务被错误接受")
		}
	}
}

func TestExplicitThinkingSwitchKeepsProviderSpecificOptions(t *testing.T) {
	for _, family := range []string{"glm", "kimi", "doubao", "mimo"} {
		raw, err := chatBodyFor(&provider.ChatRequest{Model: "model-test", ReasoningEffort: "medium", Raw: json.RawMessage(`{"enable_thinking":true,"thinking":{"clear_thinking":false,"keep":"all"}}`)}, false, family)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		json.Unmarshal(raw, &body)
		thinking := body["thinking"].(map[string]any)
		if thinking["type"] != "enabled" || thinking["keep"] != "all" || thinking["clear_thinking"] != false || body["reasoning_effort"] != nil || body["enable_thinking"] != nil {
			t.Fatal("思考选项错误：", string(raw))
		}
	}
	for _, pair := range [][2]string{{"kimi", "kimi-k3"}, {"kimi", "kimi-k2.7-code"}, {"glm", "glm-5.3"}} {
		_, err := chatBodyFor(&provider.ChatRequest{Model: pair[1], ReasoningEffort: "none"}, false, pair[0])
		f, ok := provider.AsFailure(err)
		if !ok || f.Status != 400 {
			t.Fatal("始终思考模型错误接受关闭指令")
		}
	}
}
