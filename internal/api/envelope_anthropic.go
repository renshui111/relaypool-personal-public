package api

import (
	"bytes"
	"encoding/json"

	"github.com/relaypool/relaypool/internal/executor"
	"github.com/relaypool/relaypool/internal/provider"
)

// anthropicEnvelope renders the Anthropic Messages shape.
//
// The two protocols differ in more than field names, and the differences are
// the ones that break clients when ignored:
//
//   - The system prompt is a TOP-LEVEL string, not a message role.
//   - Streaming uses named events (message_start, content_block_delta, ...)
//     rather than bare data frames.
//   - Usage is nested under "usage" with input_tokens/output_tokens.
//   - The reply is a list of content blocks, not a single content string.
type anthropicEnvelope struct{}

func (anthropicEnvelope) contentType() string {
	return "application/json; charset=utf-8"
}

// parseAnthropicRequest converts an Anthropic request into the neutral form.
func parseAnthropicRequest(raw []byte) (*provider.ChatRequest, error) {
	var wire struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
		// System may be a string or an array of text blocks.
		System     json.RawMessage   `json:"system"`
		Messages   []json.RawMessage `json:"messages"`
		Tools      json.RawMessage   `json:"tools"`
		ToolChoice json.RawMessage   `json:"tool_choice"`
		// Anthropic names the temperature/top_p fields the same way OpenAI does.
		Temperature *float64        `json:"temperature"`
		TopP        *float64        `json:"top_p"`
		Stop        json.RawMessage `json:"stop_sequences"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}

	req := &provider.ChatRequest{
		Model:       wire.Model,
		Stream:      wire.Stream,
		Temperature: wire.Temperature,
		TopP:        wire.TopP,
		Raw:         raw,
		ToolChoice:  wire.ToolChoice,
	}
	if wire.MaxTokens > 0 {
		maxTokens := wire.MaxTokens
		req.MaxTokens = &maxTokens
	}

	// The system prompt becomes a leading system message, because the upstream
	// speaks the OpenAI shape. This is the translation the namespace exists for.
	if system := anthropicSystemText(wire.System); system != "" {
		encoded, _ := json.Marshal(system)
		req.Messages = append(req.Messages, provider.Message{
			Role:          "system",
			Content:       encoded,
			NativeContent: wire.System,
		})
	}

	for _, rawMessage := range wire.Messages {
		req.Messages = append(req.Messages, parseAnthropicMessages(rawMessage)...)
	}

	if len(wire.Stop) > 0 {
		var many []string
		if err := json.Unmarshal(wire.Stop, &many); err == nil {
			req.Stop = many
		}
	}
	if len(wire.Tools) > 0 {
		req.Tools = wire.Tools
	}
	return req, nil
}

// anthropicSystemText flattens the system field, which may be a string or a
// list of text blocks.
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		out := ""
		for _, block := range blocks {
			if block.Type == "" || block.Type == "text" {
				out += block.Text
			}
		}
		return out
	}
	return ""
}

// parseAnthropicMessage converts one message, flattening content blocks to the
// text-plus-tools shape the upstream understands.
func parseAnthropicMessage(raw json.RawMessage) provider.Message {
	var envelope struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return provider.Message{Content: raw}
	}

	message := provider.Message{
		Role:          envelope.Role,
		Content:       envelope.Content,
		NativeContent: envelope.Content,
	}

	// Anthropic content is always a block list. Text blocks become the content
	// string; tool_use/tool_result blocks become the OpenAI tool fields.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		// tool_use
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
		// tool_result
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(envelope.Content, &blocks); err != nil {
		// A plain string content is already valid for the neutral form.
		return message
	}

	var text string
	var parts []map[string]any
	structured := false
	var toolCalls []any
	for _, block := range blocks {
		switch block.Type {
		case "text", "":
			text += block.Text
		case "tool_use":
			arguments := "{}"
			if len(block.Input) > 0 {
				arguments = string(block.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   block.ID,
				"type": "function",
				"function": map[string]any{
					"name":      block.Name,
					"arguments": arguments,
				},
			})
		case "tool_result":
			// A tool result becomes a tool-role message content.
			if len(block.Content) > 0 {
				text += string(block.Content)
			}
			message.ToolCallID = block.ToolUseID
		}
	}
	// 中立图文数组供 Gemini、OpenAI 和兼容渠道使用，Claude 则使用原生块。
	var original []map[string]any
	_ = json.Unmarshal(envelope.Content, &original)
	for _, block := range original {
		kind, _ := block["type"].(string)
		if kind == "text" || kind == "" {
			parts = append(parts, block)
		}
		if kind == "thinking" {
			value, _ := block["thinking"].(string)
			message.ReasoningContent += value
		}
		if kind == "image" {
			structured = true
			source, _ := block["source"].(map[string]any)
			address, _ := source["url"].(string)
			if source["type"] == "base64" {
				mime, _ := source["media_type"].(string)
				data, _ := source["data"].(string)
				address = "data:" + mime + ";base64," + data
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": address}})
		}
	}

	if encoded, err := json.Marshal(text); err == nil {
		message.Content = encoded
	}
	if structured {
		message.Content, _ = json.Marshal(parts)
	}
	if len(toolCalls) > 0 {
		if encoded, err := json.Marshal(toolCalls); err == nil {
			message.ToolCalls = encoded
		}
	}
	return message
}

// 多个工具结果必须分别保留调用编号，不能拼成一条普通用户消息。
func parseAnthropicMessages(raw json.RawMessage) []provider.Message {
	var wire struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &wire) != nil || wire.Role != "user" {
		return []provider.Message{parseAnthropicMessage(raw)}
	}
	var blocks []json.RawMessage
	if json.Unmarshal(wire.Content, &blocks) != nil {
		return []provider.Message{parseAnthropicMessage(raw)}
	}
	var out []provider.Message
	var pending []json.RawMessage
	flush := func() {
		if len(pending) == 0 {
			return
		}
		content, _ := json.Marshal(pending)
		message, _ := json.Marshal(map[string]any{"role": wire.Role, "content": json.RawMessage(content)})
		out = append(out, parseAnthropicMessage(message))
		pending = nil
	}
	for _, block := range blocks {
		var result struct {
			Type    string          `json:"type"`
			ID      string          `json:"tool_use_id"`
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(block, &result)
		if result.Type != "tool_result" {
			pending = append(pending, block)
			continue
		}
		flush()
		native, _ := json.Marshal([]json.RawMessage{block})
		content := result.Content
		if len(content) == 0 {
			content = json.RawMessage(`""`)
		}
		// 工具结果内的图片与文本也按相同规则转换。
		converted, _ := json.Marshal(map[string]any{"role": "user", "content": content})
		neutral := parseAnthropicMessage(converted)
		out = append(out, provider.Message{Role: "tool", ToolCallID: result.ID, Content: neutral.Content, NativeContent: native})
	}
	flush()
	if len(out) == 0 {
		return []provider.Message{parseAnthropicMessage(raw)}
	}
	return out
}

// renderNonStream builds an Anthropic message object.
func (anthropicEnvelope) renderNonStream(result *executor.Result, requestedModel string) ([]byte, error) {
	if result == nil || result.Chat == nil {
		return marshalJSON(map[string]any{
			"type": "message", "role": "assistant", "content": []any{},
		})
	}
	chat := result.Chat

	content := []any{}
	if len(chat.Message.NativeContent) > 0 {
		_ = json.Unmarshal(chat.Message.NativeContent, &content)
	}
	if len(chat.Message.NativeContent) == 0 && len(chat.Message.Content) > 0 {
		var text string
		if err := json.Unmarshal(chat.Message.Content, &text); err == nil {
			if text != "" {
				content = append(content, map[string]any{"type": "text", "text": text})
			}
		}
	}
	// Tool calls are content blocks in this protocol, not a separate field.
	if len(chat.Message.NativeContent) == 0 && len(chat.Message.ToolCalls) > 0 {
		var calls []struct {
			ID       string `json:"id"`
			Function struct {
				Name string `json:"name"`
				// Arguments is a RawMessage, not a string, so an upstream that
				// sends a JSON OBJECT rather than a string does not silently
				// drop every tool call in the message. Decoding into a string
				// made the whole Unmarshal fail, which discarded them all: a
				// formatting difference in one upstream became missing tool
				// calls with no error anywhere.
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		}
		if err := json.Unmarshal(chat.Message.ToolCalls, &calls); err == nil {
			for _, call := range calls {
				content = append(content, map[string]any{
					"type": "tool_use", "id": call.ID,
					"name":  call.Function.Name,
					"input": toolInputObject(call.Function.Arguments),
				})
			}
		}
	}

	return marshalJSON(map[string]any{
		"id":            orDefault(chat.ID, "msg_"+resultID(result)),
		"type":          "message",
		"role":          "assistant",
		"model":         orDefault(chat.Model, requestedModel),
		"content":       content,
		"stop_reason":   anthropicStopReason(chat.FinishReason),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  chat.Usage.PromptTokens,
			"output_tokens": chat.Usage.CompletionTokens,
		},
	})
}

// anthropicStopReason maps an OpenAI finish reason onto the Anthropic set.
// The vocabularies differ: "tool_calls" becomes "tool_use", and "length"
// becomes "max_tokens".
func anthropicStopReason(finishReason string) string {
	switch finishReason {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop", "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// renderStreamStart emits the message_start preamble.
func (anthropicEnvelope) renderStreamStart(state *streamState) []byte {
	payload, err := marshalJSON(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": state.ResponseID, "type": "message", "role": "assistant",
			"model": state.Model, "content": []any{},
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	if err != nil {
		return nil
	}
	return sseEvent("message_start", payload)
}

// renderStreamChunk translates one upstream frame into Anthropic events.
//
// The mapping is not one-to-one: a single upstream frame may produce a
// content_block_start, a delta, and a content_block_stop, because Anthropic
// brackets each content block explicitly.
func (anthropicEnvelope) renderStreamChunk(raw []byte, state *streamState) ([]byte, bool) {
	frame := &sseFrame{Raw: raw}
	if err := json.Unmarshal(raw, &frame.Parsed); err != nil {
		// An unparseable frame cannot be translated; dropping it is better
		// than emitting a frame the client cannot parse.
		return nil, false
	}

	var out []byte

	// The text block must be opened before the first text delta.
	if text := frameText(frame); text != "" {
		if state.openTextBlock() {
			start, err := marshalJSON(map[string]any{
				"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
			if err == nil {
				out = append(out, sseEvent("content_block_start", start)...)
			}
			state.SentRole = true
		}
		delta, err := marshalJSON(map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
		if err == nil {
			out = append(out, sseEvent("content_block_delta", delta)...)
		}
	}

	// Reasoning is surfaced as a thinking delta, which is how Anthropic
	// clients display it.
	if reasoning := frameReasoning(frame); reasoning != "" {
		delta, err := marshalJSON(map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
		})
		if err == nil {
			out = append(out, sseEvent("content_block_delta", delta)...)
		}
	}

	// Tool calls: the FIRST frame for an upstream tool index opens a block and
	// every later frame for that same index is a delta into it.
	//
	// The upstream streams one tool call over many frames (id+name first, then
	// argument fragments), so opening a block per frame produced three
	// content_block_start events for one call -- all on the same index, with the
	// id and name empty after the first. The lifecycle now comes from
	// streamState, which is the only place that knows which indices are open.
	for _, call := range frameToolCalls(frame) {
		index, first := state.Tools.openTool(toolIndex(call))
		if index < 0 {
			continue
		}
		if first {
			start, err := marshalJSON(map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{
					"type": "tool_use", "id": toolCallID(call), "name": toolCallName(call),
					"input": map[string]any{},
				},
			})
			if err == nil {
				out = append(out, sseEvent("content_block_start", start)...)
			}
		}
		// Arguments are forwarded as input_json_delta on BOTH the opening frame
		// and every continuation: the opening frame may already carry part of
		// the JSON, and dropping it would truncate the call.
		//
		// The value goes through toolArgumentsDelta rather than the old
		// string-only assertion, which returned "" for anything that was not
		// already a string -- so an upstream sending an object silently lost the
		// arguments on the STREAMING path while the non-streaming path handled
		// it. One shape of the fix had been applied to only one of the two paths.
		if args := toolArgumentsDelta(call); args != "" {
			delta, err := marshalJSON(map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			})
			if err == nil {
				out = append(out, sseEvent("content_block_delta", delta)...)
			}
		}
	}

	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// renderStreamEnd closes the blocks and the message.
func (anthropicEnvelope) renderStreamEnd(state *streamState) []byte {
	var out []byte

	// Every block that was opened must be closed, in the order it was opened,
	// and nothing else may be closed.
	//
	// Closing only index 0 (the text block) left tool blocks unterminated, so a
	// client waiting for content_block_stop on its tool_use block hangs or
	// reports a truncated stream. Closing index 0 unconditionally is the mirror
	// error: a stream that carried only tool calls never opened a text block, so
	// a stop for it would reference a block the client never saw.
	//
	// Both conditions are now driven by streamState, which is the only structure
	// that knows what was actually opened.
	if state.TextBlockOpen {
		stop, err := marshalJSON(map[string]any{"type": "content_block_stop", "index": 0})
		if err == nil {
			out = append(out, sseEvent("content_block_stop", stop)...)
		}
	}
	for _, upstreamIndex := range state.Tools.openedUpstreamIndices() {
		block := state.Tools.blockFor(upstreamIndex)
		if block < 0 {
			continue
		}
		toolStop, err := marshalJSON(map[string]any{"type": "content_block_stop", "index": block})
		if err == nil {
			out = append(out, sseEvent("content_block_stop", toolStop)...)
		}
	}

	// The stop reason must reflect WHY the model stopped, using the upstream's
	// own finish reason.
	//
	// Deriving it from the content shape was wrong: a TRUNCATED answer
	// (finish_reason "length") was reported as "end_turn" -- or as "tool_use"
	// when a call happened to be in flight -- so a client could not tell a
	// cut-off response from a complete one, and would neither retry nor warn.
	// The non-streaming path has always used anthropicStopReason; this now
	// matches it exactly.
	//
	// The fallback applies only when the upstream never stated a reason: a tool
	// call in flight is then the best available inference, and otherwise the
	// turn simply ended.
	stopReason := anthropicStopReason(state.FinishReason)
	if state.FinishReason == "" && state.Tools.opened() {
		stopReason = "tool_use"
	}
	delta, err := marshalJSON(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": state.Usage.CompletionTokens},
	})
	if err == nil {
		out = append(out, sseEvent("message_delta", delta)...)
	}
	stopEvent, err := marshalJSON(map[string]any{"type": "message_stop"})
	if err == nil {
		out = append(out, sseEvent("message_stop", stopEvent)...)
	}
	state.Finished = true
	return out
}

func (anthropicEnvelope) renderError(status int, code, message string) []byte {
	body, err := marshalJSON(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    anthropicErrorType(status),
			"message": message,
		},
	})
	if err != nil {
		return []byte(`{"type":"error","error":{"type":"api_error","message":"internal error"}}`)
	}
	return body
}

// anthropicErrorType maps a status onto the Anthropic error taxonomy.
func anthropicErrorType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

func toolCallID(call any) string {
	entry, ok := call.(map[string]any)
	if !ok {
		return ""
	}
	id, _ := entry["id"].(string)
	return id
}

func toolCallName(call any) string {
	entry, ok := call.(map[string]any)
	if !ok {
		return ""
	}
	fn, _ := entry["function"].(map[string]any)
	name, _ := fn["name"].(string)
	return name
}

// toolArgumentsDelta renders one frame's tool arguments as the text to stream,
// accepting every shape an upstream can send.
//
// It REPLACED a helper that asserted the value was a string and returned ""
// otherwise, so an upstream sending a JSON object or a number had its arguments
// silently dropped on the STREAMING path -- even though the non-streaming path
// already normalised them. The same defect had been fixed in one place and not
// the other, which is why both paths now go through a normaliser.
//
// A string is forwarded verbatim, because that is what a streamed delta is: the
// client concatenates the fragments into the final argument JSON, so re-encoding
// each fragment would corrupt the assembly. A non-string value has no fragments
// to preserve, so it is encoded once.
func toolArgumentsDelta(call any) string {
	entry, ok := call.(map[string]any)
	if !ok {
		return ""
	}
	fn, _ := entry["function"].(map[string]any)
	if fn == nil {
		return ""
	}
	switch v := fn["arguments"].(type) {
	case string:
		// The normal case: a fragment of the argument JSON.
		return v
	case nil:
		return ""
	default:
		// An object, array, number or bool. There is nothing to concatenate, so
		// it is serialised once and streamed as a single delta.
		encoded, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

// toolInputObject renders tool-call arguments as the JSON OBJECT both the
// Anthropic and Responses protocols require for a tool's input.
//
// It accepts every shape an upstream might send, because a strict decoder turns
// an upstream's formatting choice into missing tool calls:
//
//   - a JSON object                 -> passed through unchanged (the normal case)
//   - a JSON string holding an object -> parsed, so the client sees a real object
//   - anything else                  -> wrapped, so the value is preserved
//     rather than dropped
//
// Returning a valid object for every input is the point: no shape may cause the
// tool call to disappear.
func toolInputObject(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage("{}")
	}
	// Already an object or array: forward verbatim.
	if trimmed[0] == '{' || trimmed[0] == '[' {
		if json.Valid(trimmed) {
			if trimmed[0] == '[' {
				// A bare array is not an object; wrap it so the field still
				// type-checks on a strict client.
				return json.RawMessage(`{"_items":` + string(trimmed) + `}`)
			}
			return json.RawMessage(trimmed)
		}
	}
	// A JSON string that itself contains a serialised object.
	if trimmed[0] == '"' {
		var inner string
		if err := json.Unmarshal(trimmed, &inner); err == nil {
			nested := bytes.TrimSpace([]byte(inner))
			if len(nested) > 0 && nested[0] == '{' && json.Valid(nested) {
				return json.RawMessage(nested)
			}
			// A plain string argument: wrap it so the value survives.
			encoded, err := json.Marshal(inner)
			if err == nil {
				return json.RawMessage(`{"_raw":` + string(encoded) + `}`)
			}
		}
	}
	// A bare number, bool, null, or malformed text: wrap whatever we have so the
	// call is still delivered.
	encoded, err := json.Marshal(string(trimmed))
	if err != nil {
		return json.RawMessage("{}")
	}
	return json.RawMessage(`{"_raw":` + string(encoded) + `}`)
}
