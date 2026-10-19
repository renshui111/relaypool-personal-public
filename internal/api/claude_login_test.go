package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/browserlogin"
	"github.com/relaypool/relaypool/internal/provider/native"
)

type claudeLoginTransport func(*http.Request) (*http.Response, error)

func (f claudeLoginTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 模拟真实 Claude 适配器的授权请求，其他凭证解析沿用正式实现。
type claudeLoginAdapter struct {
	provider.Provider
	client *http.Client
}

func (c *claudeLoginAdapter) BeginBrowserLogin(ctx context.Context, o provider.BrowserLoginOptions) (*provider.BrowserLoginSession, error) {
	return browserlogin.Begin(ctx, c.client, "claude", o)
}
func (c *claudeLoginAdapter) PollBrowserLogin(ctx context.Context, s *provider.BrowserLoginSession) ([]byte, bool, error) {
	return browserlogin.Poll(ctx, c.client, s)
}
func (c *claudeLoginAdapter) CompleteBrowserLogin(ctx context.Context, s *provider.BrowserLoginSession, input string) ([]byte, error) {
	return browserlogin.Complete(ctx, c.client, s, input)
}

func claudeLoginTestServer(t *testing.T) (*Server, *int) {
	t.Helper()
	s := newTestServerNoAdapter(t)
	calls := new(int)
	client := &http.Client{Transport: claudeLoginTransport(func(r *http.Request) (*http.Response, error) {
		*calls = *calls + 1
		if r.URL.String() != browserlogin.ClaudeTokenURL {
			t.Fatal("登录请求未固定到 Claude 令牌服务")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"access_token":"synthetic-claude-access","refresh_token":"synthetic-claude-refresh","expires_in":3600,"scope":"user:profile user:inference","account":{"uuid":"synthetic-claude-user"}}`))}, nil
	})}
	s.registry.Register(&claudeLoginAdapter{Provider: native.NewClaude(native.Config{}), client: client})
	return s, calls
}

func startClaudeTestLogin(t *testing.T, s *Server, host, body string) (string, string) {
	t.Helper()
	r := httptest.NewRequest("POST", "/admin/accounts/login/start", strings.NewReader(body))
	r.Host = host
	r.Header.Set("Authorization", "Bearer rp_test")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatal("无法开始 Claude 授权", w.Code, w.Body.String())
	}
	var result struct {
		ID       string `json:"login_id"`
		Redirect string `json:"callback_url"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return result.ID, result.Redirect
}

func assertClaudeSaved(t *testing.T, s *Server, id string) {
	t.Helper()
	account, err := s.store.GetAccount(t.Context(), id)
	if err != nil || !account.HasCredential || !account.Enabled || account.AuthType != "oauth" || account.Region != "global" {
		t.Fatal("Claude 授权结果未自动入池", err)
	}
	stored, err := s.store.GetCredential(t.Context(), id)
	if err != nil || !stored.ExpiresAt.After(time.Now()) || stored.Format != "claude-oauth-v1" {
		t.Fatal("未保存凭证期限或渠道格式", err)
	}
	w := loginTestRequest(s, "GET", "/admin/accounts/login/"+id, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"done"`) || strings.Contains(w.Body.String(), "synthetic-claude-access") || strings.Contains(w.Body.String(), "synthetic-claude-refresh") {
		t.Fatal("完成状态不正确或向浏览器泄露令牌", w.Body.String())
	}
	loginTestRequest(s, "POST", "/admin/accounts/login/"+id+"/cancel", "{}")
	if _, err := s.store.GetCredential(t.Context(), id); err != nil {
		t.Fatal("晚到取消删掉了已完成的账号")
	}
}

func TestClaudeBrowserCallbackSavesBeforePolling(t *testing.T) {
	s, calls := claudeLoginTestServer(t)
	id, redirect := startClaudeTestLogin(t, s, "127.0.0.1:9876", `{"provider":"claude","models":["actual-model"]}`)
	if redirect != "http://localhost:9876/callback" {
		t.Fatal("本机回调未使用访问端口和官方注册路径")
	}
	p, _ := s.getLogin(id)
	state := p.flow.session.State
	r := httptest.NewRequest("GET", "/callback?code=once&state="+url.QueryEscape(state), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || *calls != 1 {
		t.Fatal("本机授权回调失败", w.Code, w.Body.String())
	}
	// 尚未调用状态轮询，凭证和启用状态也必须已经落库。
	assertClaudeSaved(t, s, id)
	// 轮询可能在回调前取到快照、回调后才拿到锁，仍应报告完成。
	stalePoll := httptest.NewRecorder()
	s.pollProviderLogin(stalePoll, httptest.NewRequest("GET", "/admin/accounts/login/"+id, nil), p)
	if !strings.Contains(stalePoll.Body.String(), `"status":"done"`) {
		t.Fatal("晚到轮询把已完成授权误判为过期")
	}
	snapshot, _ := json.Marshal(s.pool.Snapshot())
	if !strings.Contains(string(snapshot), "actual-model") {
		t.Fatal("登录时填写的模型未交给号池")
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 400 || *calls != 1 {
		t.Fatal("回调重放重复兑换令牌")
	}
}

func TestClaudeRemoteCodeSavesAndCustomClientMustSpecifyRedirect(t *testing.T) {
	s, calls := claudeLoginTestServer(t)
	id, redirect := startClaudeTestLogin(t, s, "192.168.1.10:8787", `{"provider":"claude"}`)
	if redirect != browserlogin.ClaudeManualRedirectURI {
		t.Fatal("局域网授权依赖访问者的本机端口")
	}
	p, _ := s.getLogin(id)
	bad, _ := json.Marshal(map[string]string{"callback_url": "once#wrong"})
	if w := loginTestRequest(s, "POST", "/admin/accounts/login/"+id+"/complete", string(bad)); w.Code != 400 || *calls != 0 {
		t.Fatal("错误状态没有在请求上游前拒绝")
	}
	body, _ := json.Marshal(map[string]string{"callback_url": "once#" + p.flow.session.State})
	w := loginTestRequest(s, "POST", "/admin/accounts/login/"+id+"/complete", string(body))
	if w.Code != 200 || *calls != 1 || strings.Contains(w.Body.String(), "synthetic-claude-") {
		t.Fatal("远程授权码无法保存或泄露令牌", w.Code, w.Body.String())
	}
	assertClaudeSaved(t, s, id)
	for _, input := range []string{`{"provider":"claude","region":"cn"}`, `{"provider":"claude","client_id":"own-client"}`, `{"provider":"claude","redirect_uri":"http://evil.invalid/callback"}`} {
		if w := loginTestRequest(s, "POST", "/admin/accounts/login/start", input); w.Code != 400 {
			t.Fatal("接受了不匹配的地区或客户端回调配置")
		}
	}
}
