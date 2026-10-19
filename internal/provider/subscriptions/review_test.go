package subscriptions

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/native"
)

func TestStreamPreservesWhitespaceAndRejectsEmptyTools(t *testing.T) {
	c := NewQoder(Config{})
	cred := testCredential(t, c, "", nil)
	for _, test := range []struct {
		raw   string
		valid bool
	}{
		{"data: {\"choices\":[{\"delta\":{\"content\":\" \"}}]}\n\ndata: [DONE]\n\n", true},
		{"data: {\"choices\":[{\"delta\":{\"tool_calls\":[]},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", false},
	} {
		stream := newSubscriptionStream(io.NopCloser(strings.NewReader(test.raw)), c, cred, "model")
		defer stream.Close()
		result, err := native.CollectChatStream(stream, "model")
		if test.valid {
			if err != nil || result.Message.ContentText() != " " {
				t.Fatalf("丢失空格 %v", err)
			}
		} else if err == nil {
			t.Fatal("空工具数组不能作为回答")
		}
	}
}

func TestStreamHTTPErrorCodesKeepRealClassification(t *testing.T) {
	c := NewTrae(Config{})
	cred := testCredential(t, c, "", nil)
	for code, expected := range map[string]string{"401": "auth", "429": "rate_limit", "503": ""} {
		f, _ := provider.AsFailure(c.upstreamFailure(cred, 502, code, []byte("synthetic-trae-access"), nil))
		if f.KindHint != expected || f.Body != "" || strings.Contains(f.Message, "synthetic-trae-access") {
			t.Fatal("分类或脱敏错误")
		}
	}
}

func TestUpstreamErrorCodeCannotEchoCredential(t *testing.T) {
	c := NewTrae(Config{})
	cred := testCredential(t, c, "", nil)
	for _, code := range []string{"synthetic-trae-access", strings.Repeat("private", 10000), "999999999", "invalid_token:synthetic-trae-access"} {
		f, _ := provider.AsFailure(c.upstreamFailure(cred, 502, code, []byte("invalid token"), nil))
		if f.Code != "subscription_upstream_error" || f.Status != 401 || f.KindHint != "auth" || f.Body != "" {
			t.Fatal("错误码泄露正文或破坏认证分类")
		}
	}
}

func TestRefreshCacheCannotBlockOtherAccounts(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"device_token":"dt-new","refresh_token":"drt-new","expires_in":3600000}`)
	}))
	defer server.Close()
	c := NewQoder(Config{})
	cred := testCredential(t, c, server.URL, map[string]any{"refresh_token": "drt-old"})
	for i := 0; i < 128; i++ {
		done := make(chan struct{})
		close(done)
		c.refreshes[fmt.Sprint("finished-", i)] = &refreshCall{done: done, finished: time.Now().Add(-time.Second)}
	}
	if _, err := c.Refresh(context.Background(), cred); err != nil || calls.Load() != 1 || len(c.refreshes) != 128 {
		t.Fatalf("已完成缓存阻塞了其他账号或容量越界：%v", err)
	}
	// 正在刷新的结果不能驱逐，否则会对旋转令牌重复发起刷新。
	active := NewQoder(Config{})
	for i := 0; i < 128; i++ {
		active.refreshes[fmt.Sprint("active-", i)] = &refreshCall{done: make(chan struct{})}
	}
	_, err := active.Refresh(context.Background(), cred)
	f, ok := provider.AsFailure(err)
	if !ok || f.Status != 503 || len(active.refreshes) != 128 || calls.Load() != 1 {
		t.Fatal("在途刷新被驱逐或并发容量失效")
	}
}

func TestRedirectCannotForwardSubscriptionCredential(t *testing.T) {
	var stolen atomic.Int32
	thief := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { stolen.Add(1) }))
	defer thief.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, thief.URL, 307) }))
	defer source.Close()
	c := NewTrae(Config{})
	cred := testCredential(t, c, source.URL, nil)
	_, err := c.ChatStream(context.Background(), cred, &provider.ChatRequest{Model: "test", Messages: []provider.Message{{Role: "user", Content: marshal("hi")}}})
	if err == nil || stolen.Load() != 0 {
		t.Fatal("重定向转发了订阅凭证")
	}
}

func TestStreamIdleTimeoutReleasesUpstream(t *testing.T) {
	canceled := make(chan struct{})
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer source.Close()
	c := NewTrae(Config{IdleTimeout: 20 * time.Millisecond})
	cred := testCredential(t, c, source.URL, nil)
	_, err := c.ChatStream(context.Background(), cred, &provider.ChatRequest{Model: "test", Messages: []provider.Message{{Role: "user", Content: marshal("hi")}}})
	if err == nil {
		t.Fatal("空闲流没有超时")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("超时后上游连接未关闭")
	}
}
