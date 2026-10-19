package api

// Tool-call SHAPES that a real upstream produces and the primary regression file
// (toolcall_stream_test.go) does not cover.
//
// WHY THIS FILE EXISTS: the first independent audit found that the Anthropic
// envelope re-opened a content block on every frame of one tool call, and that
// the Responses envelope dropped tool calls entirely. The fix moved the
// lifecycle into streamState.Tools. The primary tests pin the simple shapes; the
// cases here pin the ones an upstream actually varies:
//
//   - arguments split across many frames (the normal case for long JSON)
//   - two parallel calls INTERLEAVED frame by frame (fragments must not cross)
//   - sparse, out-of-order upstream indexes (5 then 2)
//   - a missing `index` field on the opening frame
//   - empty arguments
//   - a tool call reported with finish_reason "stop"
//
// These were written by the second independent audit and kept because each one
// probes a way the block bookkeeping could still be wrong while the simple cases
// pass.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// ---- helpers --------------------------------------------------------------

// streamEvent is one parsed SSE event.
type streamEvent struct {
	Event string
	Data  string
}

// parseStreamEvents splits an SSE body into events.
func parseStreamEvents(raw string) []streamEvent {
	var out []streamEvent
	for _, block := range strings.Split(raw, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var ev streamEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.Data = strings.TrimPrefix(line, "data: ")
			}
		}
		if ev.Event != "" || ev.Data != "" {
			out = append(out, ev)
		}
	}
	return out
}

// dumpStream renders the events so a failure shows what the client received,
// not only a count. A bare "want 1 got 3" is very hard to act on.
func dumpStream(events []streamEvent) string {
	var b strings.Builder
	for i, e := range events {
		b.WriteString("  [")
		b.WriteString(itoa(i))
		b.WriteString("] ")
		if e.Event != "" {
			b.WriteString(e.Event)
			b.WriteString(" | ")
		}
		b.WriteString(e.Data)
		b.WriteString("\n")
	}
	return b.String()
}

// blockStartStop returns the count of content_block_start / _stop per index.
func blockStartStop(events []streamEvent) (starts, stops map[int]int) {
	starts = map[int]int{}
	stops = map[int]int{}
	for _, e := range events {
		var payload struct {
			Index int `json:"index"`
		}
		switch e.Event {
		case "content_block_start":
			_ = json.Unmarshal([]byte(e.Data), &payload)
			starts[payload.Index]++
		case "content_block_stop":
			_ = json.Unmarshal([]byte(e.Data), &payload)
			stops[payload.Index]++
		}
	}
	return starts, stops
}

