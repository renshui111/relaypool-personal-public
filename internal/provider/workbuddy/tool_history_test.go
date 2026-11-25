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

func TestInvalidMessagesStopAtFirstAccount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		stream bool
		status int
		code   string
	}{
		{"numeric", `{"error":{"code":11148,"message":"tool calls and tool results do not match, please start a new conversation and retry"}}`, true, 502, "11148"},
		{"nested-code-only", `{"error":{"data":{"code":"11148"}}}`, false, 502, "11148"},
		{"message-only", `{"error":{"message":"Tool calls and tool results do not match"}}`, false, 502, ""},
		{"parse-400", `{"error":{"code":11101,"message":"Parse message failed: unsupported content type at index 0: input_text"}}`, false, 400, "11101"},
		{"parse-502", `{"error":{"data":{"code":"11101","msg":"Parse message failed: unsupported content type at index 0: input_text"}}}`, true, 502, "11101"},
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
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
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
				if failure.Attempts != 1 || failure.LastAccountID == "" || failure.LastProvider != Family {
					t.Fatalf("missing attempt attribution: %+v", failure)
				}
				if tc.code != "" && failure.Code != tc.code {
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

func TestUnrelatedWBErrorsKeepOriginalClassification(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":11101,"message":"Service unavailable"}}`,
		`{"error":{"code":"12345","message":"Parse message failed: unsupported content type"}}`,
	} {
		failure := classifyHTTP(502, "", body)
		if failure.Status != 502 || failure.KindHint == pool.KindInvalidRequest {
			t.Fatalf("unexpected classification: %+v", failure)
		}
	}
}
