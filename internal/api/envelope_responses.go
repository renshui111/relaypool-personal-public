package api

import (
	"bytes"
	"encoding/json"

	"github.com/relaypool/relaypool/internal/executor"
	"github.com/relaypool/relaypool/internal/provider"
)

// responsesEnvelope renders the OpenAI Responses shape.
//
// This is the newest of the three protocols and differs structurally from Chat
// Completions: the output is a list of typed items rather than choices, and the
// streaming events are named and numbered. It is also the only one that
// distinguishes "output text" aggregation from per-item deltas.
type responsesEnvelope struct{}

func (responsesEnvelope) contentType() string {
	return "application/json; charset=utf-8"
}

// parseResponsesRequest converts a Responses request into the neutral form.
func parseResponsesRequest(raw []byte) (*provider.ChatRequest, error) {
	var wire struct {
		Model string `json:"model"`
		// Input may be a plain string or a list of typed items.
		Input  json.RawMessage `json:"input"`
		Stream bool            `json:"stream"`

		// Instructions is the system prompt in this protocol.
		Instructions string `json:"instructions"`

		MaxOutputTokens *int     `json:"max_output_tokens"`
		Temperature     *float64 `json:"temperature"`
		TopP            *float64 `json:"top_p"`

		Tools      json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}

	req := &provider.ChatRequest{
		Model:       wire.Model,
		Stream:      wire.Stream,
		Temperature: wire.Temperature,
		TopP:        wire.TopP,
		MaxTokens:   wire.MaxOutputTokens,
		Raw:         raw,
		NativeInput: wire.Input,
		ToolChoice:  wire.ToolChoice,
	}

	if wire.Instructions != "" {
		encoded, _ := json.Marshal(wire.Instructions)
		req.Messages = append(req.Messages, provider.Message{
			Role: "system", Content: encoded,
		})
	}
	req.Messages = append(req.Messages, parseResponsesInput(wire.Input)...)

	if len(wire.Tools) > 0 {
		// Responses declares tools in a flatter shape than Chat Completions;
		// normalize to the nested form the upstream accepts.
		req.Tools = normalizeResponsesTools(wire.Tools)
	}
	return req, nil
}

// parseResponsesInput flattens the input field into messages.
func parseResponsesInput(raw json.RawMessage) []provider.Message {
	if len(raw) == 0 {
		return nil
	}

	// A bare string is the whole user turn.
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		encoded, _ := json.Marshal(text)
		return []provider.Message{{Role: "user", Content: encoded}}
	}

	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		// output_text variant
		Text string `json:"text"`
		// function_call variant
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Output    json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}

	var out []provider.Message
	for _, item := range items {
		switch item.Type {
		case "function_call":
			// A tool invocation from a previous turn.
			calls := []any{map[string]any{
				"id": item.CallID, "type": "function",
				"function": map[string]any{"name": item.Name, "arguments": item.Arguments},
			}}
			encoded, _ := json.Marshal(calls)
			out = append(out, provider.Message{
				Role: "assistant", Content: json.RawMessage(`""`), ToolCalls: encoded,
			})
		case "function_call_output":
			// A tool result.
			encoded, _ := json.Marshal(item.Arguments)
			if len(item.Output) > 0 {
				encoded = item.Output
			}
			out = append(out, provider.Message{
				Role: "tool", Content: encoded, ToolCallID: item.CallID,
			})
		case "reasoning":
			// 原生加密推理项留在 NativeInput；不要伪装成空的用户消息。
			continue
		default:
			role := item.Role
			if role == "" {
				role = "user"
			}
			content := item.Content
			if len(content) == 0 && item.Text != "" {
				content, _ = json.Marshal(item.Text)
			}
			out = append(out, provider.Message{Role: role, Content: flattenResponsesContent(content)})
		}
	}
	return out
}

