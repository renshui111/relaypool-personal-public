package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/executor"
	"github.com/relaypool/relaypool/internal/provider"
)

// fixedStreamProvider is a stub adapter that always returns one canned SSE body.
//
// It exists so these tests can push a TOOL-CALL shaped stream through the real
// HTTP handlers and the real executor. The shared stubProvider only emits text,
// and that gap is exactly why the defects below survived three review rounds.
type fixedStreamProvider struct {
	family string
	body   string
}

func (p *fixedStreamProvider) Family() string      { return p.family }
func (p *fixedStreamProvider) Kind() provider.Kind { return provider.KindInProcess }

func (p *fixedStreamProvider) DecodeCredential(raw []byte) (provider.Credential, error) {
	return provider.Credential{Provider: p.family, Raw: raw,
		Fields: map[string]string{"access_token": "at_test"}}, nil
}

func (p *fixedStreamProvider) FetchCatalog(context.Context, provider.Credential) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{NativeModel: "glm-5.3", PublicModel: "glm-5.3"}}, nil
}

func (p *fixedStreamProvider) Checkin(context.Context, provider.Credential) (provider.CheckinResult, error) {
	return provider.CheckinResult{}, provider.ErrUnsupported
}

func (p *fixedStreamProvider) Refresh(context.Context, provider.Credential) (provider.Credential, error) {
	return provider.Credential{}, provider.ErrUnsupported
}

func (p *fixedStreamProvider) ChatStream(_ context.Context, _ provider.Credential, _ *provider.ChatRequest) (*provider.Stream, error) {
	return &provider.Stream{
		Body:        io.NopCloser(strings.NewReader(p.body)),
		ContentType: "text/event-stream",
		Model:       "glm-5.3",
	}, nil
}

func (p *fixedStreamProvider) ChatNonStream(_ context.Context, _ provider.Credential, req *provider.ChatRequest) (*provider.ChatResult, error) {
	// Fold the canned stream into a buffered result, the way a real adapter does
	// for a non-streaming request. Returning ErrUnsupported would make the
	// buffered path untestable, which is how the object-argument defect stayed
	// hidden on one path while the other was fixed.
	return foldStreamForTest(p.body, req)
}

// newFixedStreamServer builds a server whose upstream always returns the given
// SSE body, with one routable account seeded.
func newFixedStreamServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := newTestServerNoAdapter(t)
	adapter := &fixedStreamProvider{family: "workbuddy", body: body}
	server.registry.Register(adapter)
	seedCredential(t, server.store, "acc_test", adapter.family)
	server.pool.Upsert(poolItem("acc_test", adapter.family))
	server.executor = executor.New(server.pool, server.registry, server.sessions, credentialSource{
		store:    server.store,
		registry: server.registry,
	}, executor.Options{
		MaxAttempts:          4,
		DefaultCooldown:      server.cfg.Pool.DefaultCooldown,
		MaxRetryAfter:        server.cfg.Pool.MaxRetryAfter,
		MinRateLimitCooldown: server.cfg.Pool.MinRateLimitCooldown,
		BreakerThreshold:     server.cfg.Pool.BreakerThreshold,
	})
	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// Streamed tool calls must survive the protocol envelopes intact.
//
// WHY THIS FILE EXISTS: the three end-to-end drivers (e2e/features/auto-armor)
// are entirely text-only. No test ever pushed a tool_calls frame through the
// streaming envelopes, so two real defects lived there undetected:
//
//   - the Anthropic envelope opened a NEW content_block_start for every frame of
//     one tool call (three starts, all on index 1, id/name empty after the
//     first) and closed only index 0 at the end;
//   - the Responses envelope had no tool branch in its streaming renderer at
//     all, so tool calls vanished and the client saw a "complete" empty answer.
//
// Each test below pins one of those behaviours.

