package huawei

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/compatible"
)

func adapter(family string) *Client {
	return New(family, compatible.Config{Timeout: time.Second, HeaderTimeout: time.Second, IdleTimeout: time.Second})
}
func credentials(t *testing.T, c *Client, base string) provider.Credential {
	t.Helper()
	data := map[string]any{"base_url": base, "access_key": "synthetic-access", "secret_key": "synthetic-secret", "credential_kind": "aksk"}
	if c.family == OfficeAce {
		data = map[string]any{"model_base_url": base, "model_app_key": "synthetic-app-key", "model_app_secret": "synthetic-app-secret"}
	}
	raw, _ := json.Marshal(data)
	cred, err := c.DecodeCredential(raw)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

func TestSigningGolden(t *testing.T) {
	r, _ := http.NewRequest("POST", "https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions", nil)
	for k, v := range map[string]string{"Content-Type": "application/json", "X-Sdk-Date": "20260101T000000Z", "maas_type": "benefit", "model-id": "glm-5.3-flash", "X-Ot-Function": "  agent-general  "} {
		r.Header.Set(k, v)
	}
	cred := provider.Credential{Fields: map[string]string{"access_key": "HSTAAAAAAAAAAAAAAAAAAA", "secret_key": "sk-secret-0123456789abcdef", "security_token": "sts-token-abc123"}}
	if err := sign(r, []byte(`{"model":"glm-5.3-flash","stream":false}`), cred); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(r.Header.Get("Authorization"), "Signature=d74d70f38c5d7e69b73c728740a2c7397a4191737d93f207d1a6f5fce49e99c4") {
		t.Fatal("SDK signature diverged from independent reference vector")
	}
}

func TestCredentialBoundaries(t *testing.T) {
	for _, family := range []string{CodeArts, OfficeAce} {
		c := adapter(family)
		for _, raw := range []string{`null`, `[]`, `{"api_key":"test"}`, `{"model_app_key":"a","model_app_secret":"b","model_base_url":"http://example.com"}`, `{"access_key":"a","secret_key":"b"}`} {
			if _, err := c.DecodeCredential([]byte(raw)); err == nil {
				t.Fatalf("%s accepted %s", family, raw)
			}
		}
	}
	c := adapter(CodeArts)
	cred := credentials(t, c, "https://example.com/api/v2")
	if !cred.ExpiresAt.IsZero() || c.NeedsRefresh(cred) {
		t.Fatal("permanent AK/SK was treated as expiring")
	}
	raw := `{"access_key":"test","secret_key":"secret","security_token":"sts","expires_at":"2020-01-01T00:00:00Z"}`
	cred, err := c.DecodeCredential([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("POST", "https://example.com", nil)
	if c.authenticate(r, cred, nil) == nil {
		t.Fatal("expired STS sent upstream")
	}
	office := adapter(OfficeAce)
	cred, err = office.DecodeCredential([]byte(`{"model_app_key":"a","model_app_secret":"b","model_base_url":"https://example.com","expires_at":"2020-01-01T00:00:00Z"}`))
	if err != nil || !cred.ExpiresAt.IsZero() {
		t.Fatal("OfficeAce STS expiry expired its distinct inference key")
	}
}

const responseStream = "data: {\"id\":\"completion-test\",\"model\":\"user-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"reason\",\"content\":\"你好\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\ndata: [DONE]\n\n"

func TestCatalogAndChatAuthentication(t *testing.T) {
	for _, family := range []string{CodeArts, OfficeAce} {
		t.Run(family, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if family == OfficeAce {
					a, b, ok := r.BasicAuth()
					if !ok || a != "synthetic-app-key" || b != "synthetic-app-secret" {
						t.Error("wrong OfficeAce auth")
					}
				} else {
					if !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 Access=synthetic-access,") {
						t.Error("wrong CodeArts auth")
					}
				}
				if r.Method == "GET" {
					w.Header().Set("Content-Type", "application/json")
					if family == CodeArts {
						if r.URL.Path != "/v1/model/builtin" || r.Header.Get("Agent-Type") != "PromptCenter" {
							t.Error("wrong catalog endpoint")
						}
						io.WriteString(w, `{"builtinModels":[{"model_id":"user-model","enable":true,"display_enabled":true},{"model_id":"hidden","enable":false}]}`)
					} else {
						if r.URL.Path != "/v2/models" {
							t.Error("wrong OfficeAce catalog path")
						}
						io.WriteString(w, `{"data":[{"id":"user-model"}]}`)
					}
					return
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["model"] != "user-model" || body["stream"] != true {
					t.Error("model/stream lost")
				}
				if family == CodeArts && r.Header.Get("model-id") != "user-model" {
					t.Error("model header does not match body")
				}
				if body["tools"] == nil {
					t.Error("tools discarded")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, responseStream)
			}))
			defer server.Close()
			c := adapter(family)
			base := server.URL + "/api/v2"
			if family == OfficeAce {
				base = server.URL
			}
			cred := credentials(t, c, base)
			models, err := c.FetchCatalog(t.Context(), cred)
			if err != nil || len(models) != 1 || models[0].NativeModel != "user-model" {
				t.Fatalf("catalog: %v %v", models, err)
			}
			req := &provider.ChatRequest{Model: "user-model", Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}}, Tools: json.RawMessage(`[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`)}
			result, err := c.ChatNonStream(t.Context(), cred, req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Message.ContentText() != "你好" || result.Message.ReasoningContent != "reason" || result.Usage.TotalTokens != 6 {
				t.Fatal("content/reasoning/usage not preserved")
			}
			stream, err := c.ChatStream(t.Context(), cred, req)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(stream.Body)
			stream.Close()
			if err != nil || !strings.Contains(string(raw), "[DONE]") {
				t.Fatal("stream did not complete")
			}
		})
	}
}

