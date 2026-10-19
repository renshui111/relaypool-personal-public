package api

import (
	"encoding/json"

	"github.com/relaypool/relaypool/internal/executor"
)

// chatCompletionsEnvelope renders the OpenAI Chat Completions shape, which is
// the native wire format the upstream already speaks. Rendering is therefore
// mostly pass-through: the interesting work is rebuilding the frames the
// adapter stripped.
type chatCompletionsEnvelope struct{}

func (chatCompletionsEnvelope) contentType() string {
	return "application/json; charset=utf-8"
}

// renderNonStream builds a complete chat.completion object.
func (chatCompletionsEnvelope) renderNonStream(result *executor.Result, requestedModel string) ([]byte, error) {
	if result == nil || result.Chat == nil {
		return marshalJSON(map[string]any{
			"object":  "chat.completion",
			"choices": []any{},
		})
	}
	chat := result.Chat

	message := map[string]any{"role": orDefault(chat.Message.Role, "assistant")}
	if len(chat.Message.Content) > 0 {
		message["content"] = json.RawMessage(chat.Message.Content)
	} else {
		message["content"] = ""
	}
	if len(chat.Message.ToolCalls) > 0 {
		message["tool_calls"] = json.RawMessage(chat.Message.ToolCalls)
	}
	if chat.Message.ReasoningContent != "" {
		message["reasoning_content"] = chat.Message.ReasoningContent
	}

	return marshalJSON(map[string]any{
		"id":      orDefault(chat.ID, "chatcmpl-"+resultID(result)),
		"object":  "chat.completion",
		"created": orDefaultInt64(chat.Created, nowUnix()),
		"model":   orDefault(chat.Model, requestedModel),
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": orDefault(chat.FinishReason, "stop"),
			},
		},
		"usage": usageObject(chat.Usage),
	})
}

// renderStreamChunk converts a sanitized upstream frame into a client frame.
//
// The adapter has already stripped the upstream's placeholder fields, so this
// mostly forwards. What it must NOT do is invent fields the adapter removed:
// re-adding an empty content string would recreate the blank-chunk flood.
func (chatCompletionsEnvelope) renderStreamChunk(raw []byte, state *streamState) ([]byte, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	return sseWrite(raw), true
}

func (chatCompletionsEnvelope) renderStreamStart(*streamState) []byte { return nil }

func (chatCompletionsEnvelope) renderStreamEnd(*streamState) []byte {
	return sseWrite([]byte("[DONE]"))
}

func (chatCompletionsEnvelope) renderError(status int, code, message string) []byte {
	body, err := marshalJSON(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "api_error",
			"code":    code,
		},
	})
	if err != nil {
		return []byte(`{"error":{"message":"internal error","type":"api_error"}}`)
	}
	return body
}

// usageObject renders usage, omitting unknown counts rather than reporting zero.
// A reported 0 is a lie the client may act on; an absent field is honest.
func usageObject(usage providerUsage) map[string]any {
	out := map[string]any{}
	if usage.PromptTokens > 0 {
		out["prompt_tokens"] = usage.PromptTokens
	}
	if usage.CompletionTokens > 0 {
		out["completion_tokens"] = usage.CompletionTokens
	}
	if usage.TotalTokens > 0 {
		out["total_tokens"] = usage.TotalTokens
	}
	if len(out) == 0 {
		// Some clients require a usage object even when empty.
		out["prompt_tokens"] = 0
		out["completion_tokens"] = 0
		out["total_tokens"] = 0
	}
	return out
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func orDefaultInt64(value, fallback int64) int64 {
	if value == 0 {
		return fallback
	}
	return value
}

func resultID(result *executor.Result) string {
	if result == nil || result.Chat == nil || result.Chat.ID == "" {
		return "unknown"
	}
	return result.Chat.ID
}
