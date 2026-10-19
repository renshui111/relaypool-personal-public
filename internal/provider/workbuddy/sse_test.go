package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"
)

// upstreamPlaceholderFrame reproduces what the upstream actually emits: every
// chunk carries a fixed set of empty placeholder fields.
//
// This is the exact shape that makes OpenAI-compatible clients render a flood
// of blank thinking blocks, so it is the shape the sanitizer must clean.
const upstreamPlaceholderFrame = `{"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"","refusal":"","tool_calls":[],"function_call":{"name":"","arguments":""},"extra_fields":null},"finish_reason":""}]}`

func sse(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// A frame carrying only placeholders must be DROPPED. Forwarding it is exactly
// the blank-chunk flood this sanitizer exists to prevent.
func TestSanitizeDropsPlaceholderOnlyFrame(t *testing.T) {
	var sentRole bool
	_, keep := sanitizeSSEPayload(upstreamPlaceholderFrame, &sentRole)
	if keep {
		t.Fatalf("a placeholder-only frame must be dropped")
	}
}

// A frame with real content must survive, with the empties stripped.
func TestSanitizeKeepsContentAndStripsEmpties(t *testing.T) {
	frame := `{"choices":[{"index":0,"delta":{"content":"hello","reasoning_content":"","refusal":"","tool_calls":[]},"finish_reason":""}]}`
	var sentRole bool
	cleaned, keep := sanitizeSSEPayload(frame, &sentRole)
	if !keep {
		t.Fatalf("a frame with content must be kept")
	}

	var chunk map[string]any
	if err := json.Unmarshal([]byte(cleaned), &chunk); err != nil {
		t.Fatalf("cleaned frame is not valid JSON: %v", err)
	}
	choices := chunk["choices"].([]any)
	delta := choices[0].(map[string]any)["delta"].(map[string]any)

	if delta["content"] != "hello" {
		t.Errorf("content=%v, want hello", delta["content"])
	}
	for _, key := range []string{"reasoning_content", "refusal", "tool_calls", "extra_fields", "function_call"} {
		if _, present := delta[key]; present {
			t.Errorf("placeholder field %q survived sanitizing: %#v", key, delta)
		}
	}
	// An empty finish_reason must also go.
	if _, present := choices[0].(map[string]any)["finish_reason"]; present {
		t.Errorf("empty finish_reason survived: %#v", choices[0])
	}
}

// The role must be sent once. Repeating it makes some clients treat a later
// frame as the start of a new message.
func TestSanitizeSendsRoleOnlyOnce(t *testing.T) {
	var sentRole bool

	first := `{"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`
	cleaned, keep := sanitizeSSEPayload(first, &sentRole)
	if !keep {
		t.Fatalf("first frame should be kept")
	}
	if !strings.Contains(cleaned, `"role"`) {
		t.Errorf("the first frame must carry the role: %s", cleaned)
	}
	if !sentRole {
		t.Errorf("sentRole should now be true")
	}

	second := `{"choices":[{"index":0,"delta":{"role":"assistant","content":"there"}}]}`
	cleaned2, keep2 := sanitizeSSEPayload(second, &sentRole)
	if !keep2 {
		t.Fatalf("second frame should still be kept (it has content)")
	}
	if strings.Contains(cleaned2, `"role"`) {
		t.Errorf("a repeated role must be stripped: %s", cleaned2)
	}
}

// A usage frame has no choices but must survive: it is how token accounting
// reaches the client.
func TestSanitizeKeepsUsageFrame(t *testing.T) {
	frame := `{"id":"c1","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15},"choices":[]}`
	var sentRole bool
	cleaned, keep := sanitizeSSEPayload(frame, &sentRole)
	if !keep {
		t.Fatalf("a usage frame must be kept")
	}
	if !strings.Contains(cleaned, "total_tokens") {
		t.Errorf("usage payload lost: %s", cleaned)
	}
}

// A finish_reason alone is enough to keep a frame: it is the signal that the
// answer ended.
func TestSanitizeKeepsFinishReasonFrame(t *testing.T) {
	frame := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	var sentRole bool
	cleaned, keep := sanitizeSSEPayload(frame, &sentRole)
	if !keep {
		t.Fatalf("a finish_reason frame must be kept")
	}
	if !strings.Contains(cleaned, `"stop"`) {
		t.Errorf("finish_reason lost: %s", cleaned)
	}
}

func TestSanitizePassesThroughDone(t *testing.T) {
	var sentRole bool
	cleaned, keep := sanitizeSSEPayload("[DONE]", &sentRole)
	if !keep || cleaned != "[DONE]" {
		t.Errorf("DONE should pass through, got %q keep=%v", cleaned, keep)
	}
}

// An unparseable frame must be forwarded rather than silently dropped:
// discarding data we do not understand is worse than passing it on.
func TestSanitizePassesThroughUnparseable(t *testing.T) {
	var sentRole bool
	cleaned, keep := sanitizeSSEPayload("{not json", &sentRole)
	if !keep {
		t.Fatalf("an unparseable frame should be forwarded")
	}
	if cleaned != "{not json" {
		t.Errorf("got %q, want it unchanged", cleaned)
	}
}

// ---- aggregation ----

func TestAggregateConcatenatesContent(t *testing.T) {
	stream := sse(
		`{"id":"c1","model":"glm-5.2","created":100,"choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":""}]}`,
		`{"choices":[{"index":0,"delta":{"content":", world"},"finish_reason":""}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	result, err := Aggregate(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	choices := result["choices"].([]map[string]any)
	message := choices[0]["message"].(map[string]any)
	if message["content"] != "Hello, world" {
		t.Errorf("content=%q, want \"Hello, world\"", message["content"])
	}
	if choices[0]["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v, want stop", choices[0]["finish_reason"])
	}
	if result["id"] != "c1" {
		t.Errorf("id=%v, want c1", result["id"])
	}
}

// Tool call arguments arrive as fragments and must be concatenated in order.
// Getting this wrong produces invalid JSON in the tool call, which breaks the
// client's tool loop entirely.
func TestAggregateMergesToolCallFragments(t *testing.T) {
	stream := sse(
		`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":""}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":""}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":""}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	result, err := Aggregate(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	choices := result["choices"].([]map[string]any)
	message := choices[0]["message"].(map[string]any)

	calls, ok := message["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls=%#v, want one call", message["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_1" {
		t.Errorf("id=%v, want call_1", call["id"])
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("name=%v, want get_weather", fn["name"])
	}
	if fn["arguments"] != `{"city":"Paris"}` {
		t.Errorf("arguments=%q, want the concatenated JSON", fn["arguments"])
	}
	if choices[0]["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v, want tool_calls", choices[0]["finish_reason"])
	}
}

// Multiple concurrent tool calls must stay separated by index, and be emitted
// in index order.
func TestAggregateSeparatesMultipleToolCalls(t *testing.T) {
	stream := sse(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"second","arguments":"{}"}}]},"finish_reason":""}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"first","arguments":"{}"}}]},"finish_reason":""}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	result, err := Aggregate(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	choices := result["choices"].([]map[string]any)
	calls := choices[0]["message"].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	// Index order, not arrival order.
	first := calls[0].(map[string]any)
	if first["id"] != "call_a" {
		t.Errorf("calls are not in index order: %#v", calls)
	}
}