// 将 Responses 内容转换为中立文本或图文数组，保留图片地址及细节参数。
func flattenResponsesContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`""`)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return raw
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return raw
	}
	joined := ""
	parts := make([]map[string]any, 0, len(blocks))
	structured := false
	for _, block := range blocks {
		kind, _ := block["type"].(string)
		if kind == "input_text" || kind == "output_text" || kind == "text" || kind == "" {
			text, _ := block["text"].(string)
			joined += text
			parts = append(parts, map[string]any{"type": "text", "text": text})
		} else if kind == "input_image" {
			structured = true
			image := map[string]any{"url": block["image_url"]}
			if detail, ok := block["detail"]; ok {
				image["detail"] = detail
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": image})
		} else {
			structured = true
			parts = append(parts, block)
		}
	}
	if structured {
		encoded, _ := json.Marshal(parts)
		return encoded
	}
	encoded, err := json.Marshal(joined)
	if err != nil {
		return raw
	}
	return encoded
}

// normalizeResponsesTools converts flat tool declarations to the nested shape.
func normalizeResponsesTools(raw json.RawMessage) json.RawMessage {
	var tools []map[string]any
	if err := json.Unmarshal(raw, &tools); err != nil {
		return raw
	}
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		if _, nested := tool["function"]; nested {
			out = append(out, tool)
			continue
		}
		if kind, _ := tool["type"].(string); kind != "" && kind != "function" {
			out = append(out, tool)
			continue
		}
		// Flat form: {type:"function", name:..., parameters:...}
		fn := map[string]any{}
		if name, ok := tool["name"]; ok {
			fn["name"] = name
		}
		if params, ok := tool["parameters"]; ok {
			fn["parameters"] = params
		}
		if desc, ok := tool["description"]; ok {
			fn["description"] = desc
		}
		if strict, ok := tool["strict"]; ok {
			fn["strict"] = strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return encoded
}

// renderNonStream builds a Responses object.
func (responsesEnvelope) renderNonStream(result *executor.Result, requestedModel string) ([]byte, error) {
	if result == nil || result.Chat == nil {
		return marshalJSON(map[string]any{
			"object": "response", "status": "completed", "output": []any{},
		})
	}
	chat := result.Chat

	var output []any
	if len(chat.Message.Content) > 0 {
		var text string
		if err := json.Unmarshal(chat.Message.Content, &text); err == nil && text != "" {
			output = append(output, map[string]any{
				"type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text}},
			})
		}
	}
	// Tool calls are their own output items in this protocol.
	if len(chat.Message.ToolCalls) > 0 {
		var calls []struct {
			ID       string `json:"id"`
			Function struct {
				Name string `json:"name"`
				// RawMessage rather than string: this protocol carries arguments
				// as a STRING, so a string field is what it wants, but decoding
				// strictly means an upstream that sends an object loses every
				// tool call in the message. Accepting either shape and
				// normalising is the only form that cannot silently drop calls.
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		}
		if err := json.Unmarshal(chat.Message.ToolCalls, &calls); err == nil {
			for _, call := range calls {
				output = append(output, map[string]any{
					"type": "function_call", "call_id": call.ID,
					"name": call.Function.Name, "arguments": argumentsString(call.Function.Arguments),
				})
			}
		}
	}

	if len(chat.NativeOutput) > 0 {
		_ = json.Unmarshal(chat.NativeOutput, &output)
	}
	status := "completed"
	if chat.FinishReason == "length" {
		status = "incomplete"
	}
	return marshalJSON(map[string]any{
		"id":         orDefault(chat.ID, "resp_"+resultID(result)),
		"object":     "response",
		"created_at": orDefaultInt64(chat.Created, nowUnix()),
		"status":     status,
		"model":      orDefault(chat.Model, requestedModel),
		"output":     output,
		"usage": map[string]any{
			"input_tokens":  chat.Usage.PromptTokens,
			"output_tokens": chat.Usage.CompletionTokens,
			"total_tokens":  chat.Usage.TotalTokens,
		},
	})
}

// renderStreamStart emits response.created.
func (responsesEnvelope) renderStreamStart(state *streamState) []byte {
	created, err := marshalJSON(map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": state.ResponseID, "object": "response",
			"created_at": state.Created, "status": "in_progress",
			"model": state.Model, "output": []any{},
		},
	})
	if err != nil {
		return nil
	}
	return sseEvent("response.created", created)
}

