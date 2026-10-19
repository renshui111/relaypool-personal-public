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
)

func TestAPICredentialDefaultsMetadataAndReplacement(t *testing.T) {
	s := newTestServerNoAdapter(t)
	created := callComplete(t, s, "POST", "/admin/accounts", `{"name":"密钥账号","provider":"mimo","credential":"{\"api_key\":\"synthetic\",\"models\":[\"mimo-test\"]}"}`)
	if created.Code != 201 {
		t.Fatal(created.Body.String())
	}
	var account map[string]any
	json.Unmarshal(created.Body.Bytes(), &account)
	if account["drop_system_prompt"] != false || account["auth_type"] != "api_key" || account["has_credential"] != true {
		t.Fatal("创建账号的默认配置错误")
	}
	id := account["id"].(string)
	stored, err := s.store.GetCredential(t.Context(), id)
	if err != nil || stored.Format != "mimo-api_key-v1" {
		t.Fatal("凭证类型元数据错误", err)
	}
	listed := callComplete(t, s, "GET", "/admin/accounts", "")
	if !strings.Contains(listed.Body.String(), `"has_credential":true`) || strings.Contains(listed.Body.String(), "synthetic") {
		t.Fatal("列表元数据缺失或泄露密钥")
	}
	bad := callComplete(t, s, "POST", "/admin/accounts", `{"provider":"mimo","auth_type":"oauth","credential":"{\"api_key\":\"synthetic\",\"models\":[\"mimo-test\"]}"}`)
	if bad.Code != 400 {
		t.Fatal("接受了与凭证不一致的类型")
	}
	id = createPlatformAccount(t, s, "claude", map[string]any{"access_token": "synthetic-login", "expires_at": "2030-01-01T00:00:00Z"})
	stored, err = s.store.GetCredential(t.Context(), id)
	if err != nil || stored.ExpiresAt.Year() != 2030 {
		t.Fatal("过期时间未保存", err)
	}
	updated := callComplete(t, s, "PUT", "/admin/accounts/"+id+"/credential", `{"payload":{"api_key":"synthetic-api"}}`)
	if updated.Code != 200 {
		t.Fatal(updated.Body.String())
	}
	detail := callComplete(t, s, "GET", "/admin/accounts/"+id, "")
	json.Unmarshal(detail.Body.Bytes(), &account)
	if account["auth_type"] != "api_key" || account["has_credential"] != true {
		t.Fatal("替换后账号类型不一致")
	}
	if _, exists := account["ready"]; exists {
		t.Fatal("导入凭证被错误标为已真实验证")
	}
	blank := callComplete(t, s, "POST", "/admin/accounts", `{"provider":"workbuddy"}`)
	json.Unmarshal(blank.Body.Bytes(), &account)
	if account["has_credential"] != false || account["drop_system_prompt"] != true {
		t.Fatal("空账号或原 WorkBuddy 默认行为错误")
	}
}

func TestArmorReplacesDeveloperWithoutDiscardingConversation(t *testing.T) {
	r := &provider.ChatRequest{Messages: []provider.Message{{Role: "developer", Content: json.RawMessage(`"old-developer"`)}, {Role: "system", Content: json.RawMessage(`"old-system"`)}, {Role: "user", Content: json.RawMessage(`"问题"`)}, {Role: "tool", ToolCallID: "call", Content: json.RawMessage(`"结果"`)}}}
	if !applyArmor(r, armor.Decision{Applied: true, Preset: armor.Preset{Body: "新指令"}}) || len(r.Messages) != 3 || r.Messages[1].Role != "user" || r.Messages[2].ToolCallID != "call" {
		t.Fatal("破甲覆盖未清理 developer 或丢失工具轮次")
	}
}

func TestExpandedPlatformHTTPProtocolsAndInvalidToolHealth(t *testing.T) {
	for _, family := range []string{"grok", "qwen", "glm", "kimi", "doubao", "hunyuan", "mimo"} {
		t.Run(family, func(t *testing.T) {
			s := newTestServerNoAdapter(t)
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var request map[string]any
				json.NewDecoder(r.Body).Decode(&request)
				if request["model"] != "test-model" || request["messages"].([]any)[0].(map[string]any)["content"] != "保留系统指令" {
					t.Error("模型别名或系统指令错误")
				}
				if request["stream"] == true {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"测试完成\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"测试完成"},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`)
			}))
			defer up.Close()
			id := createPlatformAccount(t, s, family, map[string]any{"api_key": "synthetic", "base_url": up.URL, "models": []string{"test-model"}})
			for _, endpoint := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
				for _, stream := range []bool{false, true} {
					body := map[string]any{"model": family + "/test-model", "stream": stream, "messages": []any{map[string]any{"role": "system", "content": "保留系统指令"}, map[string]any{"role": "user", "content": "问题"}}}
					if endpoint == "/v1/responses" {
						body["input"] = body["messages"]
						delete(body, "messages")
					}
					response := callComplete(t, s, "POST", endpoint, string(mustJSON(body)))
					if response.Code != 200 || !strings.Contains(response.Body.String(), "测试完成") {
						t.Fatalf("协议 %s 流式 %v：%d %s", endpoint, stream, response.Code, response.Body.String())
					}
				}
			}
			before := calls
			view := callComplete(t, s, "GET", "/admin/accounts/"+id, "")
			var status map[string]any
			json.Unmarshal(view.Body.Bytes(), &status)
			if status["ready"] != true || status["status"] != "ready" {
				t.Fatal("真实请求完成后凭证状态未更新")
			}
			bad := callComplete(t, s, "POST", "/v1/responses", `{"model":"`+family+`/test-model","input":"问题","tools":[{"type":"web_search"}]}`)
			if bad.Code != 400 || calls != before {
				t.Fatal("无法转换的内置工具未在请求前拒绝")
			}
			item, _ := s.pool.ByID(id)
			if item.AccountDownUntil().After(time.Now()) || item.InFlight != 0 {
				t.Fatal("请求配置错误误伤了账号健康或泄漏槽位")
			}
		})
	}
}
