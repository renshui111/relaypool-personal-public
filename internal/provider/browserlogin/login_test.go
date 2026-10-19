package browserlogin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}
func TestCodexDeviceGrantAndPending(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "auth.openai.com" {
			t.Fatalf("错误授权域名：%s", r.URL.Host)
		}
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			return response(200, `{"device_auth_id":"test-device","user_code":"TEST-1234","interval":"5"}`), nil
		case "/api/accounts/deviceauth/token":
			if requests == 2 {
				return response(403, ``), nil
			}
			return response(200, `{"authorization_code":"one-time-code","code_verifier":"server-verifier"}`), nil
		case "/oauth/token":
			body, _ := io.ReadAll(r.Body)
			values, _ := url.ParseQuery(string(body))
			if values.Get("redirect_uri") != "https://auth.openai.com/deviceauth/callback" || values.Get("code_verifier") != "server-verifier" {
				t.Fatal("授权码兑换参数错误")
			}
			return response(200, `{"access_token":"test-access","refresh_token":"test-refresh","expires_in":3600}`), nil
		}
		t.Fatalf("意外请求：%s", r.URL.Path)
		return nil, nil
	})}
	s, err := Begin(context.Background(), client, "codex", provider.BrowserLoginOptions{Region: "global", Models: []string{"my-model"}})
	if err != nil {
		t.Fatal(err)
	}
	if s.UserCode != "TEST-1234" || s.PollInterval != 5*time.Second {
		t.Fatal("设备验证码或轮询间隔错误")
	}
	if _, done, err := Poll(context.Background(), client, s); err != nil || done {
		t.Fatal("未授权阶段错误")
	}
	if _, done, err := Poll(context.Background(), client, s); err != nil || done || requests != 2 {
		t.Fatal("未遵守轮询间隔")
	}
	s.NextPoll = time.Time{}
	raw, done, err := Poll(context.Background(), client, s)
	if err != nil || !done {
		t.Fatal("授权无法完成", err)
	}
	var tokens map[string]any
	_ = json.Unmarshal(raw, &tokens)
	if tokens["refresh_token"] != "test-refresh" || tokens["client_id"] != codexClientID {
		t.Fatal("刷新凭证未保留")
	}
}

func TestQoderRegionsAndPKCE(t *testing.T) {
	for _, region := range []string{"cn", "global"} {
		t.Run(region, func(t *testing.T) {
			s, err := Begin(context.Background(), &http.Client{}, "qoder", provider.BrowserLoginOptions{Region: region})
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(s.AuthURL)
			if u.Query().Get("challenge") != challenge(s.Verifier) || u.Query().Get("nonce") != s.State {
				t.Fatal("PKCE 参数不匹配")
			}
			client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if !strings.HasPrefix(r.URL.String(), qoderAPI(s)) {
					t.Fatal("地区串用")
				}
				if r.URL.Query().Get("verifier") != s.Verifier {
					t.Fatal("轮询未绑定本次授权")
				}
				return response(200, `{"token":"test-access","uid":"test-user","refresh_token":"drt-test","expires_in":3600}`), nil
			})}
			raw, done, err := Poll(context.Background(), client, s)
			if err != nil || !done {
				t.Fatal(err)
			}
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			if m["region"] != region || m["machine_id"] != s.MachineID {
				t.Fatal("原登录地区或设备未保留")
			}
		})
	}
}

func TestCallbackRejectsInjectionBeforeNetwork(t *testing.T) {
	s, err := Begin(context.Background(), &http.Client{}, "gemini", provider.BrowserLoginOptions{Region: "global", ClientID: "my-client", ProjectID: "my-project", RedirectURI: "http://127.0.0.1:8787/auth/callback"})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("错误回调触发了网络请求")
		return nil, nil
	})}
	for _, callback := range []string{"http://evil.test/auth/callback?state=" + s.State, "http://127.0.0.1:8787/wrong?state=" + s.State, "http://127.0.0.1:8787/auth/callback?state=wrong&code=bad", "http://127.0.0.1:8787/auth/callback?state=" + s.State + "&error=access_denied"} {
		if _, err := Complete(context.Background(), client, s, callback); err == nil {
			t.Fatal("接受了错误回调")
		}
	}
}

