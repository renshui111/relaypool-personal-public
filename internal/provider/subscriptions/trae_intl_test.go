package subscriptions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

func intlCredential(t *testing.T, c *Client, endpoint string) provider.Credential {
	return testCredential(t, c, endpoint, map[string]any{"provider": "trae", "region": "global", "access_token": "intl-synthetic", "refresh_token": "intl-refresh", "timezone": "Asia/Shanghai", "web_id": "original-web"})
}

func TestIntlCredentialBoundaries(t *testing.T) {
	c := NewTraeIntl(Config{})
	for _, raw := range []string{
		`{"access_token":"synthetic","uid":"user","region":"cn"}`,
		`{"access_token":"synthetic","uid":"user","region":"global","api_host":"https://api.trae.com.cn"}`,
		`{"access_token":"synthetic","uid":"user","referer_origin":"https://evil.invalid"}`,
		`{"access_token":"synthetic","uid":"user","timezone":"Asia/Shanghai\r\nSecret: bad"}`,
	} {
		if _, err := c.DecodeCredential([]byte(raw)); err == nil {
			t.Fatal("国际凭证接受了跨地区、跨站或头注入")
		}
	}
	cred, err := c.DecodeCredential([]byte(`{"auth":{"accessToken":"synthetic","domain":"trae.ai","region":"US-East","webId":"original"},"account":{"uid":"user"},"providerSpecificData":{"userTimezone":"Asia/Shanghai"}}`))
	if err != nil || cred.Region != provider.RegionGlobal || cred.Field("upstream_region") != "US-East" || cred.Field("timezone") != "Asia/Shanghai" || cred.Field("web_id") != "original" {
		t.Fatalf("完整登录态导入失效：%v", err)
	}
}

func TestIntlWireCatalogAndCumulativeText(t *testing.T) {
	c := NewTraeIntl(Config{})
	var created atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT intl-synthetic" || r.Header.Get("Origin") != "https://work.trae.ai" || r.Header.Get("X-Trae-User-Timezone") != "Asia/Shanghai" {
			t.Error("国际请求头未保留登录来源和时区")
		}
		switch r.URL.Path {
		case traeIntlPrefix + "/models":
			io.WriteString(w, `{"code":0,"data":[{"name":"model-test"}]}`)
		case traeIntlPrefix + "/chat_sessions":
			created.Add(1)
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			initial := object(body["initial_message"])
			if body["auto_create_project"] != false || initial["model_name"] != "model-test" || initial["model_selection_strategy"] != "manual" || !strings.Contains(initial["query"].(string), "[System]") {
				t.Error("国际会话创建字段或完整历史错误")
			}
			io.WriteString(w, `{"code":0,"data":{"chat_session_id":"session-one","message_id":"message-one"}}`)
		case traeIntlPrefix + "/chat_sessions/session-one/events":
			if r.URL.Query().Get("reply_to_message_id") != "message-one" {
				t.Error("会话消息关联错误")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: plan_item\ndata: {\"id\":\"p1\",\"thought\":\" 你好\"}\n\nevent: plan_item\ndata: {\"id\":\"p1\",\"thought\":\" 你好\"}\n\nevent: plan_item\ndata: {\"id\":\"p1\",\"thought\":\" \"}\n\nevent: plan_item\ndata: {\"id\":\"p1\",\"thought\":\" 你好 世界 \"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":3,\"completion_tokens\":2}\n\nevent: done\ndata: {}\n\n")
		default:
			t.Error("请求走入错误协议路径")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	cred := intlCredential(t, c, server.URL)
	models, err := c.FetchCatalog(context.Background(), cred)
	if err != nil || len(models) != 3 || models[0].Tools {
		t.Fatalf("目录错误：%v", err)
	}
	for i := 0; i < 2; i++ {
		result, err := c.ChatNonStream(context.Background(), cred, &provider.ChatRequest{Model: "model-test", Messages: []provider.Message{{Role: "system", Content: marshal("指令")}, {Role: "user", Content: marshal("hello")}}})
		if err != nil || result.Message.ContentText() != " 你好 世界 " || result.Usage.TotalTokens != 5 {
			t.Fatalf("累计文本重复、空格丢失或用量错误：%v", err)
		}
	}
	if created.Load() != 2 {
		t.Fatal("请求没有建立各自独立的会话")
	}
}

func TestIntlCommittedSessionNeverRetries(t *testing.T) {
	for _, scenario := range []string{"events-auth", "truncated", "invalid-id", "late-error"} {
		t.Run(scenario, func(t *testing.T) {
			c := NewTraeIntl(Config{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					if scenario == "invalid-id" {
						io.WriteString(w, `{"code":0,"data":{"chat_session_id":"../escape","message_id":"m1"}}`)
					} else {
						io.WriteString(w, `{"code":0,"data":{"chat_session_id":"s1","message_id":"m1"}}`)
					}
					return
				}
				if scenario == "events-auth" {
					w.WriteHeader(401)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "event: plan_item\ndata: {\"id\":\"p1\",\"thought\":\"text\"}\n\n")
				if scenario == "late-error" {
					io.WriteString(w, "event: error\ndata: {\"code\":429,\"message\":\"intl-synthetic\"}\n\n")
				}
			}))
			defer server.Close()
			_, err := c.ChatNonStream(context.Background(), intlCredential(t, c, server.URL), &provider.ChatRequest{Model: "auto", Messages: []provider.Message{{Role: "user", Content: marshal("hi")}}})
			f, ok := provider.AsFailure(err)
			if !ok || !f.NoRetry || strings.Contains(f.Error(), "intl-synthetic") {
				t.Fatal("受理后的失败可重复创建任务或泄露凭证")
			}
		})
	}
}

