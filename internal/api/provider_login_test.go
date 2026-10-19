package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/browserlogin"
	"github.com/relaypool/relaypool/internal/provider/native"
)

type syntheticBrowserAdapter struct {
	provider.Provider
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *syntheticBrowserAdapter) BeginBrowserLogin(ctx context.Context, o provider.BrowserLoginOptions) (*provider.BrowserLoginSession, error) {
	return browserlogin.Begin(ctx, &http.Client{}, "gemini", o)
}
func (c *syntheticBrowserAdapter) PollBrowserLogin(ctx context.Context, s *provider.BrowserLoginSession) ([]byte, bool, error) {
	if c.entered != nil {
		c.once.Do(func() { close(c.entered) })
		select {
		case <-c.release:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	return []byte(`{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","client_id":"own-client","project_id":"own-project","mode":"code_assist","models":["synthetic-model"]}`), true, nil
}
func (c *syntheticBrowserAdapter) CompleteBrowserLogin(context.Context, *provider.BrowserLoginSession, string) ([]byte, error) {
	return nil, provider.ErrUnsupported
}

func loginTestRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer rp_test")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func startSyntheticLogin(t *testing.T, s *Server) string {
	t.Helper()
	w := loginTestRequest(s, "POST", "/admin/accounts/login/start", `{"provider":"gemini","region":"global","client_id":"own-client","project_id":"own-project"}`)
	if w.Code != 201 {
		t.Fatalf("开始授权失败：%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "synthetic-access") || strings.Contains(w.Body.String(), "own-secret") {
		t.Fatal("返回秘密")
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body["login_id"].(string)
}

func TestProviderBrowserLoginSavesUsableCredentialAndLateCancelPreservesAccount(t *testing.T) {
	s := newTestServerNoAdapter(t)
	s.registry.Register(&syntheticBrowserAdapter{Provider: native.NewGemini(native.Config{})})
	id := startSyntheticLogin(t, s)
	w := loginTestRequest(s, "GET", "/admin/accounts/login/"+id, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"done"`) {
		t.Fatal("授权未保存", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "synthetic-access") || strings.Contains(w.Body.String(), "synthetic-refresh") {
		t.Fatal("完成响应暴露凭据")
	}
	if a, err := s.store.GetAccount(context.Background(), id); err != nil || !a.Enabled || !a.HasCredential || a.AuthType != "oauth" {
		t.Fatal("保存的账号不可使用", err)
	}
	loginTestRequest(s, "POST", "/admin/accounts/login/"+id+"/cancel", "{}")
	if _, err := s.store.GetCredential(context.Background(), id); err != nil {
		t.Fatal("晚到的取消删掉了账号凭证")
	}
}

func TestProviderLoginCancelCannotRaceSuccessfulSave(t *testing.T) {
	s := newTestServerNoAdapter(t)
	adapter := &syntheticBrowserAdapter{Provider: native.NewGemini(native.Config{}), entered: make(chan struct{}), release: make(chan struct{})}
	s.registry.Register(adapter)
	id := startSyntheticLogin(t, s)
	pollDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { pollDone <- loginTestRequest(s, "GET", "/admin/accounts/login/"+id, "") }()
	<-adapter.entered
	cancelDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { cancelDone <- loginTestRequest(s, "POST", "/admin/accounts/login/"+id+"/cancel", "{}") }()
	close(adapter.release)
	if w := <-pollDone; w.Code != 200 {
		t.Fatal("保存失败")
	}
	<-cancelDone
	if a, err := s.store.GetAccount(context.Background(), id); err != nil || !a.Enabled || !a.HasCredential {
		t.Fatal("取消删除了正在完成的账号")
	}
}

func TestBrowserCallbackRejectsWrongStateOriginAndPath(t *testing.T) {
	s := newTestServerNoAdapter(t)
	s.registry.Register(&syntheticBrowserAdapter{Provider: native.NewGemini(native.Config{})})
	id := startSyntheticLogin(t, s)
	p, _ := s.getLogin(id)
	for _, path := range []string{"/auth/callback?state=wrong&code=bad", "/authorize?state=" + url.QueryEscape(p.flow.session.State) + "&code=bad"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal("错误状态或路径未拒绝", w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/auth/callback?state="+url.QueryEscape(p.flow.session.State), nil)
	r.Header.Set("Origin", "https://evil.test")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("跨站回调未拒绝")
	}
	p.flow.session.ExpiresAt = time.Now().Add(-time.Second)
	w = loginTestRequest(s, "GET", "/admin/accounts/login/"+id, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "expired") {
		t.Fatal("过期会话没有清理")
	}
	if _, err := s.store.GetAccount(context.Background(), id); err == nil {
		t.Fatal("遗留无凭证账号")
	}
}

func TestUnsupportedCheckinConfigurationIsRejected(t *testing.T) {
	s := newTestServerNoAdapter(t)
	w := loginTestRequest(s, "POST", "/admin/accounts", `{"name":"api-no-checkin","provider":"openai","region":"global","auto_checkin":true,"enabled":false}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "unsupported_checkin") {
		t.Fatal("不支持签到的渠道仍可开启签到", w.Code, w.Body.String())
	}
	w = loginTestRequest(s, "GET", "/admin/providers", "")
	var data struct {
		Data []struct {
			ID      string                     `json:"id"`
			Browser bool                       `json:"browser_login"`
			Checkin provider.CheckinCapability `json:"checkin"`
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &data)
	for _, p := range data.Data {
		if p.ID == "qoder" && (!p.Browser || p.Checkin.Mode != "campaign") {
			t.Fatal("Qoder 能力不正确")
		}
		if p.ID == "trae-intl" && p.Checkin.Supported {
			t.Fatal("国际版误开签到")
		}
		if p.ID == "claude" && !p.Browser {
			t.Fatal("已接入的 Claude 浏览器授权未展示")
		}
	}
}
