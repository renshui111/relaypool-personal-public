package workbuddy

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/executor"
	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
)

type toolHistoryCredentials struct{}

func (toolHistoryCredentials) CredentialFor(id string) (provider.Credential, error) {
	return provider.Credential{AccountID: id, Raw: []byte(`{"access_token":"test","uid":"test","domain":"codebuddy.cn"}`)}, nil
}

func TestToolHistoryMismatchStopsAtFirstAccount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		stream bool
	}{
		{"numeric", `{"error":{"code":11148,"message":"tool calls and tool results do not match, please start a new conversation and retry"}}`, true},
		{"nested-code-only", `{"error":{"data":{"code":"11148"}}}`, false},
		{"message-only", `{"error":{"message":"Tool calls and tool results do not match"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pool.New(pool.StrategyRoundRobin)
			for _, id := range []string{"a", "b"} {
				p.Upsert(pool.Item{ID: id, Provider: Family, Enabled: true, Models: []string{"m"}})
			}
			calls := 0
			client := New(Config{})
			client.streamer.Transport = checkinBusinessTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 502, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			registry := provider.NewRegistry()
			registry.Register(client)
			e := executor.New(p, registry, nil, toolHistoryCredentials{}, executor.Options{MaxAttempts: 2})
			chat := e.ChatNonStream
			if tc.stream {
				chat = e.ChatStream
			}
			for range 4 {
				before := calls
				_, err := chat(t.Context(), executor.Request{Chat: &provider.ChatRequest{Model: "m"}})
				var failure *executor.FailureError
				if !errors.As(err, &failure) || calls != before+1 || failure.Status != 400 || failure.Kind != pool.KindInvalidRequest || failure.RetryAfter != 0 {
					t.Fatalf("upstream calls=%d, failure=%+v", calls-before, failure)
				}
				if tc.name != "message-only" && failure.Code != "11148" {
					t.Errorf("lost upstream code: %q", failure.Code)
				}
				if tc.name == "nested-code-only" && !strings.Contains(failure.Message, "新建会话") {
					t.Errorf("missing recovery guidance: %q", failure.Message)
				}
				for _, item := range p.Items() {
					if !item.AccountDownUntil().IsZero() || len(item.ModelDownUntil) != 0 || len(item.ModelBackoff) != 0 || item.BackoffLevel != 0 {
						t.Fatalf("request error penalized account %s", item.ID)
					}
				}
			}
		})
	}
}
