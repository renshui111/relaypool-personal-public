package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
)

// ---- test doubles ----

// fakeProvider is a scripted adapter. Each account can be given a per-call
// outcome so a test can model "account A fails twice then succeeds".
type fakeProvider struct {
	family string
	kind   provider.Kind

	mu       sync.Mutex
	calls    []string
	behavior map[string]func(callIndex int) (*provider.ChatResult, error)
	// defaultBehavior applies to accounts without a specific script.
	defaultBehavior func(accountID string, callIndex int) (*provider.ChatResult, error)
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		family:   "workbuddy",
		kind:     provider.KindInProcess,
		behavior: map[string]func(callIndex int) (*provider.ChatResult, error){},
	}
}

func (f *fakeProvider) Family() string      { return f.family }
func (f *fakeProvider) Kind() provider.Kind { return f.kind }

func (f *fakeProvider) DecodeCredential(raw []byte) (provider.Credential, error) {
	return provider.Credential{Raw: raw}, nil
}

func (f *fakeProvider) FetchCatalog(context.Context, provider.Credential) ([]provider.ModelInfo, error) {
	return nil, nil
}

func (f *fakeProvider) Checkin(context.Context, provider.Credential) (provider.CheckinResult, error) {
	return provider.CheckinResult{}, provider.ErrUnsupported
}

func (f *fakeProvider) Refresh(context.Context, provider.Credential) (provider.Credential, error) {
	return provider.Credential{}, provider.ErrUnsupported
}

func (f *fakeProvider) ChatNonStream(_ context.Context, cred provider.Credential, _ *provider.ChatRequest) (*provider.ChatResult, error) {
	accountID := cred.AccountID
	index := f.recordCall(accountID)

	if behavior, ok := f.behavior[accountID]; ok {
		return behavior(index)
	}
	if f.defaultBehavior != nil {
		return f.defaultBehavior(accountID, index)
	}
	return &provider.ChatResult{
		ID:           "cmpl-" + accountID,
		Model:        "test-model",
		FinishReason: "stop",
		Message:      provider.Message{Role: "assistant", Content: json.RawMessage(`"ok"`)},
	}, nil
}

func (f *fakeProvider) ChatStream(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.Stream, error) {
	result, err := f.ChatNonStream(ctx, cred, req)
	if err != nil {
		return nil, err
	}
	body := fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		extractText(result.Message.Content))
	return &provider.Stream{
		Body:        io.NopCloser(strings.NewReader(body)),
		ContentType: "text/event-stream",
		Model:       result.Model,
	}, nil
}

func (f *fakeProvider) recordCall(accountID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, accountID)
	count := 0
	for _, call := range f.calls {
		if call == accountID {
			count++
		}
	}
	return count
}

func (f *fakeProvider) callOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func extractText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return ""
}

// fakeCredentials is a static credential source.
type fakeCredentials struct {
	mu     sync.Mutex
	byID   map[string]provider.Credential
	failOn map[string]error
}

func newFakeCredentials(accountIDs ...string) *fakeCredentials {
	source := &fakeCredentials{
		byID:   map[string]provider.Credential{},
		failOn: map[string]error{},
	}
	for _, id := range accountIDs {
		source.byID[id] = provider.Credential{
			AccountID: id,
			Provider:  "workbuddy",
			Fields:    map[string]string{"access_token": "at_" + id},
			Raw:       []byte(`{"access_token":"at_` + id + `"}`),
		}
	}
	return source
}

func (f *fakeCredentials) CredentialFor(accountID string) (provider.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failOn[accountID]; ok {
		return provider.Credential{}, err
	}
	cred, ok := f.byID[accountID]
	if !ok {
		return provider.Credential{}, fmt.Errorf("no credential for %s", accountID)
	}
	return cred, nil
}

// harness bundles the pieces a test needs.
type harness struct {
	pool       *pool.Pool
	registry   *provider.Registry
	sessions   *SessionAffinity
	creds      *fakeCredentials
	provider   *fakeProvider
	executor   *Executor
	attempts   []AttemptRecord
	attemptsMu sync.Mutex
}