func TestAggregatePreservesReasoningContent(t *testing.T) {
	stream := sse(
		`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Let me think"},"finish_reason":""}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Answer"},"finish_reason":""}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	result, err := Aggregate(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	choices := result["choices"].([]map[string]any)
	message := choices[0]["message"].(map[string]any)
	if message["reasoning_content"] != "Let me think" {
		t.Errorf("reasoning_content=%v, want it preserved", message["reasoning_content"])
	}
}

// A stream that ends without [DONE] is truncated. Returning it as success would
// let a client treat a cut-off answer as complete.
func TestAggregateRejectsTruncatedStream(t *testing.T) {
	stream := "data: " + `{"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n"
	if _, err := Aggregate(strings.NewReader(stream)); err == nil {
		t.Fatalf("a stream without [DONE] must be an error")
	}
}

func TestAggregateIgnoresNonDataLines(t *testing.T) {
	stream := ": keep-alive\n\ndata: " +
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}` +
		"\n\ndata: [DONE]\n\n"
	result, err := Aggregate(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	choices := result["choices"].([]map[string]any)
	if choices[0]["message"].(map[string]any)["content"] != "ok" {
		t.Errorf("content lost when a comment line was present")
	}
}

// A malformed chunk must not abort an otherwise good stream.
func TestAggregateSkipsMalformedChunks(t *testing.T) {
	stream := sse(
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"a"},"finish_reason":""}]}`,
		`{broken`,
		`{"choices":[{"index":0,"delta":{"content":"b"},"finish_reason":"stop"}]}`,
	)
	result, err := Aggregate(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("a malformed chunk should not abort the stream: %v", err)
	}
	choices := result["choices"].([]map[string]any)
	if choices[0]["message"].(map[string]any)["content"] != "ab" {
		t.Errorf("content=%v, want ab", choices[0]["message"].(map[string]any)["content"])
	}
}

