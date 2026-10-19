package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

// maxSSELine bounds one upstream SSE line. Reasoning models emit long deltas,
// so this is generous, while still bounded: an unbounded scanner buffer on a
// remote stream is a memory-exhaustion vector.
const maxSSELine = 16 << 20

// sseFrame is one parsed upstream SSE event.
type sseFrame struct {
	// Raw is the original JSON payload, forwarded to the protocol adapter so
	// it can render without re-serializing twice.
	Raw []byte
	// Parsed is the decoded frame, for identity and usage extraction.
	Parsed map[string]any

	ID      string
	Model   string
	Created int64
	Usage   *provider.Usage
	// FinishReason is the upstream's own stop reason, when this frame carries
	// one. It is captured so the terminal frame can report truncation
	// faithfully instead of assuming the model finished normally.
	FinishReason string

	// IsDone marks the terminal [DONE] sentinel.
	IsDone bool
}

// sseFrameReader yields parsed SSE frames from an upstream stream.
//
// It exists so the transport can inspect every frame (to learn the model and
// usage) without the protocol adapters each re-parsing the stream.
type sseFrameReader struct {
	scanner *bufio.Scanner
	done    bool
}

func newSSEFrameReader(reader io.Reader) *sseFrameReader {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELine)
	return &sseFrameReader{scanner: scanner}
}

// Next returns the next frame, or io.EOF at the end.
//
// A nil frame with a nil error means "nothing to forward, keep reading", which
// is how non-data SSE lines (comments, event names) are skipped.
func (r *sseFrameReader) Next() (*sseFrame, error) {
	for r.scanner.Scan() {
		if r.done {
			// The upstream may send trailing frames after [DONE]; ignoring
			// them prevents a duplicate terminal frame downstream.
			continue
		}
		line := strings.TrimSuffix(r.scanner.Text(), "\r")
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// Comments and unknown fields are not part of the payload.
			continue
		}
		payload := bytes.TrimSpace([]byte(strings.TrimPrefix(line, "data:")))
		if len(payload) == 0 {
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			r.done = true
			return &sseFrame{IsDone: true}, nil
		}

		frame := &sseFrame{Raw: payload}
		// UseNumber preserves large integers exactly; float64 would silently
		// round usage counters and timestamps.
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.UseNumber()
		if err := decoder.Decode(&frame.Parsed); err != nil {
			// An unparseable frame still gets forwarded so no data is lost;
			// the protocol adapter passes it through unchanged.
			return frame, nil
		}
		extractFrameMetadata(frame)
		return frame, nil
	}
	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// extractFrameMetadata reads the fields the transport needs off a frame.
func extractFrameMetadata(frame *sseFrame) {
	if frame.Parsed == nil {
		return
	}
	if id, ok := frame.Parsed["id"].(string); ok {
		frame.ID = id
	}
	if model, ok := frame.Parsed["model"].(string); ok {
		frame.Model = model
	}
	switch created := frame.Parsed["created"].(type) {
	case json.Number:
		if v, err := created.Int64(); err == nil {
			frame.Created = v
		}
	case float64:
		frame.Created = int64(created)
	}
	if usage, ok := frame.Parsed["usage"].(map[string]any); ok {
		parsed := parseUsage(usage)
		frame.Usage = &parsed
	}
	// The finish reason lives on the first choice. Any non-empty value is
	// recorded, so a later frame that omits it does not erase an earlier one:
	// the transport keeps the last reason the upstream actually stated.
	for _, raw := range choicesOf(frame.Parsed) {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}
		if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
			frame.FinishReason = reason
		}
	}
}

// choicesOf returns a frame's choices array, tolerating a missing or malformed
// field.
func choicesOf(parsed map[string]any) []any {
	choices, _ := parsed["choices"].([]any)
	return choices
}

// parseUsage reads a usage object, accepting the several naming styles.
func parseUsage(usage map[string]any) provider.Usage {
	out := provider.Usage{}
	out.PromptTokens = intFromMap(usage, "prompt_tokens", "promptTokens", "input_tokens")
	out.CompletionTokens = intFromMap(usage, "completion_tokens", "completionTokens", "output_tokens")
	out.TotalTokens = intFromMap(usage, "total_tokens", "totalTokens")
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	switch credits := usage["credits"].(type) {
	case float64:
		out.Credits = credits
	case json.Number:
		if v, err := credits.Float64(); err == nil {
			out.Credits = v
		}
	}
	return out
}

// 只有实际提供非负整数计数才属于用量上报；空对象和 null 不能伪造零用量。
func hasReportedTokenUsage(parsed map[string]any) bool {
	usage, ok := parsed["usage"].(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"prompt_tokens", "promptTokens", "input_tokens", "completion_tokens", "completionTokens", "output_tokens", "total_tokens", "totalTokens"} {
		switch value := usage[key].(type) {
		case json.Number:
			if count, err := value.Int64(); err == nil && count >= 0 {
				return true
			}
		case float64:
			if value >= 0 && value == float64(int64(value)) {
				return true
			}
		}
	}
	return false
}

func intFromMap(m map[string]any, keys ...string) int {
	for _, key := range keys {
		switch v := m[key].(type) {
		case float64:
			return int(v)
		case json.Number:
			if parsed, err := v.Int64(); err == nil {
				return int(parsed)
			}
		case int:
			return v
		case int64:
			return int(v)
		}
	}
	return 0
}

// frameDelta extracts the first choice's delta object from a frame.
func frameDelta(frame *sseFrame) map[string]any {
	if frame == nil || frame.Parsed == nil {
		return nil
	}
	choices, ok := frame.Parsed["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil
	}
	delta, ok := choice["delta"].(map[string]any)
	if !ok {
		return nil
	}
	return delta
}

// frameFinishReason extracts the first choice's finish_reason.
func frameFinishReason(frame *sseFrame) string {
	if frame == nil || frame.Parsed == nil {
		return ""
	}
	choices, ok := frame.Parsed["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return ""
	}
	reason, _ := choice["finish_reason"].(string)
	return reason
}

// frameText concatenates the text content of a frame's delta.
func frameText(frame *sseFrame) string {
	delta := frameDelta(frame)
	if delta == nil {
		return ""
	}
	text, _ := delta["content"].(string)
	return text
}

// frameReasoning concatenates the reasoning content of a frame's delta.
func frameReasoning(frame *sseFrame) string {
	delta := frameDelta(frame)
	if delta == nil {
		return ""
	}
	text, _ := delta["reasoning_content"].(string)
	return text
}

// frameToolCalls returns the delta's tool_calls array, if present and non-empty.
func frameToolCalls(frame *sseFrame) []any {
	delta := frameDelta(frame)
	if delta == nil {
		return nil
	}
	calls, ok := delta["tool_calls"].([]any)
	if !ok || len(calls) == 0 {
		return nil
	}
	return calls
}

// sseWrite renders one SSE event.
func sseWrite(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+16)
	out = append(out, "data: "...)
	out = append(out, payload...)
	out = append(out, '\n', '\n')
	return out
}

// sseEvent renders a named SSE event, which the Anthropic protocol uses.
func sseEvent(name string, payload []byte) []byte {
	out := make([]byte, 0, len(payload)+len(name)+24)
	out = append(out, "event: "...)
	out = append(out, name...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, payload...)
	out = append(out, '\n', '\n')
	return out
}

// nowUnix is a test seam for the created timestamp.
var nowUnix = func() int64 { return time.Now().Unix() }