// toolCallStreamProvider emits an upstream stream shaped like a real streamed
// tool call: an opening frame with id + name + partial arguments, then argument
// fragments, then a finish frame.
//
// buildToolCallFrames renders the upstream SSE body for N streamed tool calls.
//
// The shape mirrors what the WorkBuddy adapter produces: index + id + name on
// the first frame, then index + arguments fragments only.
func buildToolCallFrames(n int) string {
	var b strings.Builder
	// Opening frames: one per call, carrying id and name.
	for i := 0; i < n; i++ {
		b.WriteString(`data: {"id":"cmpl_tc","model":"glm-5.3","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":` +
			itoa(i) + `,"id":"call_` + itoa(i+1) + `","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}` + "\n\n")
	}
	// Argument fragments: the same indices, no id/name.
	for i := 0; i < n; i++ {
		b.WriteString(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":` + itoa(i) +
			`,"function":{"arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]}` + "\n\n")
	}
	// Finish.
	b.WriteString(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// collectAnthropicStream returns every (event, data) pair the gateway emitted.
func collectAnthropicStream(t *testing.T, srv *httptest.Server, path, body string) []struct{ Event, Data string } {
	t.Helper()
	req := authRequest(t, http.MethodPost, srv.URL+path, body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var events []struct{ Event, Data string }
	for _, block := range strings.Split(string(raw), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var ev struct{ Event, Data string }
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				ev.Event = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				ev.Data = strings.TrimPrefix(line, "data: ")
			}
		}
		if ev.Event != "" || ev.Data != "" {
			events = append(events, ev)
		}
	}
	return events
}

// One streamed tool call over many frames must open exactly ONE Anthropic
// content block, and that block must be closed.
func TestAnthropicStreamedToolCallOpensOneBlock(t *testing.T) {
	srv := newFixedStreamServer(t, buildToolCallFrames(1))

	events := collectAnthropicStream(t, srv, "/v1/messages",
		`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"weather?"}]}`)

	starts, stops := 0, map[int]int{}
	startIndex := -1
	for _, e := range events {
		switch e.Event {
		case "content_block_start":
			starts++
			var payload struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			_ = json.Unmarshal([]byte(e.Data), &payload)
			if payload.ContentBlock.Type == "tool_use" {
				startIndex = payload.Index
				if payload.ContentBlock.ID == "" || payload.ContentBlock.Name == "" {
					t.Errorf("the opening frame for a tool_use block must carry id and name, got id=%q name=%q",
						payload.ContentBlock.ID, payload.ContentBlock.Name)
				}
			}
		case "content_block_stop":
			var payload struct {
				Index int `json:"index"`
			}
			_ = json.Unmarshal([]byte(e.Data), &payload)
			stops[payload.Index]++
		}
	}

	// This stream carries no text at all, so exactly ONE block opens (the tool
	// block). More than one means a continuation frame re-opened the call.
	if starts != 1 {
		t.Errorf("content_block_start count = %d, want 1 (the tool block; this stream has no text); "+
			"more means a tool call was re-opened on a continuation frame", starts)
	}
	if startIndex < 0 {
		t.Fatalf("no tool_use content_block_start was emitted at all")
	}
	if startIndex != 1 {
		t.Errorf("the tool block opened at index %d, want 1 (index 0 is reserved for text)", startIndex)
	}
	if stops[startIndex] != 1 {
		t.Errorf("content_block_stop for the tool block at index %d = %d, want exactly 1; "+
			"an unclosed block leaves the client waiting", startIndex, stops[startIndex])
	}
}

// A text stream that then calls a tool must open a text block AND a tool block,
// each exactly once, and close both.
func TestAnthropicTextThenToolCallOpensTwoBlocks(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"cmpl_tt","model":"glm-5.3","choices":[{"index":0,"delta":{"role":"assistant","content":"let me check"},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	srv := newFixedStreamServer(t, body)

	events := collectAnthropicStream(t, srv, "/v1/messages",
		`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"x"}]}`)

	starts, stops := 0, map[int]int{}
	for _, e := range events {
		switch e.Event {
		case "content_block_start":
			starts++
		case "content_block_stop":
			var payload struct {
				Index int `json:"index"`
			}
			_ = json.Unmarshal([]byte(e.Data), &payload)
			stops[payload.Index]++
		}
	}

	// One text block + one tool block.
	if starts != 2 {
		t.Errorf("content_block_start count = %d, want 2 (text + tool)", starts)
	}
	for _, index := range []int{0, 1} {
		if stops[index] != 1 {
			t.Errorf("content_block_stop for index %d = %d, want exactly 1", index, stops[index])
		}
	}
}

// The same must hold for parallel tool calls: one block each, all closed.
func TestAnthropicParallelToolCallsOpenOneBlockEach(t *testing.T) {
	const n = 3
	srv := newFixedStreamServer(t, buildToolCallFrames(n))

	events := collectAnthropicStream(t, srv, "/v1/messages",
		`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"two cities"}]}`)

	starts, stops := 0, map[int]int{}
	for _, e := range events {
		switch e.Event {
		case "content_block_start":
			starts++
		case "content_block_stop":
			var payload struct {
				Index int `json:"index"`
			}
			_ = json.Unmarshal([]byte(e.Data), &payload)
			stops[payload.Index]++
		}
	}

	// n tool blocks, no text block.
	if starts != n {
		t.Errorf("content_block_start count = %d, want %d (%d tool calls, no text); "+
			"a higher number means each continuation frame re-opened a block", starts, n, n)
	}
	// Tool blocks are numbered from 1 (index 0 is the text block), and every
	// opened index must be closed exactly once -- the stream must balance.
	for index := 1; index <= n; index++ {
		if stops[index] != 1 {
			t.Errorf("content_block_stop for index %d = %d, want exactly 1; the stream is unbalanced",
				index, stops[index])
		}
	}
	// No text was streamed, so nothing may have closed index 0.
	if stops[0] != 0 {
		t.Errorf("content_block_stop for the text index 0 = %d, want 0 (no text was streamed)", stops[0])
	}
}

// The Anthropic stop reason must say tool_use when a tool call was emitted, so
// the client knows to execute it rather than treating the turn as finished.
func TestAnthropicStreamedToolCallReportsToolUseStopReason(t *testing.T) {
	srv := newFixedStreamServer(t, buildToolCallFrames(1))

	events := collectAnthropicStream(t, srv, "/v1/messages",
		`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"weather?"}]}`)

	var reason string
	for _, e := range events {
		if e.Event != "message_delta" {
			continue
		}
		var payload struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		_ = json.Unmarshal([]byte(e.Data), &payload)
		reason = payload.Delta.StopReason
	}
	if reason != "tool_use" {
		t.Errorf("stop_reason = %q, want %q; reporting end_turn tells the client the turn "+
			"is over when it should be calling the tool", reason, "tool_use")
	}
}

// The Responses envelope must emit a function_call item for a streamed tool
// call. Before the fix it emitted nothing and reported completion.
func TestResponsesStreamedToolCallEmitsFunctionCallItem(t *testing.T) {
	srv := newFixedStreamServer(t, buildToolCallFrames(1))

	events := collectAnthropicStream(t, srv, "/v1/responses",
		`{"model":"glm-5.3","stream":true,"input":"weather?"}`)

	var added, delta, done int
	for _, e := range events {
		switch e.Event {
		case "response.output_item.added":
			var payload struct {
				Item struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
					Name   string `json:"name"`
				} `json:"item"`
			}
			_ = json.Unmarshal([]byte(e.Data), &payload)
			if payload.Item.Type == "function_call" {
				added++
				if payload.Item.CallID == "" || payload.Item.Name == "" {
					t.Errorf("the function_call item must carry call_id and name, got %q / %q",
						payload.Item.CallID, payload.Item.Name)
				}
			}
		case "response.function_call_arguments.delta":
			delta++
		case "response.output_item.done":
			done++
		}
	}

	if added != 1 {
		t.Errorf("function_call output_item.added count = %d, want 1; "+
			"zero means the tool call was silently dropped from the stream", added)
	}
	if delta == 0 {
		t.Errorf("no function_call_arguments.delta was emitted, so the call has no arguments")
	}
	if done != 1 {
		t.Errorf("function_call output_item.done count = %d, want 1; "+
			"an item that is never done leaves an agent waiting", done)
	}
}

// The text path must keep working after adding the tool branch: a plain text
// stream must not emit any tool events.
func TestStreamedTextStillHasNoToolEvents(t *testing.T) {
	// The default stub provider emits a text-only stream.
	srv, _ := newTestServer(t, newStubProvider())

	for _, tc := range []struct{ path, body string }{
		{"/v1/messages", `{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"glm-5.3","stream":true,"input":"hi"}`},
	} {
		events := collectAnthropicStream(t, srv, tc.path, tc.body)
		for _, e := range events {
			if strings.Contains(e.Event, "function_call") || strings.Contains(e.Data, "function_call") {
				t.Errorf("%s: a text-only stream emitted a tool event %q: %s", tc.path, e.Event, e.Data)
			}
		}
	}
}

// A tool call whose arguments arrive entirely on the opening frame must not lose
// them: the opening frame is where the upstream puts the first fragment.
func TestStreamedToolCallArgumentsOnOpeningFrameAreKept(t *testing.T) {
	// One frame only, carrying id, name AND the complete arguments.
	body := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	srv := newFixedStreamServer(t, body)

	events := collectAnthropicStream(t, srv, "/v1/messages",
		`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"x"}]}`)

	var partials []string
	for _, e := range events {
		if e.Event != "content_block_delta" {
			continue
		}
		var payload struct {
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		_ = json.Unmarshal([]byte(e.Data), &payload)
		if payload.Delta.Type == "input_json_delta" {
			partials = append(partials, payload.Delta.PartialJSON)
		}
	}
	joined := strings.Join(partials, "")
	if !strings.Contains(joined, `"a":1`) {
		t.Errorf("arguments carried on the opening frame were dropped; got %q from %v", joined, partials)
	}
}