func TestAggregateFillsMissingIDAndCreated(t *testing.T) {
	stream := sse(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`)
	result, err := Aggregate(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if id, _ := result["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
		t.Errorf("a missing id should be synthesized, got %v", result["id"])
	}
	if created, ok := result["created"].(int64); !ok || created == 0 {
		t.Errorf("created should be filled in, got %#v", result["created"])
	}
}

// ---- end-to-end sanitizing of a realistic upstream stream ----

// The full pipeline: frames full of placeholders plus real deltas.
//
// This is the regression test for the blank-chunk flood that a real deployment
// hit: the upstream sends mostly-empty frames and the client renders each one.
func TestEndToEndSanitizingRemovesBlankFrames(t *testing.T) {
	// Two placeholder-only frames interleaved with three real deltas.
	upstream := sse(
		upstreamPlaceholderFrame,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"The"},"finish_reason":""}]}`,
		upstreamPlaceholderFrame,
		`{"choices":[{"index":0,"delta":{"content":" answer"},"finish_reason":""}]}`,
		upstreamPlaceholderFrame,
		`{"choices":[{"index":0,"delta":{"content":"."},"finish_reason":"stop"}]}`,
	)

	sanitized := collectSanitized(t, upstream)

	// Exactly the three real frames plus [DONE] and the finish frame survive.
	if len(sanitized) != 4 {
		t.Fatalf("expected 4 frames out (3 content + DONE), got %d: %#v", len(sanitized), sanitized)
	}
	for i, frame := range sanitized[:3] {
		if !strings.Contains(frame, `"content"`) {
			t.Errorf("frame %d lost its content: %s", i, frame)
		}
		// No frame may carry an empty content field.
		if strings.Contains(frame, `"content":""`) {
			t.Errorf("frame %d still carries an empty content field: %s", i, frame)
		}
	}
	if sanitized[3] != "[DONE]" {
		t.Errorf("the last frame should be [DONE], got %q", sanitized[3])
	}
}

// The whole point: the sanitized stream must aggregate to the same content as
// the original, so cleaning does not corrupt the answer.
func TestSanitizedStreamAggregatesCorrectly(t *testing.T) {
	upstream := sse(
		upstreamPlaceholderFrame,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"The"},"finish_reason":""}]}`,
		upstreamPlaceholderFrame,
		`{"choices":[{"index":0,"delta":{"content":" answer"},"finish_reason":""}]}`,
		`{"choices":[{"index":0,"delta":{"content":"."},"finish_reason":"stop"}]}`,
	)

	// Re-frame the sanitized frames as SSE for the aggregator.
	var b strings.Builder
	for _, frame := range collectSanitized(t, upstream) {
		b.WriteString("data: ")
		b.WriteString(frame)
		b.WriteString("\n\n")
	}

	result, err := Aggregate(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("aggregate sanitized stream: %v", err)
	}
	choices := result["choices"].([]map[string]any)
	content := choices[0]["message"].(map[string]any)["content"]
	if content != "The answer." {
		t.Errorf("content=%q, want \"The answer.\"", content)
	}
}

// collectSanitized runs the sanitizer over a stream and returns kept frames.
func collectSanitized(t *testing.T, stream string) []string {
	t.Helper()
	var out []string
	var pw strings.Builder
	if err := copySanitizedSSE(strings.NewReader(stream), &pw); err != nil {
		t.Fatalf("copySanitizedSSE: %v", err)
	}
	for _, block := range strings.Split(pw.String(), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		if !strings.HasPrefix(block, "data:") {
			continue
		}
		out = append(out, strings.TrimSpace(strings.TrimPrefix(block, "data:")))
	}
	return out
}
