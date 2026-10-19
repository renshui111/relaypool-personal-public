package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/provider"
)

// Two defects found by the third independent audit. Both are "the fix was
// applied to one path and not its twin", so each test asserts the paths AGREE
// rather than checking one in isolation.

// ---- defect A: the terminal frame must report WHY the model stopped ----

// finish_reason "length" means the answer was TRUNCATED. Reporting it as
// end_turn (or tool_use) makes a cut-off reply indistinguishable from a complete
// one, so the client neither retries nor warns.
//
// The non-streaming path always mapped this correctly through
// anthropicStopReason; the streaming path derived stop_reason from the content
// shape instead. This drives the streaming path with each upstream reason and
// requires the documented Anthropic equivalent.
func TestAnthropicStreamReportsTheUpstreamFinishReason(t *testing.T) {
	cases := []struct {
		upstream string
		want     string
	}{
		{"length", "max_tokens"},
		{"stop", "end_turn"},
		{"tool_calls", "tool_use"},
	}

	for _, tc := range cases {
		t.Run(tc.upstream, func(t *testing.T) {
			body := upstreamFrame(`{"role":"assistant","content":"partial answer"}`) +
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"` + tc.upstream + `"}]}` + "\n\n" +
				"data: [DONE]\n\n"
			srv := newFixedStreamServer(t, body)

			events := chatEvents(t, srv.URL)
			if got := stopReasonOf(events); got != tc.want {
				t.Errorf("upstream finish_reason=%q -> client stop_reason=%q, want %q\n%s",
					tc.upstream, got, tc.want, dumpStream(events))
			}
		})
	}
}

// Truncation must be reported as truncation EVEN WHEN a tool call is in flight.
// This is the case the old content-shape heuristic got wrong: it saw a tool
// block and answered "tool_use" regardless of why the model actually stopped.
func TestAnthropicTruncationWithToolCallStillReportsMaxTokens(t *testing.T) {
	body := upstreamFrame(`{"role":"assistant","tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"f","arguments":"{\"a\""}}]}`) +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	srv := newFixedStreamServer(t, body)

	events := chatEvents(t, srv.URL)
	if got := stopReasonOf(events); got != "max_tokens" {
		t.Errorf("a truncated answer with a tool in flight reported stop_reason=%q, want max_tokens; "+
			"the client cannot tell this apart from a completed tool call\n%s", got, dumpStream(events))
	}
}

// When the upstream never states a reason, the fallback must still be sensible:
// a tool call in flight means tool_use, otherwise the turn ended.
func TestAnthropicStopReasonFallbackWhenUpstreamIsSilent(t *testing.T) {
	t.Run("with a tool call", func(t *testing.T) {
		body := upstreamFrame(`{"role":"assistant","tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}`) +
			"data: [DONE]\n\n"
		srv := newFixedStreamServer(t, body)
		if got := stopReasonOf(chatEvents(t, srv.URL)); got != "tool_use" {
			t.Errorf("silent upstream with a tool call -> stop_reason=%q, want tool_use", got)
		}
	})

	t.Run("without a tool call", func(t *testing.T) {
		body := upstreamFrame(`{"role":"assistant","content":"done"}`) + "data: [DONE]\n\n"
		srv := newFixedStreamServer(t, body)
		if got := stopReasonOf(chatEvents(t, srv.URL)); got != "end_turn" {
			t.Errorf("silent upstream with no tool call -> stop_reason=%q, want end_turn", got)
		}
	})
}

// The Responses protocol must not report a truncated answer as completed.
func TestResponsesStreamMarksTruncationIncomplete(t *testing.T) {
	body := upstreamFrame(`{"role":"assistant","content":"cut off mid"}`) +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	srv := newFixedStreamServer(t, body)

	_, events := streamPost(t, srv.URL, "/v1/responses",
		`{"model":"glm-5.3","stream":true,"input":"x"}`)

	status, reason := responsesCompletionStatus(t, events)
	if status != "incomplete" {
		t.Errorf("a truncated answer reported status=%q, want incomplete\n%s", status, dumpStream(events))
	}
	if reason != "max_output_tokens" {
		t.Errorf("incomplete_details.reason=%q, want max_output_tokens", reason)
	}
}

// A normal stop must still be reported as a clean completion.
func TestResponsesStreamNormalStopStaysCompleted(t *testing.T) {
	body := upstreamFrame(`{"role":"assistant","content":"all done"}`) +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	srv := newFixedStreamServer(t, body)

	_, events := streamPost(t, srv.URL, "/v1/responses",
		`{"model":"glm-5.3","stream":true,"input":"x"}`)

	status, _ := responsesCompletionStatus(t, events)
	if status != "completed" {
		t.Errorf("a normal stop reported status=%q, want completed\n%s", status, dumpStream(events))
	}
	for _, e := range events {
		if e.Event == "response.completed" && strings.Contains(e.Data, "incomplete_details") {
			t.Errorf("a normal stop must not carry incomplete_details: %s", e.Data)
		}
	}
}

// ---- defect B: the streaming path must accept every argument shape ----