func newHarness(t *testing.T, accountIDs ...string) *harness {
	t.Helper()

	h := &harness{
		pool:     pool.New(pool.StrategyRoundRobin),
		registry: provider.NewRegistry(),
		creds:    newFakeCredentials(accountIDs...),
		provider: newFakeProvider(),
	}
	h.sessions = NewSessionAffinity(time.Minute, 100)
	h.registry.Register(h.provider)

	for _, id := range accountIDs {
		h.pool.Upsert(pool.Item{
			ID: id, Name: id, Provider: "workbuddy", Region: "cn",
			Enabled: true, Weight: 50, MaxInflight: 4,
		})
	}

	h.executor = New(h.pool, h.registry, h.sessions, h.creds, Options{
		MaxAttempts:          4,
		DefaultCooldown:      50 * time.Millisecond,
		MaxRetryAfter:        time.Minute,
		MinRateLimitCooldown: 30 * time.Second,
		BreakerThreshold:     3,
	})
	h.executor.OnAttempt(func(record AttemptRecord) {
		h.attemptsMu.Lock()
		h.attempts = append(h.attempts, record)
		h.attemptsMu.Unlock()
	})
	return h
}

func (h *harness) recordedAttempts() []AttemptRecord {
	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()
	return append([]AttemptRecord(nil), h.attempts...)
}

func request(model string) Request {
	return Request{
		ID:   "req_test",
		Chat: &provider.ChatRequest{Model: model},
	}
}

// ---- happy path ----

