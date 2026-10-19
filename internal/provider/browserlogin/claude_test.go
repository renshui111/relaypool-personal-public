package browserlogin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

const claudeTestTokens = `{"access_token":"synthetic-claude-access","refresh_token":"synthetic-claude-refresh","expires_in":3600,"token_type":"Bearer","scope":"user:profile user:inference","account":{"uuid":"synthetic-user"},"organization":{"uuid":"synthetic-org"},"base_url":"https://evil.invalid","models":["evil-model"],"client_id":"evil-client"}`

func TestClaudeAuthorizationBindsClientPKCEAndRegisteredRedirect(t *testing.T) {
	for _, redirect := range []string{"", "http://localhost:8787/callback", "https://relay.example/auth/callback"} {
		s, err := Begin(t.Context(), &http.Client{}, "claude", provider.BrowserLoginOptions{RedirectURI: redirect, Models: []string{"actual-model"}})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(s.AuthURL)
		q := u.Query()
		if u.Host != "claude.ai" || q.Get("client_id") != ClaudeClientID || q.Get("code_challenge") != challenge(s.Verifier) || q.Get("state") != s.State || s.State == s.Verifier || q.Get("scope") != ClaudeScope || q.Get("redirect_uri") != s.Options.RedirectURI {
			t.Fatal("授权客户端、随机状态或 PKCE 未正确绑定")
		}
		if redirect == "" && (s.Mode != "code" || s.Options.RedirectURI != ClaudeManualRedirectURI) {
			t.Fatal("远程授权未使用平台的授权码页")
		}
		if _, done, err := Poll(t.Context(), &http.Client{}, s); err != nil || done {
			t.Fatal("等待回调不应发起设备轮询")
		}
	}
	for _, redirect := range []string{"http://evil.invalid/callback", "https://user:password@host.invalid/callback", "https://host.invalid/callback?code=wrong", "https://host.invalid/callback#fragment"} {
		if _, err := Begin(t.Context(), &http.Client{}, "claude", provider.BrowserLoginOptions{RedirectURI: redirect}); err == nil {
			t.Fatal("接受了不安全的回调地址")
		}
	}
	if _, err := Begin(t.Context(), &http.Client{}, "claude", provider.BrowserLoginOptions{ClientID: "own-approved-client"}); err == nil {
		t.Fatal("自有客户端未要求登记回调地址")
	}
}

func TestClaudeCodeAndCallbackExchangeOnce(t *testing.T) {
	for _, manual := range []bool{true, false} {
		t.Run(map[bool]string{true: "授权码", false: "回调地址"}[manual], func(t *testing.T) {
			opts := provider.BrowserLoginOptions{ClientID: "own-approved-client", ClientSecret: "synthetic-secret", RedirectURI: "http://localhost:8787/callback", Models: []string{"actual-model"}}
			s, err := Begin(t.Context(), &http.Client{}, "claude", opts)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				if r.URL.String() != ClaudeTokenURL || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || body["code_verifier"] != s.Verifier || body["state"] != s.State || body["redirect_uri"] != opts.RedirectURI || body["client_id"] != opts.ClientID || body["client_secret"] != opts.ClientSecret || body["code"] != "one-time-code" {
					t.Fatal("Claude 授权码兑换参数错误")
				}
				return response(200, claudeTestTokens), nil
			})}
			callback := opts.RedirectURI + "?code=one-time-code&state=" + s.State
			if manual {
				callback = "one-time-code#" + s.State
			}
			for range 2 {
				raw, err := Complete(t.Context(), client, s, callback)
				if err != nil {
					t.Fatal(err)
				}
				var tokens map[string]any
				_ = json.Unmarshal(raw, &tokens)
				if tokens["client_id"] != opts.ClientID || tokens["account_id"] != "synthetic-user" || tokens["base_url"] != nil || tokens["models"].([]any)[0] != "actual-model" || tokens["expires_at"] == "" || tokens["client_secret"] != opts.ClientSecret {
					t.Fatal("上游覆写配置或丢失身份、续期信息")
				}
			}
			if calls != 1 {
				t.Fatal("重复消费授权码")
			}
			if raw, done, err := Poll(t.Context(), client, s); err != nil || !done || len(raw) == 0 {
				t.Fatal("完成凭证未交给号池")
			}
		})
	}
}

