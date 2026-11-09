package workbuddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

func TestRefreshRetainsRotationExpiryAndAccountIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, body, refresh string
		expiry              int64
	}{
		{"camel", `{"accessToken":"new-access","refreshToken":"new-refresh","expiresAt":1900003600000}`, "new-refresh", 1900003600},
		{"snake", `{"access_token":"new-access","refresh_token":"new-refresh","created_at":1900000000000,"expires_in":3600}`, "new-refresh", 1900003600},
		{"envelope", `{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","createdAt":1900000000,"expiresIn":3600}}`, "new-refresh", 1900003600},
		{"unrotated", `{"accessToken":"new-access","expires_at":1900003600,"uid":"unrelated","domain":"workbuddy.ai"}`, "old-refresh", 1900003600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(Config{})
			c.http.Transport = checkinBusinessTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != pathTokenRefresh || r.Header.Get("X-Refresh-Token") != "old-refresh" {
					t.Fatal("wrong refresh route")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			cred := provider.Credential{AccountID: "account", Raw: []byte(`{"access_token":"old-access","refresh_token":"old-refresh","uid":"original","domain":"codebuddy.cn","enterprise_id":"enterprise","expires_at":1}`)}
			got, err := c.Refresh(context.Background(), cred)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeCredential(got.Raw)
			if err != nil || decoded.AccessToken != "new-access" || decoded.RefreshToken != tc.refresh || decoded.ExpiresAt != tc.expiry || decoded.UID != "original" || decoded.Domain != "codebuddy.cn" || decoded.EnterpriseID != "enterprise" || got.AccountID != "account" || got.ExpiresAt.Unix() != tc.expiry {
				t.Fatal("refreshed credential lost token, expiry, or account identity")
			}
		})
	}
}

func TestTokenRelativeExpiryUsesReceiptTime(t *testing.T) {
	now := time.Unix(1900000000, 0)
	got, err := decodeTokenData([]byte(`{"accessToken":"new","expires_in":3600}`), now)
	if err != nil || got.ExpiresAt != 1900003600 {
		t.Fatalf("relative expiry: %v", err)
	}
}

func TestRefreshRejectsInvalidRepliesWithoutExposingSecrets(t *testing.T) {
	for _, body := range []string{`null`, `{"code":null,"accessToken":"private-secret"}`, `{"code":12153,"accessToken":"private-secret"}`, `{"code":0,"data":null}`, `{"accessToken":123}`, `{"accessToken":"private-secret","expiresIn":-1}`, `{"accessToken":"private-secret","expires_at":-1}`, `{"accessToken":"private-secret"} trailing`, strings.Repeat("x", (1<<20)+1)} {
		c := New(Config{})
		c.http.Transport = checkinBusinessTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		input := []byte(`{"access_token":"old","refresh_token":"refresh","uid":"user","domain":"codebuddy.cn"}`)
		got, err := c.Refresh(t.Context(), provider.Credential{Raw: input})
		if err == nil || len(got.Raw) != 0 || strings.Contains(err.Error(), "private-secret") || !json.Valid(input) {
			t.Fatal("bad refresh accepted or secrets exposed")
		}
	}
}
