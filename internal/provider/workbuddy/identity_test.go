package workbuddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/provider"
)

func TestDesktopIdentityOnFinalOutboundRequests(t *testing.T) {
	for _, tc := range []struct{ name, profile, domain, ua, ide, host string }{
		{"desktop-cn", DesktopIdentity593, "codebuddy.cn", "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.161.0", "WorkBuddy", "copilot.tencent.com"},
		{"legacy-cn", "cli", "codebuddy.cn", "custom-UA", "CLI", "copilot.tencent.com"},
		{"global-unchanged", DesktopIdentity593, "workbuddy.ai", "custom-UA", "CLI", "www.workbuddy.ai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(Config{IdentityProfile: tc.profile, UserAgent: "custom-UA"})
			cred := provider.Credential{Raw: []byte(`{"access_token":"synthetic-token","uid":"synthetic-user","domain":"` + tc.domain + `"}`)}
			calls := map[string]int{}
			c.http.Transport = checkinBusinessTransport(func(r *http.Request) (*http.Response, error) {
				calls[r.URL.Path]++
				if r.URL.Host != tc.host || r.Header.Get("User-Agent") != tc.ua || r.Header.Get("Authorization") != "Bearer synthetic-token" {
					t.Fatal("destination, identity or credential changed unexpectedly")
				}
				if tc.name == "desktop-cn" {
					for k, want := range map[string]string{"X-IDE-Type": "WorkBuddy", "X-IDE-Name": "WorkBuddy", "X-IDE-Version": "5.9.3", "X-Product-Version": "5.9.3", "X-Domain": "workbuddy.cn", "Origin": "https://www.workbuddy.cn", "Referer": "https://www.workbuddy.cn/", "X-Agent-Intent": "craft", "X-Agent-Purpose": "conversation", "X-Agent-Type": "main", "X-Agent-Runtime": "local-agent"} {
						if r.Header.Get(k) != want {
							t.Errorf("%s=%q want %q", k, r.Header.Get(k), want)
						}
					}
				}
				body := `{"data":{"models":[]}}`
				if r.URL.Path == pathChat {
					if r.Header.Get("X-IDE-Type") != tc.ide {
						t.Fatal("wrong chat channel")
					}
					body = "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
				} else if tc.name != "desktop-cn" && r.Header.Get("X-IDE-Type") != "" {
					t.Fatal("legacy catalog acquired chat headers")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			c.streamer.Transport = c.http.Transport
			// The fixture deliberately has no models; capture still proves catalog headers.
			_, _ = c.FetchCatalog(context.Background(), cred)
			req := &provider.ChatRequest{Model: "auto", Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"OK"`)}}}
			result, err := c.ChatNonStream(context.Background(), cred, req)
			if err != nil || result.Message.ContentText() != "OK" {
				t.Fatalf("nonstream: %v", err)
			}
			s, err := c.ChatStream(context.Background(), cred, req)
			if err != nil {
				t.Fatal(err)
			}
			s.Body.Close()
			if calls[pathChat] != 2 || len(calls) != 2 {
				t.Fatalf("missing wire-path coverage: %v", calls)
			}
		})
	}
}

func TestDesktopIdentityDoesNotAlterBillingOrRefresh(t *testing.T) {
	c := New(Config{IdentityProfile: DesktopIdentity593})
	cred := provider.Credential{Raw: []byte(`{"access_token":"synthetic-token","refresh_token":"synthetic-refresh","uid":"synthetic-user","domain":"codebuddy.cn"}`)}
	calls := 0
	c.http.Transport = checkinBusinessTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("User-Agent") != UserAgent || r.Header.Get("X-IDE-Type") != "" || r.Header.Get("Origin") != "https://www.codebuddy.cn" {
			t.Fatal("desktop profile leaked to billing/refresh")
		}
		return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":1}`))}, nil
	})
	_, _ = c.Checkin(context.Background(), cred)
	_, _ = c.FetchQuota(context.Background(), cred)
	_, _ = c.Refresh(context.Background(), cred)
	if calls != 3 {
		t.Fatalf("expected three paths, got %d", calls)
	}
}
