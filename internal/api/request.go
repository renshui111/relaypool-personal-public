package api

import (
	"encoding/json"
	"strings"

	"github.com/relaypool/relaypool/internal/provider"
)

// chatRequestWire is the OpenAI Chat Completions request shape.
//
// Every field the gateway acts on is modelled explicitly; anything else is
// preserved because the adapter receives the original body, so vendor
// extensions still reach the upstream. Dropping unknown fields silently would
// change behaviour for clients that rely on them.
type chatRequestWire struct {
	Model    string            `json:"model"`
	Messages []json.RawMessage `json:"messages"`
	Stream   bool              `json:"stream"`

	Temperature         *float64 `json:"temperature"`
	TopP                *float64 `json:"top_p"`
	MaxTokens           *int     `json:"max_tokens"`
	MaxCompletionTokens *int     `json:"max_completion_tokens"`

	Stop json.RawMessage `json:"stop"`

	Tools      json.RawMessage `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`

	ReasoningEffort json.RawMessage `json:"reasoning_effort"`
	EnableThinking  *bool           `json:"enable_thinking"`
	EnableReasoning *bool           `json:"enable_reasoning"`
}

// parseChatRequest converts the wire request into the provider-neutral form.
func parseChatRequest(raw []byte) (*provider.ChatRequest, error) {
	var wire chatRequestWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}

	req := &provider.ChatRequest{
		Model:       strings.TrimSpace(wire.Model),
		Stream:      wire.Stream,
		Tools:       wire.Tools,
		ToolChoice:  wire.ToolChoice,
		Temperature: wire.Temperature,
		TopP:        wire.TopP,
		MaxTokens:   wire.MaxTokens,
		// The original body is kept so the adapter can pass through the fields
		// this struct does not model.
		Raw: raw,
	}

	for _, rawMessage := range wire.Messages {
		req.Messages = append(req.Messages, parseChatMessage(rawMessage))
	}

	req.Stop = parseStopField(wire.Stop)
	if req.MaxTokens == nil {
		req.MaxTokens = wire.MaxCompletionTokens
	}
	req.ReasoningEffort = reasoningEffortFrom(wire)
	return req, nil
}

// parseChatMessage splits a raw message into the fields the gateway needs while
// keeping the content in its original form (string or parts array), because
// the upstream understands both and re-encoding would lose information.
func parseChatMessage(raw json.RawMessage) provider.Message {
	var envelope struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		Name             string          `json:"name"`
		ToolCalls        json.RawMessage `json:"tool_calls"`
		ToolCallID       string          `json:"tool_call_id"`
		ReasoningContent string          `json:"reasoning_content"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		// An unparseable message keeps its raw content so the upstream can
		// produce a specific diagnostic instead of a generic local error.
		return provider.Message{Content: raw}
	}
	return provider.Message{
		Role:             envelope.Role,
		Content:          envelope.Content,
		Name:             envelope.Name,
		ToolCalls:        envelope.ToolCalls,
		ToolCallID:       envelope.ToolCallID,
		ReasoningContent: envelope.ReasoningContent,
	}
}

// parseStopField reads the stop field, which may be a string or an array.
func parseStopField(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil
		}
		return []string{single}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

// reasoningEffortFrom normalizes the effort field.
//
// Clients spell this several ways: a bare string, an object with an effort or
// level key, or a boolean toggle. All three are accepted so a client does not
// have to learn this gateway's preferred spelling.
func reasoningEffortFrom(wire chatRequestWire) string {
	if len(wire.ReasoningEffort) > 0 {
		var text string
		if err := json.Unmarshal(wire.ReasoningEffort, &text); err == nil {
			if text != "" {
				return text
			}
		}
		var object struct {
			Effort string `json:"effort"`
			Level  string `json:"level"`
			Type   string `json:"type"`
		}
		if err := json.Unmarshal(wire.ReasoningEffort, &object); err == nil {
			switch {
			case object.Effort != "":
				return object.Effort
			case object.Level != "":
				return object.Level
			case object.Type != "":
				return object.Type
			}
		}
	}
	// A boolean toggle is a weaker signal: it says "on" or "off" with no
	// strength, which maps to the model default or to none.
	if wire.EnableThinking != nil {
		if *wire.EnableThinking {
			return "medium"
		}
		return "none"
	}
	if wire.EnableReasoning != nil {
		if *wire.EnableReasoning {
			return "medium"
		}
		return "none"
	}
	return ""
}

// sessionSeedFrom derives the sticky-routing seed from a conversation.
//
// The FIRST USER MESSAGE is the seed, not the whole history. Stateless clients
// resend the entire history every turn, so using all of it would change the
// seed on each request and defeat stickiness completely.
func sessionSeedFrom(messages []provider.Message) string {
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		if text := message.ContentText(); text != "" {
			return text
		}
	}
	// A conversation that opens with an image-only turn still gets a seed, so
	// it is not silently excluded from sticky routing.
	for _, message := range messages {
		if message.Role == "user" && len(message.Content) > 0 {
			return string(message.Content)
		}
	}
	return ""
}

// providerFamilies is the set of families a prefixed model ID may pin.
var providerFamilies = []string{"workbuddy", "qoder", "trae"}

// resolveModel splits a requested model ID into the routing model and an
// optional provider pin.
//
// A prefixed ID ("workbuddy/glm-5.2") pins the family; a bare ID ("glm-5.2")
// is routable across families when the pool allows it. The prefix is the
// client's explicit way of choosing a channel, which matters because the same
// model name exists on more than one upstream with different quotas.
func resolveModel(requested string) (model string, providerFilter string) {
	trimmed := strings.TrimSpace(requested)
	lower := strings.ToLower(trimmed)
	for _, family := range providerFamilies {
		prefix := family + "/"
		if strings.HasPrefix(lower, prefix) && len(trimmed) > len(prefix) {
			return strings.TrimSpace(trimmed[len(prefix):]), family
		}
	}
	return trimmed, ""
}