// renderStreamChunk translates one upstream frame into Responses events.
func (responsesEnvelope) renderStreamChunk(raw []byte, state *streamState) ([]byte, bool) {
	frame := &sseFrame{Raw: raw}
	if err := json.Unmarshal(raw, &frame.Parsed); err != nil {
		return nil, false
	}

	var out []byte

	text := frameText(frame)
	reasoning := frameReasoning(frame)
	// The text item must be opened before any delta references it.
	if text != "" || reasoning != "" {
		if state.openTextBlock() {
			start, err := marshalJSON(map[string]any{
				"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{
					"type": "message", "role": "assistant", "status": "in_progress",
					"content": []any{},
				},
			})
			if err == nil {
				out = append(out, sseEvent("response.output_item.added", start)...)
			}
			part, err := marshalJSON(map[string]any{
				"type": "response.content_part.added", "item_id": state.ResponseID,
				"output_index": 0, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": ""},
			})
			if err == nil {
				out = append(out, sseEvent("response.content_part.added", part)...)
			}
			state.SentRole = true
		}
		if text != "" {
			delta, err := marshalJSON(map[string]any{
				"type": "response.output_text.delta", "item_id": state.ResponseID,
				"output_index": 0, "content_index": 0, "delta": text,
			})
			if err == nil {
				out = append(out, sseEvent("response.output_text.delta", delta)...)
			}
		}
		if reasoning != "" {
			delta, err := marshalJSON(map[string]any{
				"type":    "response.reasoning_summary_text.delta",
				"item_id": state.ResponseID, "output_index": 0,
				"summary_index": 0, "delta": reasoning,
			})
			if err == nil {
				out = append(out, sseEvent("response.reasoning_summary_text.delta", delta)...)
			}
		}
	}

	// Tool calls become their own output items, mirroring what renderNonStream
	// emits as function_call items in the buffered response.
	//
	// This branch was MISSING, and that made the streaming path silently drop
	// every tool call while the non-streaming path handled them correctly: a
	// client received "response.completed" with no function_call item and no
	// error, so an agent lost the call while believing the turn succeeded. Both
	// paths now share the streamState.Tools bookkeeping, which is what keeps
	// them from diverging again.
	for _, call := range frameToolCalls(frame) {
		itemIndex, first := state.Tools.openTool(toolIndex(call))
		if itemIndex < 0 {
			continue
		}
		if first {
			start, err := marshalJSON(map[string]any{
				"type": "response.output_item.added", "output_index": itemIndex,
				"item": map[string]any{
					"type":      "function_call",
					"call_id":   toolCallID(call),
					"name":      toolCallName(call),
					"arguments": "",
					"status":    "in_progress",
				},
			})
			if err == nil {
				out = append(out, sseEvent("response.output_item.added", start)...)
			}
		}
		// Arguments are forwarded on the opening frame too: it may already
		// carry part of the JSON, and dropping it would truncate the call.
		//
		// toolArgumentsDelta accepts every shape; the string-only helper it
		// replaced returned "" for an object, which silently dropped the
		// arguments here while the non-streaming path handled them.
		if args := toolArgumentsDelta(call); args != "" {
			delta, err := marshalJSON(map[string]any{
				"type":         "response.function_call_arguments.delta",
				"item_id":      state.ResponseID,
				"output_index": itemIndex,
				"delta":        args,
			})
			if err == nil {
				out = append(out, sseEvent("response.function_call_arguments.delta", delta)...)
			}
		}
	}

	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// argumentsString renders tool arguments as the STRING this protocol expects,
