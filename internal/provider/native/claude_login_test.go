package native

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/browserlogin"
)

func TestClaudeRefreshPreservesApprovedClientScopeAndAccount(t *testing.T) {
	for _, ownClient := range []bool{false, true} {
		clientID, secret := browserlogin.ClaudeClientID, ""
		if ownClient {
			clientID, secret = "own-approved-client", "synthetic-client-secret"
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Header.Get("Content-Type") != "application/json" || body["grant_type"] != "refresh_token" || body["refresh_token"] != "old-refresh" || body["client_id"] != clientID || body["client_secret"] != secret || body["scope"] != "user:profile user:inference" {
				t.Error("刷新未使用原客户端和授权权限")
			}
			io.WriteString(w, `{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":7200,"client_id":"wrong-client","account_id":"wrong-user","base_url":"https://evil.invalid","scope":"user:inference"}`)
		}))
		client := NewClaude(Config{TokenURL: server.URL})
		input := map[string]any{"claudeAiOauth": map[string]any{"accessToken": "old-access", "refreshToken": "old-refresh", "scopes": []string{"user:profile", "user:inference"}}, "models": []string{"actual-model"}, "account_id": "original-user"}
		if ownClient {
			input["client_id"], input["client_secret"] = clientID, secret
		}
		cred := testCredential(t, client, input)
		updated, err := client.Refresh(t.Context(), cred)
		server.Close()
		if err != nil || updated.Field("access_token") != "new-access" || updated.Field("refresh_token") != "rotated-refresh" || updated.Field("account_id") != "original-user" || updated.Field("models") != cred.Field("models") || updated.Field("scope") != cred.Field("scope") || updated.Field("base_url") != cred.Field("base_url") || !updated.ExpiresAt.After(time.Now().Add(time.Hour)) {
			t.Fatal("刷新丢失登录信息或覆盖原账号配置", err)
		}
		if ownClient && (updated.Field("client_id") != clientID || updated.Field("client_secret") != secret) {
			t.Fatal("刷新改换了客户端身份")
		}
	}
}

func TestClaudeOAuthCatalogAndChatUseBearerCredential(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer synthetic-oauth-access" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Error("OAuth 登录态未使用正确的 Claude 鉴权")
		}
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"synthetic-claude-model"}],"has_more":false}`)
			return
		}
		if r.URL.Path != "/v1/messages" {
			t.Error("消息接口路径错误")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "synthetic-claude-model" {
			t.Error("模型编号未透传")
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, event(`{"type":"message_start","message":{"id":"msg-test","usage":{"input_tokens":1}}}`)+event(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"授权聊天成功"}}`)+event(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`)+event(`{"type":"message_stop"}`))
		} else {
			io.WriteString(w, `{"type":"message","id":"msg-test","role":"assistant","model":"synthetic-claude-model","content":[{"type":"text","text":"授权聊天成功"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`)
		}
	}))
	defer server.Close()
	client := NewClaude(Config{Timeout: time.Second})
	cred := testCredential(t, client, map[string]any{"access_token": "synthetic-oauth-access", "base_url": server.URL + "/v1"})
	catalog, err := client.FetchCatalog(t.Context(), cred)
	if err != nil || len(catalog) != 1 || catalog[0].NativeModel != "synthetic-claude-model" {
		t.Fatal("OAuth 模型目录查询失败", err)
	}
	req := &provider.ChatRequest{Model: "synthetic-claude-model", Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"你好"`)}}}
	if _, err := client.ChatNonStream(t.Context(), cred, req); err != nil {
		t.Fatal("OAuth 非流式聊天失败", err)
	}
	stream, err := client.ChatStream(t.Context(), cred, req)
	if err != nil {
		t.Fatal("OAuth 流式聊天失败", err)
	}
	raw, err := io.ReadAll(stream.Body)
	stream.Close()
	if err != nil || !strings.Contains(string(raw), "授权聊天成功") || !strings.Contains(string(raw), "[DONE]") || requests != 3 {
		t.Fatal("OAuth 聊天未完整结束", err)
	}
}