func TestUpstreamErrorsAndRedirects(t *testing.T) {
	c := adapter(OfficeAce)
	for _, scenario := range []string{"http-error", "first-event", "late-event", "truncated", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			var sinkCalls atomic.Int32
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sinkCalls.Add(1) }))
			defer sink.Close()
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "redirect" {
					http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
					return
				}
				if scenario == "http-error" {
					w.WriteHeader(403)
					io.WriteString(w, `{"error":{"code":"ModelArts.81004","message":"synthetic-app-secret"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if scenario == "late-event" || scenario == "truncated" {
					io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"start\"}}]}\n\n")
				}
				if scenario != "truncated" {
					io.WriteString(w, "data: {\"error\":{\"code\":\"ModelArts.81114\",\"message\":\"synthetic-app-secret\"}}\n\n")
				}
			}))
			defer up.Close()
			cred := credentials(t, c, up.URL)
			stream, err := c.ChatStream(t.Context(), cred, &provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}})
			if err == nil {
				_, err = io.ReadAll(stream.Body)
				stream.Close()
			}
			if err == nil {
				t.Fatal("upstream error treated as success")
			}
			if strings.Contains(err.Error(), "synthetic-app-secret") {
				t.Fatal("secret escaped into error")
			}
			if sinkCalls.Load() != 0 {
				t.Fatal("credentials followed redirect")
			}
			if scenario == "http-error" {
				f, ok := provider.AsFailure(err)
				if !ok || f.KindHint != "model_not_available" {
					t.Fatal("model error cooled the entire credential")
				}
			}
		})
	}
}

func TestDPoPAndOAuth(t *testing.T) {
	for _, family := range []string{CodeArts, OfficeAce} {
		t.Run(family, func(t *testing.T) {
			var exchanges atomic.Int32
			var client *Client
			var expectedVerifier string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/claw/auth/state":
					io.WriteString(w, `{"state":"cloud-issued-state"}`)
				case "/v1/claw/auth/code":
					if r.URL.Query().Get("state") != "cloud-issued-state" {
						t.Error("wrong cloud state")
					}
					io.WriteString(w, `{"code":"one-use-code"}`)
				case "/tokens":
					exchanges.Add(1)
					r.ParseForm()
					if r.PostForm.Get("client_id") != client.clientID() {
						t.Error("mixed OAuth client identities")
					}
					if expectedVerifier == "" || r.PostForm.Get("code_verifier") != expectedVerifier {
						t.Error("authorization code lost its session PKCE binding")
					}
					verifyProof(t, r.Header.Get("DPoP"), client.token)
					io.WriteString(w, `{"credentials":{"access_key_id":"refreshed-ak","secret_access_key":"refreshed-sk","security_token":"refreshed-sts","expiration":"2099-01-01T00:00:00Z"},"refresh_token":"rotated-refresh"}`)
				case "/v1/claw/client-permission-validate":
					if !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 ") {
						t.Error("permission validation not signed")
					}
					io.WriteString(w, `{"model_info":{"model_api_url_base":"`+client.control+`","model_auth_info":{"model_app_key":"app-key","model_app_secret":"app-secret"}}}`)
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			client = adapter(family)
			client.control = server.URL
			client.token = server.URL + "/tokens"
			s, err := client.BeginBrowserLogin(t.Context(), provider.BrowserLoginOptions{Region: "cn"})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(s.AuthURL, s.MachineID) {
				t.Fatal("private JWK put in public authorization URL")
			}
			expectedVerifier = s.Verifier
			var raw []byte
			if family == OfficeAce {
				login, _ := url.Parse(s.AuthURL)
				inner, _ := url.Parse(login.Query().Get("service"))
				if inner.Query().Get("code_challenge_method") != "SHA-256" {
					t.Fatal("OfficeAce challenge method incorrect")
				}
				var done bool
				raw, done, err = client.PollBrowserLogin(t.Context(), s)
				if !done {
					t.Fatal("OfficeAce polling not completed")
				}
			} else {
				authorize, _ := url.Parse(s.AuthURL)
				if authorize.Query().Get("state") != "" || len(authorize.Query().Get("ticket_id")) != 64 {
					t.Fatal("CodeArts ticket is not an OAuth state parameter")
				}
				for _, callback := range []string{
					"http://127.0.0.1:12801/oauth/callback?secret=x&redirect=https%3A%2F%2Fauth.huaweicloud.com",
					"http://user@127.0.0.1:12800/oauth/callback?code=x",
					"http://127.0.0.1:12800/oauth/callback?code=x&code=y",
					"http://127.0.0.1:12800/oauth/callback?code=x&ticket_id=other-session",
					"http://127.0.0.1:12800/oauth/callback?code=%ZZ",
				} {
					if _, err := client.CompleteBrowserLogin(t.Context(), s, callback); err == nil {
						t.Fatal("invalid callback accepted")
					}
				}
				if exchanges.Load() != 0 {
					t.Fatal("invalid callback reached token exchange")
				}
				if _, err := client.CompleteBrowserLogin(t.Context(), s, "http://127.0.0.1:12800/oauth/callback?secret=x&redirect=https%3A%2F%2Fevil.example"); err == nil {
					t.Fatal("arbitrary login redirect accepted")
				}
				if _, err := client.CompleteBrowserLogin(t.Context(), s, "http://127.0.0.1:12800/oauth/callback?state=huawei-portal-state&secret=x&redirect=https%3A%2F%2Fauth.huaweicloud.com%2Fauthui%2Flogin.html"); err != nil {
					t.Fatal(err)
				}
				raw, err = client.CompleteBrowserLogin(t.Context(), s, "http://127.0.0.1:12800/oauth/callback?code=one-use-code&state=huawei-issued-state")
			}
			if err != nil {
				t.Fatal(err)
			}
			cred, err := client.DecodeCredential(raw)
			if err != nil || cred.Provider != family {
				t.Fatalf("decoded login %v", err)
			}
			again, done, err := client.PollBrowserLogin(t.Context(), s)
			if err != nil || !done || string(again) != string(raw) || exchanges.Load() != 1 {
				t.Fatal("login re-consumed one-use authorization code")
			}
			s.Raw = nil
			s.ExpiresAt = time.Now().Add(-time.Second)
			if _, _, err := client.PollBrowserLogin(t.Context(), s); err == nil {
				t.Fatal("expired login accepted")
			}
		})
	}
}

func verifyProof(t *testing.T, value, endpoint string) {
	t.Helper()
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		t.Error("invalid DPoP JWS")
		return
	}
	headerRaw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var header struct {
		JWK privateJWK `json:"jwk"`
	}
	if json.Unmarshal(headerRaw, &header) != nil || header.JWK.D != "" {
		t.Error("private scalar included in proof")
		return
	}
	x, _ := base64.RawURLEncoding.DecodeString(header.JWK.X)
	y, _ := base64.RawURLEncoding.DecodeString(header.JWK.Y)
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if len(sig) != 64 {
		t.Error("invalid ES256 signature")
		return
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	curve := elliptic.P256()
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}, sum[:], r, s) || s.Cmp(new(big.Int).Rsh(new(big.Int).Set(curve.Params().N), 1)) > 0 {
		t.Error("invalid or noncanonical DPoP signature")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(payload, &claims)
	if claims["htu"] != endpoint || claims["htm"] != "POST" {
		t.Error("DPoP target mismatch")
	}
}

func TestConcurrentRefreshUsesOneRotatingToken(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(10 * time.Millisecond)
		io.WriteString(w, `{"credentials":{"access_key_id":"new-ak","secret_access_key":"new-sk","security_token":"new-sts","expiration":"2099-01-01T00:00:00Z"},"refresh_token":"new-refresh"}`)
	}))
	defer server.Close()
	c := adapter(CodeArts)
	c.token = server.URL
	jwk, _ := newJWK()
	data := map[string]any{"access_key": "old-ak", "secret_key": "old-sk", "security_token": "old-sts", "expires_at": time.Now().Add(-time.Second).Format(time.RFC3339), "refresh_token": "old-refresh", "code_verifier": "verifier", "dpop_private_jwk": json.RawMessage(jwk), "models": []string{"manual-model"}}
	raw, _ := json.Marshal(data)
	cred, err := c.DecodeCredential(raw)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			updated, err := c.Refresh(context.Background(), cred)
			if err != nil || updated.Field("access_key") != "new-ak" || updated.Field("models") != `["manual-model"]` {
				t.Error("refresh lost account configuration")
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("one-use token exchanged %d times", calls.Load())
	}
}

func TestToolCallFragmentsAndClose(t *testing.T) {
	wire := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-test\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"x\\\":\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, wire)
	}))
	defer up.Close()
	c := adapter(OfficeAce)
	cred := credentials(t, c, up.URL)
	result, err := c.ChatNonStream(t.Context(), cred, &provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Message.ToolCalls), `"arguments":"{\"x\":1}"`) || result.FinishReason != "tool_calls" {
		t.Fatalf("tool fragments lost: %s", result.Message.ToolCalls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.ChatStream(ctx, cred, &provider.ChatRequest{Model: "m"}); err == nil {
		t.Fatal("cancelled request reached upstream")
	}
}
