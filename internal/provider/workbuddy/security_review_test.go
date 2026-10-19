package workbuddy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 两种请求均不得把自定义鉴权头或正文转发到重定向目标。
func TestSecurityRedirectDoesNotForwardCredentials(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	client := New(Config{})
	for _, code := range []int{301, 302, 303, 307, 308} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, code) }))
		for _, transport := range []*http.Client{client.http, client.streamer} {
			req, _ := http.NewRequestWithContext(t.Context(), "POST", origin.URL, strings.NewReader(`{"secret":"synthetic"}`))
			req.Header.Set("Authorization", "Bearer synthetic")
			req.Header.Set("X-Access-Token", "synthetic")
			resp, err := transport.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != code || calls.Load() != 0 {
				t.Fatal("凭证被重定向转发")
			}
		}
		origin.Close()
	}
}

func TestSecurityLoginErrorsDoNotEchoTokenBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"code":1,"msg":"synthetic-private-token","data":{"accessToken":"synthetic-private-token"}}`))
	}))
	defer upstream.Close()
	client := newLoginClient(t, upstream.URL)
	_, _, err := client.StartLogin(t.Context(), "private-login", false)
	if err == nil || strings.Contains(err.Error(), "synthetic-private-token") {
		t.Fatal("授权错误公开上游秘密正文")
	}
	loginMu.Lock()
	loginStates["private-login"] = "st_test"
	loginMu.Unlock()
	defer client.CancelLogin("private-login")
	_, _, err = client.PollLogin(t.Context(), "private-login", false)
	if err == nil || strings.Contains(err.Error(), "synthetic-private-token") {
		t.Fatal("令牌轮询错误公开秘密正文")
	}
}