func TestSingleAccountSuccess(t *testing.T) {
	h := newHarness(t, "acc_a")

	result, err := h.executor.ChatNonStream(context.Background(), request("m"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.AccountID != "acc_a" {
		t.Errorf("account=%q, want acc_a", result.AccountID)
	}
	if result.AttemptCount != 1 {
		t.Errorf("attempts=%d, want 1", result.AttemptCount)
	}
	if len(h.provider.callOrder()) != 1 {
		t.Errorf("expected exactly one upstream call, got %v", h.provider.callOrder())
	}
}

// ---- failover ----

// A retryable failure must move to the next account and succeed there.
func TestFailoverToHealthyAccount(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b")

	// acc_a always fails with a rate limit (retryable).
	h.provider.behavior["acc_a"] = func(int) (*provider.ChatResult, error) {
		return nil, &provider.Failure{
			Status:   429,
			KindHint: pool.KindRateLimit,
			Body:     `{"error":{"message":"rate limit exceeded"}}`,
		}
	}

	result, err := h.executor.ChatNonStream(context.Background(), request("m"))
	if err != nil {
		t.Fatalf("expected failover to succeed: %v", err)
	}
	if result.AccountID != "acc_b" {
		t.Errorf("account=%q, want acc_b", result.AccountID)
	}
	if result.AttemptCount != 2 {
		t.Errorf("attempts=%d, want 2", result.AttemptCount)
	}
}

// This is the most important assertion in the project.
//
// An invalid_request means the REQUEST is unacceptable. Every account rejects
// it identically, so failing over burns another account's quota for nothing.
// The executor must stop immediately after the first attempt.
func TestInvalidRequestDoesNotFailover(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b", "acc_c")

	h.provider.behavior["acc_a"] = func(int) (*provider.ChatResult, error) {
		return nil, &provider.Failure{
			Status: 400,
			Body:   `{"error":{"message":"missing required field: messages","type":"invalid_request_error"}}`,
		}
	}

	_, err := h.executor.ChatNonStream(context.Background(), request("m"))
	if err == nil {
		t.Fatalf("expected an error")
	}

	// Exactly one upstream call: no failover happened.
	calls := h.provider.callOrder()
	if len(calls) != 1 {
		t.Fatalf("invalid_request caused %d upstream calls (%v); it must cause exactly 1",
			len(calls), calls)
	}
	if calls[0] != "acc_a" {
		t.Errorf("call went to %q, want acc_a", calls[0])
	}

	// The failure must surface as a 400, not a 5xx.
	var failure *FailureError
	if !errors.As(err, &failure) {
		t.Fatalf("error is not a FailureError: %T", err)
	}
	if failure.Status != 400 {
		t.Errorf("status=%d, want 400", failure.Status)
	}
	if failure.Kind != pool.KindInvalidRequest {
		t.Errorf("kind=%q, want %q", failure.Kind, pool.KindInvalidRequest)
	}
}

// A quota exhaustion is pool-wide: every account draws on the same allowance,
// so failing over would waste another account's quota.
func TestQuotaDoesNotFailover(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b", "acc_c")

	h.provider.behavior["acc_a"] = func(int) (*provider.ChatResult, error) {
		return nil, &provider.Failure{
			Status:   429,
			KindHint: pool.KindQuota,
			Body:     `{"error":{"message":"insufficient_quota"}}`,
		}
	}

	_, err := h.executor.ChatNonStream(context.Background(), request("m"))
	if err == nil {
		t.Fatalf("expected an error")
	}
	calls := h.provider.callOrder()
	if len(calls) != 1 {
		t.Fatalf("quota caused %d upstream calls (%v); it must cause exactly 1", len(calls), calls)
	}
}

// model_not_available SHOULD fail over (another account may carry the model)
// but must NOT cool the account down.
func TestModelNotAvailableFailsOverWithoutCooling(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b")

	h.provider.behavior["acc_a"] = func(int) (*provider.ChatResult, error) {
		return nil, &provider.Failure{
			Status: 400,
			Body:   `{"error":{"message":"model not found","code":"model_not_found"}}`,
		}
	}

	result, err := h.executor.ChatNonStream(context.Background(), request("m"))
	if err != nil {
		t.Fatalf("expected failover to succeed: %v", err)
	}
	if result.AccountID != "acc_b" {
		t.Errorf("account=%q, want acc_b", result.AccountID)
	}

	// acc_a must still be routable for other models: it is healthy.
	item, _ := h.pool.ByID("acc_a")
	if !item.DownUntil.IsZero() {
		t.Errorf("model_not_available cooled the whole account, which it must not do")
	}
}

// A client that disconnects must not cause more upstream calls.
func TestClientCancellationStopsFailover(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b", "acc_c")

	ctx, cancel := context.WithCancel(context.Background())

	h.provider.behavior["acc_a"] = func(int) (*provider.ChatResult, error) {
		// The client goes away while the first attempt is in flight.
		cancel()
		return nil, errors.New("connection reset")
	}

	_, err := h.executor.ChatNonStream(ctx, request("m"))
	if err == nil {
		t.Fatalf("expected an error")
	}
	calls := h.provider.callOrder()
	if len(calls) != 1 {
		t.Fatalf("a cancelled request caused %d upstream calls (%v); it must cause exactly 1",
			len(calls), calls)
	}
	// The account must NOT be cooled for a client-side cancellation.
	item, _ := h.pool.ByID("acc_a")
	if !item.DownUntil.IsZero() {
		t.Errorf("a client cancellation cooled the account, which it must not do")
	}
}

// Every account failing must surface the last failure, not a generic error.
func TestAllAccountsFailReturnsClassifiedError(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b")

	for _, id := range []string{"acc_a", "acc_b"} {
		accountID := id
		h.provider.behavior[accountID] = func(int) (*provider.ChatResult, error) {
			return nil, &provider.Failure{
				Status:   503,
				KindHint: pool.KindNotReady,
				Body:     `{"error":{"message":"service temporarily unavailable"}}`,
			}
		}
	}

	_, err := h.executor.ChatNonStream(context.Background(), request("m"))
	if err == nil {
		t.Fatalf("expected an error")
	}
	var failure *FailureError
	if !errors.As(err, &failure) {
		t.Fatalf("error is not a FailureError: %T", err)
	}
	if failure.Status != 503 {
		t.Errorf("status=%d, want 503", failure.Status)
	}
	if len(h.provider.callOrder()) != 2 {
		t.Errorf("expected both accounts to be tried, got %v", h.provider.callOrder())
	}
}

// An account must never be tried twice within one request.
func TestAccountIsNotRetriedWithinARequest(t *testing.T) {
	h := newHarness(t, "acc_a")

	h.provider.behavior["acc_a"] = func(int) (*provider.ChatResult, error) {
		return nil, &provider.Failure{Status: 500, Body: `{"error":{"message":"boom"}}`}
	}

	_, _ = h.executor.ChatNonStream(context.Background(), request("m"))

	calls := h.provider.callOrder()
	if len(calls) != 1 {
		t.Fatalf("one account was called %d times (%v); a request must not retry an account",
			len(calls), calls)
	}
}

// MaxAttempts must bound the fan-out even when more accounts exist.
func TestMaxAttemptsBoundsFanOut(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b", "acc_c", "acc_d", "acc_e")
	h.executor.opts.MaxAttempts = 2

	h.provider.defaultBehavior = func(string, int) (*provider.ChatResult, error) {
		return nil, &provider.Failure{Status: 500, Body: `{"error":{"message":"boom"}}`}
	}

	_, _ = h.executor.ChatNonStream(context.Background(), request("m"))
	if calls := h.provider.callOrder(); len(calls) > 2 {
		t.Errorf("MaxAttempts=2 but %d calls were made: %v", len(calls), calls)
	}
}

// ---- cooldown effects ----

// A rate-limited account must be skipped by a following request while cooling.
func TestCooledAccountIsSkippedByNextRequest(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b")

	h.provider.behavior["acc_a"] = func(int) (*provider.ChatResult, error) {
		return nil, &provider.Failure{
			Status:   429,
			KindHint: pool.KindRateLimit,
			Body:     `{"error":{"message":"rate limit exceeded"}}`,
		}
	}

	_, _ = h.executor.ChatNonStream(context.Background(), request("m"))
	before := len(h.provider.callOrder())

	// A second request must go straight to acc_b.
	result, err := h.executor.ChatNonStream(context.Background(), request("m"))
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	if result.AccountID != "acc_b" {
		t.Errorf("account=%q, want acc_b (acc_a should be cooling)", result.AccountID)
	}
	if got := len(h.provider.callOrder()) - before; got != 1 {
		t.Errorf("second request made %d calls, want 1 (acc_a should be skipped)", got)
	}
}

// A per-model cooldown must leave the account usable for its other models.
func TestModelCooldownLeavesAccountUsableForOtherModels(t *testing.T) {
	h := newHarness(t, "acc_a")
	h.pool.MergeModels("acc_a", []string{"model-x", "model-y"})

	h.provider.behavior["acc_a"] = func(int) (*provider.ChatResult, error) {
		return nil, &provider.Failure{
			Status:   429,
			KindHint: pool.KindRateLimit,
			Body:     `{"error":{"message":"rate limit exceeded"}}`,
		}
	}

	_, _ = h.executor.ChatNonStream(context.Background(), request("model-x"))

	// model-x is cooling, so this must fail without an upstream call.
	before := len(h.provider.callOrder())
	if _, err := h.executor.ChatNonStream(context.Background(), request("model-x")); err == nil {
		t.Errorf("expected model-x to be unavailable")
	}
	if len(h.provider.callOrder()) != before {
		t.Errorf("a cooling model was retried upstream")
	}

	// model-y on the same account must still work.
	h.provider.behavior["acc_a"] = func(int) (*provider.ChatResult, error) {
		return &provider.ChatResult{
			ID: "ok", Model: "model-y", FinishReason: "stop",
			Message: provider.Message{Role: "assistant", Content: json.RawMessage(`"fine"`)},
		}, nil
	}
	result, err := h.executor.ChatNonStream(context.Background(), request("model-y"))
	if err != nil {
		t.Fatalf("model-y should still be servable: %v", err)
	}
	if result.AccountID != "acc_a" {
		t.Errorf("account=%q, want acc_a", result.AccountID)
	}
}

// ---- session stickiness ----

// The same conversation must land on the same account across requests.
func TestSessionStickinessKeepsAccount(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b")

	req := request("m")
	req.SessionKey = "REMOVED"

	first, err := h.executor.ChatNonStream(context.Background(), req)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}

	// Rotate several more times; stickiness must override round-robin.
	for i := 0; i < 3; i++ {
		result, err := h.executor.ChatNonStream(context.Background(), req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if result.AccountID != first.AccountID {
			t.Fatalf("request %d went to %q, want the sticky %q",
				i, result.AccountID, first.AccountID)
		}
	}
}

// A conversation must move off an account that has become unusable.
func TestSessionEscapesCooledAccount(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b")

	req := request("m")
	req.SessionKey = "REMOVED"

	first, err := h.executor.ChatNonStream(context.Background(), req)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}

	// Break whichever account got picked.
	h.provider.behavior[first.AccountID] = func(int) (*provider.ChatResult, error) {
		return nil, &provider.Failure{
			Status: 401, KindHint: pool.KindAuth,
			Body: `{"error":{"message":"invalid token"}}`,
		}
	}

	result, err := h.executor.ChatNonStream(context.Background(), req)
	if err != nil {
		t.Fatalf("expected escape to another account: %v", err)
	}
	if result.AccountID == first.AccountID {
		t.Errorf("stayed on the broken account %q", first.AccountID)
	}

	// The escape must be recorded: it is the signal that accounts are unstable.
	if stats := h.sessions.Stats(); stats.Escapes == 0 {
		t.Errorf("expected an escape to be recorded")
	}
}

func TestSessionStatsTrackHits(t *testing.T) {
	h := newHarness(t, "acc_a")
	req := request("m")
	req.SessionKey = "REMOVED"

	for i := 0; i < 3; i++ {
		if _, err := h.executor.ChatNonStream(context.Background(), req); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	stats := h.sessions.Stats()
	if stats.Hits == 0 {
		t.Errorf("expected session hits to be counted, got %+v", stats)
	}
	if stats.Bindings != 1 {
		t.Errorf("bindings=%d, want 1", stats.Bindings)
	}
}

// A pinned account from the client must win over normal routing.
func TestPreferAccountWins(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b")

	req := request("m")
	req.PreferAccount = "acc_b"

	for i := 0; i < 3; i++ {
		result, err := h.executor.ChatNonStream(context.Background(), req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if result.AccountID != "acc_b" {
			t.Fatalf("request %d went to %q, want the pinned acc_b", i, result.AccountID)
		}
	}
}

// ---- credential problems ----

// A missing credential is an account problem and must fail over, not abort.
func TestMissingCredentialFailsOver(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b")
	h.creds.failOn["acc_a"] = errors.New("no credential stored")

	result, err := h.executor.ChatNonStream(context.Background(), request("m"))
	if err != nil {
		t.Fatalf("expected failover to acc_b: %v", err)
	}
	if result.AccountID != "acc_b" {
		t.Errorf("account=%q, want acc_b", result.AccountID)
	}
}

// An absent credential source must surface clearly rather than panicking.
func TestNoCredentialSourceIsReported(t *testing.T) {
	h := newHarness(t, "acc_a")
	h.executor.credentials = nil

	if _, err := h.executor.ChatNonStream(context.Background(), request("m")); err == nil {
		t.Fatalf("expected an error when no credential source is configured")
	}
}

// ---- attempt recording ----

// Observers must see one record per attempt, in order, with the failure kind.
func TestAttemptObserversReceiveEveryAttempt(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b")

	h.provider.behavior["acc_a"] = func(int) (*provider.ChatResult, error) {
		return nil, &provider.Failure{
			Status:   429,
			KindHint: pool.KindRateLimit,
			Body:     `{"error":{"message":"rate limit exceeded"}}`,
		}
	}

	if _, err := h.executor.ChatNonStream(context.Background(), request("m")); err != nil {
		t.Fatalf("request failed: %v", err)
	}

	records := h.recordedAttempts()
	if len(records) != 2 {
		t.Fatalf("expected 2 attempt records, got %d: %+v", len(records), records)
	}
	if records[0].Success {
		t.Errorf("the first attempt should be recorded as a failure")
	}
	if records[0].ErrorKind != pool.KindRateLimit {
		t.Errorf("first attempt kind=%q, want %q", records[0].ErrorKind, pool.KindRateLimit)
	}
	if !records[1].Success {
		t.Errorf("the second attempt should be recorded as a success")
	}
	if records[1].AccountID != "acc_b" {
		t.Errorf("second attempt account=%q, want acc_b", records[1].AccountID)
	}
}

// ---- streaming ----

func TestStreamSuccess(t *testing.T) {
	h := newHarness(t, "acc_a")

	result, err := h.executor.ChatStream(context.Background(), request("m"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Stream == nil {
		t.Fatalf("expected a stream")
	}
	defer result.Stream.Close()

	body, err := io.ReadAll(result.Stream.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Errorf("stream body missing [DONE]: %s", body)
	}
	if result.AccountID != "acc_a" {
		t.Errorf("account=%q, want acc_a", result.AccountID)
	}
}

// Failover is still possible when the upstream call itself fails, because
// nothing has been written to the client yet.
func TestStreamFailsOverBeforeFirstByte(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b")

	h.provider.behavior["acc_a"] = func(int) (*provider.ChatResult, error) {
		return nil, &provider.Failure{Status: 503, Body: `{"error":{"message":"unavailable"}}`}
	}

	result, err := h.executor.ChatStream(context.Background(), request("m"))
	if err != nil {
		t.Fatalf("expected failover: %v", err)
	}
	defer result.Stream.Close()
	if result.AccountID != "acc_b" {
		t.Errorf("account=%q, want acc_b", result.AccountID)
	}
}

// ---- no accounts ----

func TestNoAccountsConfigured(t *testing.T) {
	h := newHarness(t)

	_, err := h.executor.ChatNonStream(context.Background(), request("m"))
	if err == nil {
		t.Fatalf("expected an error with no accounts")
	}
	var failure *FailureError
	if !errors.As(err, &failure) {
		t.Fatalf("error is not a FailureError: %T", err)
	}
	if failure.Status < 400 {
		t.Errorf("status=%d, want a 4xx/5xx", failure.Status)
	}
}

// A fully cooling pool must report a retry-after rather than a bare failure.
func TestAllCoolingReportsRetryAfter(t *testing.T) {
	h := newHarness(t, "acc_a")
	h.pool.MarkDown("acc_a", time.Minute, "cooling")

	_, err := h.executor.ChatNonStream(context.Background(), request("m"))
	if err == nil {
		t.Fatalf("expected an error")
	}
	var failure *FailureError
	if !errors.As(err, &failure) {
		t.Fatalf("error is not a FailureError: %T", err)
	}
	if failure.RetryAfter <= 0 {
		t.Errorf("expected a retry-after hint for a cooling pool, got %v", failure.RetryAfter)
	}
}

// A saturated pool must NOT claim a retry deadline: the caller should shed
// load, and a fake deadline would make it wait instead.
func TestAllSaturatedReportsNoRetryAfter(t *testing.T) {
	h := newHarness(t, "acc_a")
	if !h.pool.Acquire("acc_a") {
		t.Fatalf("acquire failed")
	}
	// Fill the single slot.
	h.pool.SetMaxInflight("acc_a", 1)

	_, err := h.executor.ChatNonStream(context.Background(), request("m"))
	if err == nil {
		t.Fatalf("expected an error")
	}
	var failure *FailureError
	if !errors.As(err, &failure) {
		t.Fatalf("error is not a FailureError: %T", err)
	}
	if failure.RetryAfter != 0 {
		t.Errorf("a saturated pool must not promise a retry time, got %v", failure.RetryAfter)
	}
	if failure.Status != 429 {
		t.Errorf("status=%d, want 429", failure.Status)
	}
}

// ---- multi-provider routing ----

func TestProviderFilterRestrictsRouting(t *testing.T) {
	h := newHarness(t, "wb_a")
	// Add a second family sharing the registry.
	other := newFakeProvider()
	other.family = "qoder"
	h.registry.Register(other)
	h.pool.Upsert(pool.Item{
		ID: "qo_a", Name: "qo_a", Provider: "qoder", Region: "cn",
		Enabled: true, Weight: 50, MaxInflight: 4,
	})
	h.creds.byID["qo_a"] = provider.Credential{
		AccountID: "qo_a", Provider: "qoder",
		Fields: map[string]string{"access_token": "at_qo"},
		Raw:    []byte(`{"access_token":"at_qo"}`),
	}

	req := request("m")
	req.ProviderFilter = "qoder"

	result, err := h.executor.ChatNonStream(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.AccountID != "qo_a" {
		t.Errorf("account=%q, want qo_a (provider filter should restrict routing)", result.AccountID)
	}
}

func TestNoAdapterForProviderFamily(t *testing.T) {
	h := newHarness(t)
	// Register an account whose family has no adapter.
	h.pool.Upsert(pool.Item{
		ID: "orphan", Name: "orphan", Provider: "unknown-family", Region: "cn",
		Enabled: true, Weight: 50, MaxInflight: 4,
	})
	h.creds.byID["orphan"] = provider.Credential{
		AccountID: "orphan", Provider: "unknown-family",
		Fields: map[string]string{"access_token": "x"}, Raw: []byte(`{}`),
	}

	_, err := h.executor.ChatNonStream(context.Background(), request("m"))
	if err == nil {
		t.Fatalf("expected an error for an unregistered provider family")
	}
}