// The non-streaming path normalised object-shaped arguments but the streaming
// path used a string-only assertion that returned "" -- so an object was
// silently dropped on ONE path while the other handled it. This drives the same
// shape through BOTH paths and requires them to agree.
func TestToolArgumentShapesSurviveBothPaths(t *testing.T) {
	shapes := []struct {
		name        string
		arguments   string // JSON literal for the arguments field
		wantContent string // substring that must appear somewhere in the output
	}{
		{name: "string", arguments: `"{\"city\":\"SF\"}"`, wantContent: `SF`},
		{name: "object", arguments: `{"city":"SF"}`, wantContent: `SF`},
		{name: "number", arguments: `42`, wantContent: `42`},
		{name: "array", arguments: `[1,2]`, wantContent: `1`},
	}

	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			body := upstreamFrame(`{"role":"assistant","tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"f","arguments":`+sh.arguments+`}}]}`) +
				toolCallsFinish

			// Streaming path.
			streamSrv := newFixedStreamServer(t, body)
			streamEvents := chatEvents(t, streamSrv.URL)
			if streamed := toolArguments(streamEvents); !strings.Contains(streamed, sh.wantContent) {
				t.Errorf("STREAMING dropped or mangled an %s argument: got %q, want it to contain %q\n%s",
					sh.name, streamed, sh.wantContent, dumpStream(streamEvents))
			}

			// Buffered path, driven by the SAME upstream frames.
			nonStreamSrv := newFixedStreamServer(t, body)
			raw, _ := streamPost(t, nonStreamSrv.URL, "/v1/messages",
				`{"model":"glm-5.3","stream":false,"messages":[{"role":"user","content":"x"}]}`)
			if !strings.Contains(raw, sh.wantContent) {
				t.Errorf("NON-STREAMING dropped or mangled an %s argument: %q does not contain %q",
					sh.name, raw, sh.wantContent)
			}
		})
	}
}

// A string argument must be streamed VERBATIM: the deltas are concatenated by
// the client into the final JSON, so re-encoding each fragment would corrupt the
// assembly. This pins that the normaliser does not "helpfully" re-encode text.
func TestStreamedStringArgumentsAreNotReEncoded(t *testing.T) {
	fragments := []string{`{"ci`, `ty":`, `"SF"}`}
	var b strings.Builder
	b.WriteString(upstreamFrame(`{"role":"assistant","tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"f","arguments":""}}]}`))
	for _, f := range fragments {
		encoded, _ := json.Marshal(f)
		b.WriteString(upstreamFrame(`{"tool_calls":[{"index":0,"function":{"arguments":` + string(encoded) + `}}]}`))
	}
	b.WriteString(toolCallsFinish)
	srv := newFixedStreamServer(t, b.String())

	got := toolArguments(chatEvents(t, srv.URL))
	want := strings.Join(fragments, "")
	if got != want {
		t.Errorf("streamed arguments = %q, want %q; fragments must be forwarded verbatim, "+
			"not re-encoded individually", got, want)
	}
}

// ---- helpers ----

// foldStreamForTest aggregates a canned upstream SSE body into the buffered
// result shape, so the non-streaming envelope can be driven with the SAME
// upstream frames as the streaming one.
//
// This matters: the object-argument defect existed only because the two paths
// were exercised separately. Comparing them requires both to be reachable.
func foldStreamForTest(body string, _ *provider.ChatRequest) (*provider.ChatResult, error) {
	var text strings.Builder
	var toolCalls []map[string]any
	finishReason := "stop"

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content   string          `json:"content"`
					ToolCalls json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		c := chunk.Choices[0]
		if c.FinishReason != "" {
			finishReason = c.FinishReason
		}
		text.WriteString(c.Delta.Content)
		if len(c.Delta.ToolCalls) > 0 && string(c.Delta.ToolCalls) != "null" {
			var calls []map[string]any
			if json.Unmarshal(c.Delta.ToolCalls, &calls) == nil {
				toolCalls = append(toolCalls, calls...)
			}
		}
	}

	encodedText, _ := json.Marshal(text.String())
	result := &provider.ChatResult{
		ID: "cmpl_fold", Model: "glm-5.3", FinishReason: finishReason,
		Message: provider.Message{Role: "assistant", Content: encodedText},
	}
	if len(toolCalls) > 0 {
		encoded, err := json.Marshal(toolCalls)
		if err != nil {
			return nil, err
		}
		result.Message.ToolCalls = encoded
	}
	return result, nil
}

// responsesCompletionStatus returns the status and incomplete reason from the
// terminal response.completed frame.
func responsesCompletionStatus(t *testing.T, events []streamEvent) (status, reason string) {
	t.Helper()
	for _, e := range events {
		if e.Event != "response.completed" {
			continue
		}
		var payload struct {
			Response struct {
				Status            string `json:"status"`
				IncompleteDetails struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(e.Data), &payload); err != nil {
			t.Fatalf("decode response.completed: %v", err)
		}
		status = payload.Response.Status
		reason = payload.Response.IncompleteDetails.Reason
	}
	return status, reason
}

// stopReasonOf returns the stop_reason from the last message_delta.
func stopReasonOf(events []streamEvent) string {
	reason := ""
	for _, e := range events {
		if e.Event != "message_delta" {
			continue
		}
		var payload struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(e.Data), &payload); err == nil {
			reason = payload.Delta.StopReason
		}
	}
	return reason
}