func TestIntlUnsupportedRequestsStayLocal(t *testing.T) {
	c := NewTraeIntl(Config{})
	cred := intlCredential(t, c, "")
	for _, req := range []*provider.ChatRequest{
		{Model: "auto"},
		{Model: "auto", Messages: []provider.Message{{Role: "user", Content: marshal("hi")}}, Tools: []byte(`[{"type":"function","function":{"name":"echo","parameters":{"type":"object"}}}]`)},
		{Model: "auto", Messages: []provider.Message{{Role: "tool", Content: marshal("tool result"), ToolCallID: "one"}}},
		{Model: "auto", Messages: []provider.Message{{Role: "user", Content: []byte(`[{"type":"image_url","image_url":{"url":"data:image/png;base64,aA=="}}]`)}}},
		{Model: "auto", Messages: []provider.Message{{Role: "user", Content: marshal("hi")}}, DisableThinking: true},
	} {
		_, err := c.traeIntlBody(cred, req)
		f, ok := provider.AsFailure(err)
		if !ok || f.Status != 400 || f.KindHint != "invalid_request" {
			t.Fatal("不支持的参数未拒绝，或错误分类会损伤账号健康状态")
		}
	}
}

func TestIntlRefreshPreservesWebIdentity(t *testing.T) {
	c := NewTraeIntl(Config{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/cloudide/api/v3/trae/oauth/ExchangeToken" || body["ClientID"] != "ono9krqynydwx5" {
			t.Error("刷新跨入国内协议")
		}
		io.WriteString(w, `{"Result":{"AccessToken":"intl-new","RefreshToken":"intl-refresh-new","TokenExpireDuration":3600}}`)
	}))
	defer server.Close()
	updated, err := c.Refresh(context.Background(), intlCredential(t, c, server.URL))
	if err != nil || updated.Field("access_token") != "intl-new" || updated.Field("web_id") != "original-web" || updated.Field("timezone") != "Asia/Shanghai" || time.Until(updated.ExpiresAt) < 59*time.Minute {
		t.Fatalf("国际刷新丢失身份字段：%v", err)
	}
}
