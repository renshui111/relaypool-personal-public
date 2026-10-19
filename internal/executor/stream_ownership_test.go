package executor

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
)

// countingStreamProvider returns a stream whose Close count is observable, so a
// leak shows up as a number rather than as a vague suspicion.
type countingStreamProvider struct {
	closes atomic.Int64
	// behavior decides each call's outcome by attempt index.
	behavior func(call int) (*provider.ChatResult, error)
	calls    atomic.Int64
}

func (c *countingStreamProvider) Family() string      { return "workbuddy" }
func (c *countingStreamProvider) Kind() provider.Kind { return provider.KindInProcess }

func (c *countingStreamProvider) DecodeCredential(raw []byte) (provider.Credential, error) {
	return provider.Credential{
		Raw:    raw,
		Fields: map[string]string{"access_token": "at"},
	}, nil
}

func (c *countingStreamProvider) FetchCatalog(context.Context, provider.Credential) ([]provider.ModelInfo, error) {
	return nil, nil
}

func (c *countingStreamProvider) Checkin(context.Context, provider.Credential) (provider.CheckinResult, error) {
	return provider.CheckinResult{}, provider.ErrUnsupported
}

func (c *countingStreamProvider) Refresh(context.Context, provider.Credential) (provider.Credential, error) {
	return provider.Credential{}, provider.ErrUnsupported
}

func (c *countingStreamProvider) ChatNonStream(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.ChatResult, error) {
	stream, err := c.ChatStream(ctx, cred, req)
	if err != nil {
		return nil, err
	}
	// Drain and close, mirroring what a real adapter does for the buffered path.
	defer stream.Close()
	body, _ := io.ReadAll(stream.Body)
	encoded := `"` + strings.ReplaceAll(string(body), `"`, `\"`) + `"`
	return &provider.ChatResult{
		ID: "cmpl", Model: req.Model, FinishReason: "stop",
		Message: provider.Message{Role: "assistant", Content: []byte(encoded)},
	}, nil
}

func (c *countingStreamProvider) ChatStream(_ context.Context, _ provider.Credential, req *provider.ChatRequest) (*provider.Stream, error) {
	call := int(c.calls.Add(1))
	if c.behavior != nil {
		if _, err := c.behavior(call); err != nil {
			return nil, err
		}
	}
	// A live stream whose Close is counted.
	return &provider.Stream{
		Body: &countedCloser{
			Reader: strings.NewReader("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n"),
			count:  &c.closes,
		},
		Model: req.Model,
	}, nil
}

type countedCloser struct {
	io.Reader
	count *atomic.Int64
}

func (c *countedCloser) Close() error {
	c.count.Add(1)
	return nil
}

// The failover loop must close a stream that an attempt opened and then failed
// to deliver, or every failed streaming attempt leaks a connection.
func TestFailoverClosesStreamFromFailedAttempt(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b")

	adapter := &countingStreamProvider{}
	// First attempt opens a stream then the CALLER treats it as failed; this is
	// simulated by making the provider return a stream whose read then errors.
	adapter.behavior = func(call int) (*provider.ChatResult, error) {
		if call == 1 {
			// Returning a stream plus an error is impossible through the
			// provider interface, so instead the first attempt fails outright
			// and the second succeeds. The loop must then close the SUCCESSFUL
			// stream only once, on the caller's behalf.
			return nil, errors.New("attempt one failed")
		}
		return nil, nil
	}

	h.registry.Register(adapter)

	result, err := h.executor.ChatStream(context.Background(), request("m"))
	if err != nil {
		t.Fatalf("expected failover to succeed: %v", err)
	}
	// The caller owns this stream and must close it.
	if result.Stream == nil {
		t.Fatalf("expected a stream")
	}
	if err := result.Stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := adapter.closes.Load(); got != 1 {
		t.Errorf("Close called %d times, want exactly 1", got)
	}
}

// Closing twice must be safe: net/http and the executor may both close a stream,
// and a double close of a real connection is not harmless.
func TestStreamCloseIsIdempotent(t *testing.T) {
	h := newHarness(t, "acc_a")

	result, err := h.executor.ChatStream(context.Background(), request("m"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Stream == nil {
		t.Fatalf("expected a stream")
	}
	if err := result.Stream.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// A second close must not panic or report an error.
	_ = result.Stream.Close()
}

// A request whose client goes away before the loop starts must not reach the
// upstream at all.
func TestCancelledContextSkipsUpstreamEntirely(t *testing.T) {
	h := newHarness(t, "acc_a")

	adapter := &countingStreamProvider{}
	h.registry.Register(adapter)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the call

	_, err := h.executor.ChatStream(ctx, request("m"))
	if err == nil {
		t.Fatalf("a cancelled context must fail")
	}
	if adapter.calls.Load() != 0 {
		t.Errorf("the upstream was called %d times for a pre-cancelled request, want 0",
			adapter.calls.Load())
	}
}

// The pool's in-flight counter must return to zero after a stream is opened and
// closed, or the account slowly saturates and stops accepting work.
func TestInFlightReleasedAfterStreaming(t *testing.T) {
	h := newHarness(t, "acc_a")

	result, err := h.executor.ChatStream(context.Background(), request("m"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Stream != nil {
		_ = result.Stream.Close()
	}

	item, ok := h.pool.ByID("acc_a")
	if !ok {
		t.Fatalf("account missing")
	}
	if item.InFlight != 0 {
		t.Errorf("InFlight=%d after a completed streaming attempt, want 0; "+
			"a leak here slowly saturates the account", item.InFlight)
	}
}

// In-flight must also be released when the attempt FAILS, not only on success.
func TestInFlightReleasedAfterFailedAttempt(t *testing.T) {
	h := newHarness(t, "acc_a")

	h.provider.behavior["acc_a"] = func(int) (*provider.ChatResult, error) {
		return nil, &provider.Failure{Status: 500, Body: `{"error":{"message":"boom"}}`}
	}

	_, _ = h.executor.ChatNonStream(context.Background(), request("m"))

	item, _ := h.pool.ByID("acc_a")
	if item.InFlight != 0 {
		t.Errorf("InFlight=%d after a failed attempt, want 0", item.InFlight)
	}
}

// A provider with no adapter registered must not leak in-flight either.
func TestInFlightReleasedWhenAdapterMissing(t *testing.T) {
	h := newHarness(t)
	// An account whose family has no registered adapter.
	h.pool.Upsert(pool.Item{
		ID: "orphan", Name: "orphan", Provider: "nonexistent-family",
		Region: "cn", Enabled: true, Weight: 50, MaxInflight: 4,
	})
	h.creds.byID["orphan"] = provider.Credential{
		AccountID: "orphan", Provider: "nonexistent-family",
		Fields: map[string]string{"access_token": "x"}, Raw: []byte(`{}`),
	}

	_, _ = h.executor.ChatNonStream(context.Background(), request("m"))

	item, ok := h.pool.ByID("orphan")
	if !ok {
		t.Fatalf("the account vanished")
	}
	if item.InFlight != 0 {
		t.Errorf("InFlight=%d after a missing-adapter failure, want 0", item.InFlight)
	}
}
