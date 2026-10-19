package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The browser-login flow: state, then poll, then a credential.
//
// These tests drive a FAKE upstream that speaks the documented plugin protocol,
// because the real endpoints require a human to sign in. What they pin is the
// part that decides whether an operator ends up with a usable account: the
// three-step exchange, and the rule that "still waiting" is NOT an error.

// loginUpstream is a scripted auth endpoint.
type loginUpstream struct {
	// stateStatus/stateCode control POST /v2/plugin/auth/state.
	stateStatus int
	stateCode   int
	// tokenStatus/tokenCode control GET /v2/plugin/auth/token.
	tokenStatus int
	tokenCode   int
	// tokenData is the data object the token endpoint returns.
	tokenData string
	// stateCalls/tokenCalls count the requests received.
	stateCalls int
	tokenCalls int
}

func (u *loginUpstream) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/plugin/auth/state", func(w http.ResponseWriter, r *http.Request) {
		u.stateCalls++
		if r.Method != http.MethodPost {
			t.Errorf("auth state must be POST, got %s", r.Method)
		}
		w.WriteHeader(u.statusOr(u.stateStatus))
		payload := map[string]any{
			"code": u.stateCode,
			"msg":  "ok",
			"data": map[string]any{"state": "st_test", "authUrl": "https://example.test/auth?state=st_test"},
		}
		_ = json.NewEncoder(w).Encode(payload)
	})
	mux.HandleFunc("/v2/plugin/auth/token", func(w http.ResponseWriter, r *http.Request) {
		u.tokenCalls++
		if got := r.URL.Query().Get("state"); got != "st_test" {
			t.Errorf("token poll sent state %q, want st_test", got)
		}
		w.WriteHeader(u.statusOr(u.tokenStatus))
		var data any
		if u.tokenData != "" {
			_ = json.Unmarshal([]byte(u.tokenData), &data)
		}
		payload := map[string]any{"code": u.tokenCode, "msg": "ok", "data": data}
		_ = json.NewEncoder(w).Encode(payload)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// statusOr defaults an unset status to 200.
//
// The zero value would call WriteHeader(0), which PANICS inside net/http and
// aborts the connection -- so the test sees a bare EOF and looks like an
// upstream outage instead of a fixture mistake.
func (u *loginUpstream) statusOr(status int) int {
	if status == 0 {
		return http.StatusOK
	}
	return status
}

// newLoginClient builds a client pointed at the fake upstream.
//
// The base URL is injected by overriding the package constants' consumer: the
// client builds URLs from ChatBaseCN/ChatBaseGlobal, so a test cannot simply
// pass a URL. Instead the transport is redirected, which keeps the production
// URL-building code under test.
func newLoginClient(t *testing.T, target string) *Client {
	t.Helper()
	client := New(Config{Timeout: 5 * time.Second})
	client.http = &http.Client{
		Transport: redirectTransport{target: target},
	}
	return client
}

// redirectTransport rewrites every request to the fake server, preserving the
// path and query so the URL construction still runs for real.
type redirectTransport struct{ target string }

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	parsed, err := url.Parse(rt.target)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	// BOTH fields matter: leaving the scheme as https makes the client attempt a
	// TLS handshake against a plain HTTP server, which fails with EOF before the
	// handler runs and looks like an upstream outage instead of a test bug.
	clone.URL.Scheme = parsed.Scheme
	clone.URL.Host = parsed.Host
	// The Host header must follow, or a virtual-hosted server may not match.
	clone.Host = ""
	return http.DefaultTransport.RoundTrip(clone)
}

// StartLogin must return the upstream's URL and remember the state.
func TestStartLoginReturnsTheAuthURL(t *testing.T) {
	up := &loginUpstream{stateStatus: 200}
	srv := up.server(t)
	client := newLoginClient(t, srv.URL)

	authURL, state, err := client.StartLogin(context.Background(), "acc_1", false)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if authURL == "" {
		t.Error("no authorization URL was returned, so the operator has nothing to open")
	}
	if !strings.Contains(authURL, "example.test") {
		t.Errorf("auth URL %q does not look like the upstream's", authURL)
	}
	if state != "st_test" {
		t.Errorf("state = %q, want st_test", state)
	}
	if up.stateCalls != 1 {
		t.Errorf("auth state called %d times, want 1", up.stateCalls)
	}
}

// An upstream refusal must be an error, not a silent empty URL: otherwise the
// console shows a button that opens about:blank.
func TestStartLoginSurfacesAnUpstreamRefusal(t *testing.T) {
	up := &loginUpstream{stateStatus: 200, stateCode: 40001}
	srv := up.server(t)
	client := newLoginClient(t, srv.URL)

	if _, _, err := client.StartLogin(context.Background(), "acc_1", false); err == nil {
		t.Error("a non-zero upstream code must be reported as an error")
	}
}

// While the user is still signing in, the upstream returns a non-zero code.
// That is the NORMAL pending state and must not look like a failure, or the
// console would show an error while the operator is still in their browser.
func TestPollLoginReportsPendingWithoutError(t *testing.T) {
	up := &loginUpstream{tokenStatus: 200, tokenCode: 40002}
	srv := up.server(t)
	client := newLoginClient(t, srv.URL)

	if _, _, err := client.StartLogin(context.Background(), "acc_1", false); err != nil {
		t.Fatalf("setup: %v", err)
	}

	cred, done, err := client.PollLogin(context.Background(), "acc_1", false)
	if err != nil {
		t.Errorf("a pending login must not be an error, got %v", err)
	}
	if done {
		t.Error("done must be false while the upstream still refuses the exchange")
	}
	if cred.AccessToken != "" {
		t.Error("no credential should be produced while pending")
	}
}

// A successful exchange must produce a complete credential: a token without a
// UID is unusable, because the upstream rejects chat requests that lack it.
func TestPollLoginProducesACompleteCredential(t *testing.T) {
	up := &loginUpstream{
		tokenStatus: 200,
		tokenData:   `{"accessToken":"at_1","refreshToken":"rt_1","uid":"u_1","domain":"codebuddy.cn","expiresIn":3600}`,
	}
	srv := up.server(t)
	client := newLoginClient(t, srv.URL)

	if _, _, err := client.StartLogin(context.Background(), "acc_1", false); err != nil {
		t.Fatalf("setup: %v", err)
	}

	cred, done, err := client.PollLogin(context.Background(), "acc_1", false)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if !done {
		t.Fatal("done must be true once tokens are returned")
	}
	if cred.AccessToken != "at_1" || cred.RefreshToken != "rt_1" || cred.UID != "u_1" {
		t.Errorf("credential is incomplete: %+v", cred)
	}
	if !cred.Ready() {
		t.Errorf("the produced credential is not usable: %+v", cred)
	}
}

// A token without a UID must stay PENDING rather than being stored. Storing it
// would create an account that fails every request it ever receives.
func TestPollLoginDoesNotStoreATokenWithoutUID(t *testing.T) {
	up := &loginUpstream{
		tokenStatus: 200,
		tokenData:   `{"accessToken":"at_1","refreshToken":"rt_1"}`,
	}
	srv := up.server(t)
	client := newLoginClient(t, srv.URL)

	if _, _, err := client.StartLogin(context.Background(), "acc_1", false); err != nil {
		t.Fatalf("setup: %v", err)
	}

	cred, done, err := client.PollLogin(context.Background(), "acc_1", false)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if done {
		t.Error("a token with no UID must not be reported as a completed login; " +
			"the account would exist but fail every request")
	}
	if cred.Ready() {
		t.Errorf("an incomplete credential must not be returned as usable: %+v", cred)
	}
}

// Polling an account with no login in progress is a programming error and must
// say so, rather than silently reporting "pending" forever.
func TestPollLoginWithoutStartIsAnError(t *testing.T) {
	up := &loginUpstream{}
	srv := up.server(t)
	client := newLoginClient(t, srv.URL)

	if _, _, err := client.PollLogin(context.Background(), "acc_never_started", false); err == nil {
		t.Error("polling without starting must be an error")
	}
}

// The state is single-use: after a successful exchange the same state must not
// be exchangeable again, or one sign-in could mint two credentials.
func TestLoginStateIsSingleUse(t *testing.T) {
	up := &loginUpstream{
		tokenStatus: 200,
		tokenData:   `{"accessToken":"at_1","refreshToken":"rt_1","uid":"u_1"}`,
	}
	srv := up.server(t)
	client := newLoginClient(t, srv.URL)

	if _, _, err := client.StartLogin(context.Background(), "acc_1", false); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, done, err := client.PollLogin(context.Background(), "acc_1", false); err != nil || !done {
		t.Fatalf("first poll: done=%v err=%v", done, err)
	}

	if _, _, err := client.PollLogin(context.Background(), "acc_1", false); err == nil {
		t.Error("a second poll after success must be refused: the state is single-use")
	}
}

// Cancelling must forget the state so an abandoned attempt cannot complete later.
func TestCancelLoginForgetsTheState(t *testing.T) {
	up := &loginUpstream{}
	srv := up.server(t)
	client := newLoginClient(t, srv.URL)

	if _, _, err := client.StartLogin(context.Background(), "acc_1", false); err != nil {
		t.Fatalf("setup: %v", err)
	}
	client.CancelLogin("acc_1")

	if _, _, err := client.PollLogin(context.Background(), "acc_1", false); err == nil {
		t.Error("after cancelling, polling must be refused")
	}
}