// whatever shape they arrived in.
//
// The Responses protocol types `arguments` as a string, so the output is the raw
// argument text rather than a re-serialised value:
//
//   - a JSON string (the normal case) -> unwrapped, so the client receives the
//     argument text itself rather than a doubly-encoded one
//   - a JSON object or array         -> serialised, so an object-shaped upstream
//     is not emitted where a string is required
//   - anything else (a bare word, a
//     truncated fragment, empty)     -> passed through as text
//
// The last case is intentional and deliberately makes no validity claim: the
// field IS a string, so "not json" is a legal value, and forwarding it preserves
// what the upstream actually sent. The guarantee this function provides is that
// no shape is DROPPED; it is not that every value parses as JSON.
func argumentsString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "{}"
	}
	if trimmed[0] == '"' {
		// Already a JSON string: unwrap it so the value is the raw argument
		// text, not a doubly-encoded one.
		var inner string
		if err := json.Unmarshal(trimmed, &inner); err == nil {
			return inner
		}
	}
	if json.Valid(trimmed) {
		return string(trimmed)
	}
	// Not valid JSON: forward the text unchanged. Wrapping it would alter what
	// the upstream sent, and this protocol's field is a string regardless.
	return string(trimmed)
}

// renderStreamEnd closes the item and the response.
func (responsesEnvelope) renderStreamEnd(state *streamState) []byte {
	var out []byte

	// The text item is closed only if it was opened. A stream that carried only
	// tool calls never opened it, so closing it unconditionally would emit a
	// done event for an item the client never saw.
	if state.TextBlockOpen {
		done, err := marshalJSON(map[string]any{
			"type": "response.output_text.done", "item_id": state.ResponseID,
			"output_index": 0, "content_index": 0, "text": "",
		})
		if err == nil {
			out = append(out, sseEvent("response.output_text.done", done)...)
		}
	}

	// Close every tool item that was opened. An output item that is never
	// marked done leaves an agent waiting for a call that will not complete.
	for _, upstreamIndex := range state.Tools.openedUpstreamIndices() {
		itemIndex := state.Tools.blockFor(upstreamIndex)
		if itemIndex < 0 {
			continue
		}
		itemDone, err := marshalJSON(map[string]any{
			"type": "response.output_item.done", "output_index": itemIndex,
			"item": map[string]any{
				"type": "function_call", "status": "completed",
			},
		})
		if err == nil {
			out = append(out, sseEvent("response.output_item.done", itemDone)...)
		}
	}

	// A truncated answer must not be reported as a clean completion.
	//
	// The status is derived from the upstream's finish reason: "length" means
	// the model was cut off mid-answer, which this protocol expresses as an
	// incomplete response. Reporting "completed" unconditionally hid that, so a
	// client would treat a cut-off reply as finished and never retry -- the same
	// silent behaviour the Anthropic path was fixed for. The incomplete_reason
	// field is what a client uses to decide whether to continue.
	status := "completed"
	incompleteReason := ""
	if state.FinishReason == "length" {
		status = "incomplete"
		incompleteReason = "max_output_tokens"
	}
	response := map[string]any{
		"id": state.ResponseID, "object": "response",
		"created_at": state.Created, "status": status,
		"model": state.Model,
		"usage": map[string]any{
			"input_tokens":  state.Usage.PromptTokens,
			"output_tokens": state.Usage.CompletionTokens,
			"total_tokens":  state.Usage.TotalTokens,
		},
	}
	if incompleteReason != "" {
		response["incomplete_details"] = map[string]any{"reason": incompleteReason}
	}

	completed, err := marshalJSON(map[string]any{
		"type":     "response.completed",
		"response": response,
	})
	if err == nil {
		out = append(out, sseEvent("response.completed", completed)...)
	}
	state.Finished = true
	return out
}

func (responsesEnvelope) renderError(status int, code, message string) []byte {
	body, err := marshalJSON(map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
			"type":    "invalid_request_error",
		},
	})
	if err != nil {
		return []byte(`{"error":{"message":"internal error","type":"invalid_request_error"}}`)
	}
	return body
}

// ensure the unused import is meaningful: provider is used by the parse
// helpers above via provider.Message.
var _ = provider.Message{}