func TestGeminiOwnClientAndCallbackOnce(t *testing.T) {
	o := provider.BrowserLoginOptions{Region: "global", ClientID: "my-client", ClientSecret: "my-secret", ProjectID: "my-project", RedirectURI: "http://127.0.0.1:8787/auth/callback"}
	s, err := Begin(context.Background(), &http.Client{}, "gemini", o)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(r.Body)
		values, _ := url.ParseQuery(string(body))
		if r.URL.Host != "oauth2.googleapis.com" || values.Get("client_id") != o.ClientID || values.Get("client_secret") != o.ClientSecret || values.Get("code_verifier") != s.Verifier {
			t.Fatal("未使用用户配置或 PKCE")
		}
		return response(200, `{"access_token":"test-access","refresh_token":"test-refresh","expires_in":3600}`), nil
	})}
	callback := o.RedirectURI + "?state=" + s.State + "&code=once"
	for range 2 {
		raw, err := Complete(context.Background(), client, s, callback)
		if err != nil {
			t.Fatal(err)
		}
		var tokens map[string]any
		_ = json.Unmarshal(raw, &tokens)
		if tokens["project_id"] != o.ProjectID || tokens["mode"] != "code_assist" {
			t.Fatal("Code Assist 配置未保留")
		}
	}
	if calls != 1 {
		t.Fatal("重复兑换了一次性授权码")
	}
}

func TestTraeRegionalCallbackAndVerifiedIdentity(t *testing.T) {
	for _, family := range []string{"trae", "trae-intl"} {
		t.Run(family, func(t *testing.T) {
			o := provider.BrowserLoginOptions{RedirectURI: "http://127.0.0.1:8787/authorize"}
			s, err := Begin(context.Background(), &http.Client{}, family, o)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if !strings.HasPrefix(r.URL.String(), traeAPI(s)) {
					t.Fatal("国内国际凭据串用")
				}
				switch r.URL.Path {
				case "/trae/api/v3/oauth/ExchangeToken":
					var m map[string]any
					_ = json.NewDecoder(r.Body).Decode(&m)
					if m["CodeVerifier"] != s.Verifier || m["ClientID"] != s.Options.ClientID {
						t.Fatal("授权码未绑定原客户端")
					}
					return response(200, `{"Result":{"Token":"test-access","RefreshToken":"test-refresh","TokenExpireAt":1999999999}}`), nil
				case "/cloudide/api/v3/trae/GetUserInfo":
					if r.Header.Get("X-Cloudide-Token") != "test-access" {
						t.Fatal("未向平台验证身份")
					}
					return response(200, `{"Result":{"UserID":"real-upstream-user"}}`), nil
				}
				t.Fatal("未知请求")
				return nil, nil
			})}
			raw, err := Complete(context.Background(), client, s, o.RedirectURI+"?loginTraceID="+s.State+"&authCode=one-time-code&host=https://evil.test")
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			if m["machine_id"] != s.MachineID || m["device_id"] != s.DeviceID || m["uid"] != "real-upstream-user" {
				t.Fatal("原设备或真实用户编号未保存")
			}
		})
	}
}

func TestOAuthErrorDoesNotRevealCredentials(t *testing.T) {
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return response(400, `{"error":"my-secret test-refresh"}`), nil
	})}
	_, err := oauthToken(context.Background(), client, "https://oauth2.googleapis.com/token", url.Values{})
	if err == nil || strings.Contains(err.Error(), "test-refresh") || strings.Contains(err.Error(), "my-secret") {
		t.Fatal("错误暴露了凭据")
	}
}