// toolArguments returns the concatenated input_json_delta payloads, which is the
// JSON the client will parse for the tool input.
func toolArguments(events []streamEvent) string {
	var b strings.Builder
	for _, e := range events {
		var payload struct {
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if e.Event != "content_block_delta" || json.Unmarshal([]byte(e.Data), &payload) != nil {
			continue
		}
		if payload.Delta.Type == "input_json_delta" {
			b.WriteString(payload.Delta.PartialJSON)
		}
	}
	return b.String()
}

// toolArgumentsByBlock returns arguments per block index, which is how a
// cross-wiring bug between parallel calls is detected.
func toolArgumentsByBlock(events []streamEvent) map[int]string {
	out := map[int]string{}
	for _, e := range events {
		var payload struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if e.Event != "content_block_delta" || json.Unmarshal([]byte(e.Data), &payload) != nil {
			continue
		}
		if payload.Delta.Type == "input_json_delta" {
			out[payload.Index] += payload.Delta.PartialJSON
		}
	}
	return out
}

// upstreamFrame builds one upstream SSE data frame from a delta object literal.
func upstreamFrame(delta string) string {
	return `data: {"id":"cmpl_x","model":"glm-5.3","choices":[{"index":0,"delta":` + delta +
		`,"finish_reason":null}]}` + "\n\n"
}

// toolCallsFinish is the standard tail of an upstream tool-call stream.
const toolCallsFinish = `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
	"data: [DONE]\n\n"

// chatEvents posts an Anthropic streaming request and returns the parsed events.
func chatEvents(t *testing.T, srvURL string) []streamEvent {
	t.Helper()
	raw, _ := streamPost(t, srvURL, "/v1/messages",
		`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"x"}]}`)
	return parseStreamEvents(raw)
}

// streamPost issues a streaming request and returns the raw body.
func streamPost(t *testing.T, srvURL, path, body string) (string, []streamEvent) {
	t.Helper()
	req := authRequest(t, http.MethodPost, srvURL+path, body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s: %v", path, err)
	}
	defer resp.Body.Close()
	rawBytes, _ := io.ReadAll(resp.Body)
	raw := string(rawBytes)
	return raw, parseStreamEvents(raw)
}

// ---- shapes ---------------------------------------------------------------

// Empty arguments must still open and close exactly one block.
func TestToolCallEmptyArguments(t *testing.T) {
	body := upstreamFrame(`{"role":"assistant","tool_calls":[{"index":0,"id":"call_e","type":"function","function":{"name":"noargs","arguments":""}}]}`) +
		upstreamFrame(`{"tool_calls":[{"index":0,"function":{"arguments":""}}]}`) +
		toolCallsFinish
	srv := newFixedStreamServer(t, body)

	starts, stops := blockStartStop(chatEvents(t, srv.URL))
	if starts[1] != 1 || stops[1] != 1 {
		t.Errorf("empty-arg call: start[1]=%d stop[1]=%d, want 1/1", starts[1], stops[1])
	}
}

// Arguments split across many frames must arrive complete and in order: this is
// the normal case for a long tool input, and losing a fragment leaves the client
// with unparseable JSON.
func TestToolCallManyArgumentFragments(t *testing.T) {
	fragments := []string{`{"ci`, `ty":`, `"San `, `Francisco",`, `"unit":"c"}`}
	var b strings.Builder
	b.WriteString(upstreamFrame(`{"role":"assistant","tool_calls":[{"index":0,"id":"call_f","type":"function","function":{"name":"weather","arguments":""}}]}`))
	for _, f := range fragments {
		encoded, _ := json.Marshal(f)
		b.WriteString(upstreamFrame(`{"tool_calls":[{"index":0,"function":{"arguments":` + string(encoded) + `}}]}`))
	}
	b.WriteString(toolCallsFinish)
	srv := newFixedStreamServer(t, b.String())

	events := chatEvents(t, srv.URL)
	if want, got := strings.Join(fragments, ""), toolArguments(events); got != want {
		t.Errorf("fragmented arguments = %q, want %q; a truncated call cannot be parsed", got, want)
	}
	starts, stops := blockStartStop(events)
	if starts[1] != 1 || stops[1] != 1 {
		t.Errorf("fragmented call: start[1]=%d stop[1]=%d, want 1/1", starts[1], stops[1])
	}
}

// Two parallel calls whose fragments INTERLEAVE must not have their arguments
// cross-wired. This is the case a naive "current call" variable gets wrong.
func TestToolCallInterleavedParallelCallsStaySeparate(t *testing.T) {
	body := upstreamFrame(`{"role":"assistant","tool_calls":[`+
		`{"index":0,"id":"c0","type":"function","function":{"name":"alpha","arguments":""}},`+
		`{"index":1,"id":"c1","type":"function","function":{"name":"beta","arguments":""}}]}`) +
		upstreamFrame(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]}`) +
		upstreamFrame(`{"tool_calls":[{"index":1,"function":{"arguments":"{\"b\":"}}]}`) +
		upstreamFrame(`{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}`) +
		upstreamFrame(`{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}`) +
		toolCallsFinish
	srv := newFixedStreamServer(t, body)

	events := chatEvents(t, srv.URL)

	starts, stops := blockStartStop(events)
	if starts[1] != 1 || starts[2] != 1 {
		t.Errorf("interleaved: tool starts = %d at block1, %d at block2, want 1 and 1\n%s",
			starts[1], starts[2], dumpStream(events))
	}
	if stops[1] != 1 || stops[2] != 1 {
		t.Errorf("interleaved: tool stops = %d at block1, %d at block2, want 1 and 1",
			stops[1], stops[2])
	}

	perBlock := toolArgumentsByBlock(events)
	if perBlock[1] != `{"a":1}` {
		t.Errorf("block 1 (upstream index 0) arguments = %q, want {\"a\":1}; fragments crossed between calls",
			perBlock[1])
	}
	if perBlock[2] != `{"b":2}` {
		t.Errorf("block 2 (upstream index 1) arguments = %q, want {\"b\":2}", perBlock[2])
	}
}

