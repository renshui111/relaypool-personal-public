package workbuddy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxSSELine bounds one SSE line. Reasoning models emit long deltas, so this is
// generous, but it is still bounded: an unbounded scanner buffer on a stream
// from a remote host is a memory-exhaustion vector.
const maxSSELine = 16 << 20

// Aggregate consumes an upstream SSE stream and returns one Chat Completions
// object.
//
// This exists because the upstream is always streamed but clients may ask for a
// non-streaming response. Aggregating locally is what lets the gateway present
// both shapes from one upstream code path.
func Aggregate(reader io.Reader) (map[string]any, error) {
	result := map[string]any{"object": "chat.completion"}
	message := map[string]any{"role": "assistant"}

	// tool_calls arrive as fragments keyed by index and must be merged: the
	// arguments string is streamed in pieces, so concatenation in arrival order
	// is the only correct reconstruction.
	toolCalls := map[int]map[string]any{}
	var order []int

	finishReason := "stop"
	var content, reasoning strings.Builder
	sawDone := false

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELine)

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			sawDone = true
			break
		}

		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Created int64  `json:"created"`
			Usage   any    `json:"usage"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Role             string          `json:"role"`
					Content          string          `json:"content"`
					ReasoningContent string          `json:"reasoning_content"`
					ToolCalls        json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// A malformed chunk must not abort an otherwise good stream: some
			// upstreams emit keep-alive comments or partial frames.
			continue
		}

		if chunk.ID != "" {
			result["id"] = chunk.ID
		}
		if chunk.Model != "" {
			result["model"] = chunk.Model
		}
		if chunk.Created != 0 {
			result["created"] = chunk.Created
		}
		if chunk.Usage != nil {
			result["usage"] = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
		}
		if choice.Delta.Role != "" {
			message["role"] = choice.Delta.Role
		}
		content.WriteString(choice.Delta.Content)
		reasoning.WriteString(choice.Delta.ReasoningContent)
		if len(choice.Delta.ToolCalls) > 0 && string(choice.Delta.ToolCalls) != "null" {
			mergeToolCalls(toolCalls, &order, choice.Delta.ToolCalls)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	// A stream that never delivered [DONE] is truncated, not complete.
	// Returning it as a success would let a client treat a cut-off answer as a
	// finished one, so this is an error.
	if !sawDone {
		return nil, fmt.Errorf("workbuddy: stream ended before [DONE]")
	}

	message["content"] = content.String()
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		sort.Ints(order)
		calls := make([]any, 0, len(order))
		for _, index := range order {
			calls = append(calls, toolCalls[index])
		}
		message["tool_calls"] = calls
	}
	if result["id"] == nil {
		result["id"] = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if result["created"] == nil {
		result["created"] = time.Now().Unix()
	}
	result["choices"] = []map[string]any{{
		"index":         0,
		"message":       message,
		"finish_reason": finishReason,
	}}
	return result, nil
}

// ReasoningOnlyError reports that a completion produced thinking but no answer.
//
// WHY THIS IS AN ERROR AND NOT AN EMPTY SUCCESS: a reasoning model given a small
// max_tokens spends the whole budget thinking and emits no content. Returning
// an empty string as a success means the caller sees a 200 with nothing in it,
// has paid for the tokens, and cannot tell whether the model was truncated or
// simply had nothing to say. Surfacing it lets the caller raise the budget and
// retry, or at least report the truth.
type ReasoningOnlyError struct {
	// ReasoningPreview is the beginning of what the model did think, which is
	// the only diagnostic available for why it never answered.
	ReasoningPreview string
	// FinishReason is the upstream's own reason, usually "length".
	FinishReason string
}

func (e *ReasoningOnlyError) Error() string {
	detail := ""
	if e.FinishReason == "length" {
		detail = " (the token budget was consumed by thinking; raise max_tokens)"
	} else if e.FinishReason != "" {
		detail = " (finish_reason=" + e.FinishReason + ")"
	}
	return "workbuddy: the model produced reasoning but no answer" + detail
}

// IsReasoningOnly reports whether an aggregated result has thinking but no
// answer and no tool call.
//
// A tool call counts as a valid answer: a model that decides to call a tool has
// finished its turn even though it produced no prose.
func IsReasoningOnly(aggregated map[string]any) (bool, string, string) {
	choices, _ := aggregated["choices"].([]map[string]any)
	if len(choices) == 0 {
		if generic, ok := aggregated["choices"].([]any); ok && len(generic) > 0 {
			if first, ok := generic[0].(map[string]any); ok {
				choices = []map[string]any{first}
			}
		}
	}
	if len(choices) == 0 {
		return false, "", ""
	}
	choice := choices[0]
	finishReason, _ := choice["finish_reason"].(string)

	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return false, "", finishReason
	}
	content, _ := message["content"].(string)
	if strings.TrimSpace(content) != "" {
		return false, "", finishReason
	}
	// A tool call is a complete turn even without prose.
	if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
		return false, "", finishReason
	}
	reasoning, _ := message["reasoning_content"].(string)
	if strings.TrimSpace(reasoning) == "" {
		// Neither thinking nor answering: an empty reply, but not the
		// reasoning-only case this check exists for.
		return false, "", finishReason
	}

	preview := reasoning
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	return true, preview, finishReason
}

// mergeToolCalls folds one tool_calls delta into the accumulator.
func mergeToolCalls(merged map[int]map[string]any, order *[]int, raw json.RawMessage) {
	var deltas []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &deltas); err != nil {
		return
	}
	for _, delta := range deltas {
		call, exists := merged[delta.Index]
		if !exists {
			call = map[string]any{"index": delta.Index}
			merged[delta.Index] = call
			*order = append(*order, delta.Index)
		}
		if delta.ID != "" {
			call["id"] = delta.ID
		}
		if delta.Type != "" {
			call["type"] = delta.Type
		}
		if delta.Function.Name != "" {
			fn, _ := call["function"].(map[string]any)
			if fn == nil {
				fn = map[string]any{}
				call["function"] = fn
			}
			// The name may itself arrive in fragments.
			existing, _ := fn["name"].(string)
			fn["name"] = existing + delta.Function.Name
		}
		if delta.Function.Arguments != "" {
			fn, _ := call["function"].(map[string]any)
			if fn == nil {
				fn = map[string]any{}
				call["function"] = fn
			}
			arguments, _ := fn["arguments"].(string)
			fn["arguments"] = arguments + delta.Function.Arguments
		}
	}
}

// rewriteChatStream returns a response whose body is a sanitized copy of the
// upstream SSE stream.
//
// WHY THIS IS NECESSARY: the upstream emits every chunk with a fixed set of
// placeholder fields -- content:"", reasoning_content:"", refusal:"",
// tool_calls:[] and a dummy function_call. OpenAI-compatible clients treat a
// present-but-empty content field as a real (empty) token and render a flood of
// blank thinking blocks. Stripping the empties here is the only place that knows
// this is a WorkBuddy quirk rather than a client bug.
//
// # Ownership, which is the subtle part
//
// The sanitizing runs in a goroutine fed through an io.Pipe, so the caller can
// start writing to its own client immediately: buffering the whole stream first
// would defeat streaming entirely.
//
// That goroutine is parked in a read on the UPSTREAM body. Closing only the pipe
// reader does not unblock it, so the goroutine and the upstream connection would
// both leak for every abandoned stream. The returned body therefore CLOSES THE
// UPSTREAM when it is closed, which is what makes the chain:
//
//	caller closes -> pipe reader closes -> copier unblocks -> upstream closes
//
// Without that, a client that disconnects mid-answer leaks a connection and a
// goroutine per abandoned request.
func rewriteChatStream(resp *http.Response) *http.Response {
	pr, pw := io.Pipe()
	go func() {
		// Both closes are deferred so they run even on an early return. The
		// upstream close is what unblocks a read the caller abandoned.
		defer resp.Body.Close()
		defer pw.Close()
		if err := copySanitizedSSE(resp.Body, pw); err != nil {
			// Propagate the failure so the reader sees an error rather than a
			// silently truncated stream.
			_ = pw.CloseWithError(err)
		}
	}()

	out := *resp
	// The returned body closes BOTH the pipe and the upstream. Closing the pipe
	// reader alone would leave the copier blocked on the upstream read.
	out.Body = &sseBody{reader: pr, upstream: resp.Body}
	// The sanitized length is unknown, so the original Content-Length must go:
	// leaving it would make the client wait for bytes that never arrive.
	out.ContentLength = -1
	header := resp.Header.Clone()
	header.Del("Content-Length")
	out.Header = header
	return &out
}

// sseBody ties the pipe reader to the upstream body so one Close releases both.
//
// It also makes Close idempotent, because net/http and the executor may both
// close a stream and a double close of an io.Pipe reader is harmless but a
// double close of a real connection is not.
type sseBody struct {
	reader   *io.PipeReader
	upstream io.Closer

	once   sync.Once
	closed bool
}

func (b *sseBody) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *sseBody) Close() error {
	var err error
	b.once.Do(func() {
		b.closed = true
		// Closing the pipe reader unblocks the copier's write; closing the
		// upstream unblocks its read. Both are needed.
		if closeErr := b.reader.Close(); closeErr != nil {
			err = closeErr
		}
		if b.upstream != nil {
			if closeErr := b.upstream.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
		}
	})
	return err
}

// copySanitizedSSE streams sanitized frames from src to dst.
func copySanitizedSSE(src io.Reader, dst io.Writer) error {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELine)

	// The role field belongs on the first delta only. Repeating it makes some
	// clients treat a later frame as the start of a new message.
	sentRole := false

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// Non-data SSE fields (event:, id:, retry:) pass through verbatim.
			if _, err := fmt.Fprintf(dst, "%s\n", line); err != nil {
				return err
			}
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		cleaned, keep := sanitizeSSEPayload(payload, &sentRole)
		if !keep {
			// The whole frame was placeholders; forwarding it would produce
			// the blank-chunk flood this function exists to prevent.
			continue
		}
		// Emit the canonical "data: <payload>\n\n" framing: the blank line is
		// what terminates an SSE event.
		if _, err := fmt.Fprintf(dst, "data: %s\n\n", cleaned); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// sanitizeSSEPayload strips placeholder fields from one frame.
//
// It reports false when the frame carries nothing worth sending, which happens
// for the many keep-alive frames the upstream interleaves.
func sanitizeSSEPayload(payload string, sentRole *bool) (string, bool) {
	if payload == "[DONE]" {
		return payload, true
	}

	// UseNumber preserves large integers (usage counters, timestamps) exactly.
	// Decoding them as float64 would silently round them.
	decoder := json.NewDecoder(bytes.NewReader([]byte(payload)))
	decoder.UseNumber()
	var chunk map[string]any
	if err := decoder.Decode(&chunk); err != nil {
		// Unparseable frame: pass it through rather than dropping data.
		return payload, true
	}

	// A usage frame must survive even when its choices are empty, because it
	// is how token accounting reaches the client.
	keep := chunk["usage"] != nil

	if choices, ok := chunk["choices"].([]any); ok {
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			if choice == nil {
				continue
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				sanitizeDelta(delta)
				if sentRole != nil {
					if _, hasRole := delta["role"]; hasRole && *sentRole {
						delete(delta, "role")
					}
				}
				if deltaHasContent(delta) {
					keep = true
					if sentRole != nil {
						if _, hasRole := delta["role"]; hasRole {
							*sentRole = true
						}
					}
				}
			}
			switch finish := choice["finish_reason"].(type) {
			case string:
				if finish == "" {
					delete(choice, "finish_reason")
				} else {
					keep = true
				}
			case nil:
				delete(choice, "finish_reason")
			}
		}
	}

	if !keep {
		return "", false
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return payload, true
	}
	return string(encoded), true
}

// deltaHasContent reports whether a delta carries anything a client would act
// on.
//
// WHY NOT len(delta) > 0: the upstream's placeholder frames still contain
// role:"assistant" alongside the empty fields, so a non-empty map is NOT
// evidence of content. Counting only substantive keys is what actually
// distinguishes a real delta from a keep-alive.
//
// role is deliberately excluded: it is metadata about the message, not payload,
// and a role-only frame is exactly the blank chunk this sanitizer must drop.
// (The role is still forwarded when it rides along with real content, which is
// how the first frame of a normal stream carries it.)
func deltaHasContent(delta map[string]any) bool {
	for _, key := range []string{
		"content", "reasoning_content", "refusal",
		"tool_calls", "function_call",
	} {
		if value, present := delta[key]; present && value != nil {
			// A non-empty string, a non-empty array/object, or any scalar.
			switch typed := value.(type) {
			case string:
				if typed != "" {
					return true
				}
			case []any:
				if len(typed) > 0 {
					return true
				}
			case map[string]any:
				if len(typed) > 0 {
					return true
				}
			default:
				return true
			}
		}
	}
	return false
}

// sanitizeDelta removes empty placeholder fields from one delta object.
func sanitizeDelta(delta map[string]any) {
	dropEmptyString(delta, "content")
	dropEmptyString(delta, "reasoning_content")
	dropEmptyString(delta, "refusal")

	if extra, ok := delta["extra_fields"]; ok && extra == nil {
		delete(delta, "extra_fields")
	}
	if calls, ok := delta["tool_calls"].([]any); ok && len(calls) == 0 {
		delete(delta, "tool_calls")
	}
	// A present-but-null tool_calls is the same placeholder as an empty array.
	if value, ok := delta["tool_calls"]; ok && value == nil {
		delete(delta, "tool_calls")
	}
	// The dummy function_call carries neither a name nor arguments.
	switch call := delta["function_call"].(type) {
	case nil:
		delete(delta, "function_call")
	case map[string]any:
		name, _ := call["name"].(string)
		args, _ := call["arguments"].(string)
		if name == "" && args == "" {
			delete(delta, "function_call")
		}
	}
}

func dropEmptyString(delta map[string]any, key string) {
	if value, ok := delta[key].(string); ok && value == "" {
		delete(delta, key)
	}
}