func TestClaudeRejectsWrongStateDuplicateQueriesAndExpiredSessions(t *testing.T) {
	s, _ := Begin(t.Context(), &http.Client{}, "claude", provider.BrowserLoginOptions{})
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("无效授权信息触发网络请求")
		return nil, nil
	})}
	for _, text := range []string{"one-time-code", "one-time-code#wrong", "one-time-code#" + s.State + "#extra", "https://evil.invalid/oauth/code/callback?code=x&state=" + s.State, ClaudeManualRedirectURI + "?code=a&code=b&state=" + s.State, ClaudeManualRedirectURI + "?code=x&state=" + s.State + "&state=wrong", "one-time-code\n#" + s.State, ClaudeManualRedirectURI + "?code=x&state=" + s.State + "&error=access_denied"} {
		if _, err := Complete(t.Context(), client, s, text); err == nil {
			t.Fatal("接受了不匹配或含歧义的授权信息")
		}
	}
	s.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := Complete(t.Context(), client, s, "one-time-code#"+s.State); err == nil {
		t.Fatal("过期授权仍可兑换")
	}
}

func TestClaudeIncompleteTokenAndErrorNeverExposeSecrets(t *testing.T) {
	for _, body := range []string{
		`{"access_token":"secret-access","refresh_token":"secret-refresh","expires_in":3600,"scope":"user:profile"}`,
		`{"access_token":"secret-access","expires_in":3600,"scope":"user:inference"}`,
		`{"access_token":"secret-access","refresh_token":"secret-refresh","expires_in":-1,"expires_at":"2099-01-01T00:00:00Z","scope":"user:inference"}`,
		`{"access_token":"secret-access","refresh_token":"secret-refresh","expires_in":3600,"scope":"user:inference","token_type":"wrong"}`,
	} {
		s, _ := Begin(t.Context(), &http.Client{}, "claude", provider.BrowserLoginOptions{})
		client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return response(200, body), nil })}
		if _, err := Complete(t.Context(), client, s, "secret-code#"+s.State); err == nil || strings.Contains(err.Error(), "secret-") || len(s.Raw) != 0 {
			t.Fatal("接受了不完整的凭证或暴露敏感信息")
		}
	}
	s, _ := Begin(t.Context(), &http.Client{}, "claude", provider.BrowserLoginOptions{})
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return response(400, `{"error":"secret-code secret-access secret-refresh"}`), nil
	})}
	if _, err := Complete(context.Background(), client, s, "secret-code#"+s.State); err == nil || strings.Contains(err.Error(), "secret-") {
		t.Fatal("兑换错误泄露授权信息")
	}
}

func TestClaudeProfileRetryDoesNotExchangeCodeTwice(t *testing.T) {
	s, _ := Begin(t.Context(), &http.Client{}, "claude", provider.BrowserLoginOptions{})
	exchanges, profiles := 0, 0
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == ClaudeTokenURL {
			exchanges++
			return response(200, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"scope":"user:profile user:inference"}`), nil
		}
		profiles++
		if r.URL.String() != "https://api.anthropic.com/api/oauth/profile" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Fatal("身份查询地址或鉴权错误")
		}
		if profiles == 1 {
			return response(503, `{}`), nil
		}
		return response(200, `{"account":{"uuid":"verified-user"},"organization":{"uuid":"verified-org"}}`), nil
	})}
	callback := "one-time-code#" + s.State
	if _, err := Complete(t.Context(), client, s, callback); err == nil {
		t.Fatal("未确认身份时应保留待授权状态")
	}
	if _, err := Complete(t.Context(), client, s, callback); err != nil || exchanges != 1 || profiles != 2 || len(s.Exchanged) != 0 {
		t.Fatal("身份重试重复消费授权码或未清除临时令牌", err)
	}
}