// Sparse, out-of-order upstream indexes (5 then 2) must allocate blocks 1 and 2
// in first-seen order, never reuse block 0 (which belongs to text), and stay
// balanced.
func TestToolCallSparseOutOfOrderIndexes(t *testing.T) {
	body := upstreamFrame(`{"role":"assistant","tool_calls":[{"index":5,"id":"c5","type":"function","function":{"name":"five","arguments":""}}]}`) +
		upstreamFrame(`{"tool_calls":[{"index":2,"id":"c2","type":"function","function":{"name":"two","arguments":""}}]}`) +
		upstreamFrame(`{"tool_calls":[{"index":5,"function":{"arguments":"{\"n\":5}"}}]}`) +
		upstreamFrame(`{"tool_calls":[{"index":2,"function":{"arguments":"{\"n\":2}"}}]}`) +
		toolCallsFinish
	srv := newFixedStreamServer(t, body)

	events := chatEvents(t, srv.URL)
	starts, stops := blockStartStop(events)

	if len(starts) != 2 {
		t.Errorf("two sparse calls opened %d block indexes, want 2\n%s", len(starts), dumpStream(events))
	}
	for idx, n := range starts {
		if n != 1 {
			t.Errorf("block %d opened %d times, want 1", idx, n)
		}
		if stops[idx] != 1 {
			t.Errorf("block %d closed %d times, want 1; the stream is unbalanced", idx, stops[idx])
		}
	}
	// Block 0 is the text block and this stream carried no text, so nothing may
	// have touched it.
	if starts[0] != 0 || stops[0] != 0 {
		t.Errorf("tool calls leaked into the text block: start[0]=%d stop[0]=%d", starts[0], stops[0])
	}
}

// Some upstreams omit `index` on the opening frame. toolIndex treats a missing
// index as 0, which must NOT reintroduce the repeated-start defect: the two
// frames are one call, not two.
func TestToolCallMissingIndexFieldIsOneCall(t *testing.T) {
	body := upstreamFrame(`{"role":"assistant","tool_calls":[{"id":"call_ni","type":"function","function":{"name":"noidx","arguments":""}}]}`) +
		upstreamFrame(`{"tool_calls":[{"function":{"arguments":"{\"k\":1}"}}]}`) +
		toolCallsFinish
	srv := newFixedStreamServer(t, body)

	events := chatEvents(t, srv.URL)
	starts, stops := blockStartStop(events)

	if starts[1] != 1 {
		t.Errorf("index-less upstream opened %d tool blocks, want 1\n%s", starts[1], dumpStream(events))
	}
	if stops[1] != 1 {
		t.Errorf("index-less upstream closed block 1 %d times, want 1", stops[1])
	}
	if want, got := `{"k":1}`, toolArguments(events); got != want {
		t.Errorf("index-less arguments = %q, want %q", got, want)
	}
}

// A tool call reported with finish_reason "stop" must still be delivered and
// closed: the block lifecycle depends on what was OPENED, not on the reason the
// upstream gave for finishing.
func TestToolCallWithStopFinishReasonIsStillDelivered(t *testing.T) {
	body := upstreamFrame(`{"role":"assistant","tool_calls":[{"index":0,"id":"c_s","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}}]}`) +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	srv := newFixedStreamServer(t, body)

	events := chatEvents(t, srv.URL)
	starts, stops := blockStartStop(events)

	if starts[1] != 1 || stops[1] != 1 {
		t.Errorf("finish_reason=stop with a tool call: start[1]=%d stop[1]=%d, want 1/1\n%s",
			starts[1], stops[1], dumpStream(events))
	}
	if want, got := `{"x":1}`, toolArguments(events); got != want {
		t.Errorf("arguments = %q, want %q", got, want)
	}
}
