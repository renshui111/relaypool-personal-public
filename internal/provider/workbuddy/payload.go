package workbuddy

import (
	"encoding/json"
	"strings"
)

// PrepareBody rewrites a request body into the shape the upstream accepts.
//
// It only touches known keys; unknown fields pass through untouched. That
// matters because clients send vendor extensions (reasoning hints, cache
// controls) that this adapter has no business dropping.
//
// Four transforms, each fixing a distinct upstream requirement:
//
//  1. stream is forced true — the upstream only reliably supports streaming,
//     so non-streaming callers are served by aggregating the stream locally.
//  2. tool_choice is normalized from the OpenAI object form to the string form
//     the upstream accepts.
//  3. an empty tools array is removed — upstream rejects tools:[] with an error
//     rather than treating it as "no tools".
//  4. a leading system message is ensured, satisfying Global's requirement.
func PrepareBody(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var body map[string]any
	if err := json.Unmarshal(src, &body); err != nil {
		// A body this adapter cannot parse is passed through unchanged: the
		// upstream may still accept it, and corrupting it would be worse.
		return src
	}
	body["stream"] = true
	normalizeToolChoice(body)
	dropEmptyTools(body)
	ensureLeadingSystem(body)

	out, err := json.Marshal(body)
	if err != nil {
		return src
	}
	return out
}

// ensureLeadingSystem 保证首条为非空系统消息。
// 国际接口要求首条为 system；Kimi 等型号还会用 11151 拒绝空正文。
// 仅补充缺失或空的首条系统消息，保留用户原有的非空指令和后续消息。
func ensureLeadingSystem(body map[string]any) {
	raw, ok := body["messages"]
	if !ok {
		return
	}
	list, ok := raw.([]any)
	if !ok {
		return
	}
	const defaultSystemContent = "请根据用户的请求作答。"
	placeholder := map[string]any{"role": "system", "content": defaultSystemContent}
	if len(list) == 0 {
		body["messages"] = []any{placeholder}
		return
	}
	if first, ok := list[0].(map[string]any); ok {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			if emptySystemContent(first["content"]) {
				// 复制首条消息，避免改写调用者共享的消息对象。
				replacement := make(map[string]any, len(first)+1)
				for key, value := range first {
					replacement[key] = value
				}
				replacement["content"] = defaultSystemContent
				body["messages"] = append([]any{replacement}, list[1:]...)
			}
			return
		}
	}
	// Prepend without mutating the caller's slice.
	body["messages"] = append([]any{placeholder}, list...)
}

// emptySystemContent 识别空文本和空文本块，其他内容保留给上游校验。
func emptySystemContent(content any) bool {
	switch value := content.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(value) == ""
	case []any:
		for _, item := range value {
			part, ok := item.(map[string]any)
			if !ok || part["type"] != "text" {
				return false
			}
			text, ok := part["text"].(string)
			if !ok || strings.TrimSpace(text) != "" {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// normalizeToolChoice converts tool_choice to the string form the upstream
// accepts, and drops the field entirely where it is meaningless.
//
// OpenAI clients send either a bare string or an object like
// {"type":"function","function":{"name":"foo"}}. The upstream wants the plain
// string "foo", so the object form must be unwrapped.
func normalizeToolChoice(body map[string]any) {
	raw, ok := body["tool_choice"]
	if !ok {
		return
	}
	switch value := raw.(type) {
	case string:
		if value == "none" {
			// "none" means no tools at all, so the declarations go too.
			delete(body, "tool_choice")
			delete(body, "tools")
			delete(body, "functions")
			return
		}
		return
	case map[string]any:
		kind, _ := value["type"].(string)
		switch kind {
		case "none":
			delete(body, "tool_choice")
			delete(body, "tools")
			delete(body, "functions")
		case "auto", "required":
			body["tool_choice"] = kind
		case "function":
			name := functionName(value)
			if name == "" {
				// An unnamed function choice is unusable; "auto" is the
				// closest valid intent.
				name = "auto"
			}
			body["tool_choice"] = name
		default:
			delete(body, "tool_choice")
		}
	default:
		// Any other JSON type is not a form the upstream understands.
		delete(body, "tool_choice")
	}
}

// functionName extracts the function name from a tool_choice object, accepting
// both the nested and flattened spellings seen in the wild.
func functionName(value map[string]any) string {
	if fn, ok := value["function"].(map[string]any); ok {
		if name, _ := fn["name"].(string); name != "" {
			return name
		}
	}
	name, _ := value["name"].(string)
	return name
}

// dropEmptyTools removes a tools array that is empty or null.
//
// The upstream rejects tools:[] outright rather than treating it as "no tools",
// and tool_choice without tools is meaningless, so both go together.
func dropEmptyTools(body map[string]any) {
	raw, ok := body["tools"]
	if !ok {
		return
	}
	if raw == nil {
		delete(body, "tools")
		delete(body, "tool_choice")
		return
	}
	if list, ok := raw.([]any); ok && len(list) == 0 {
		delete(body, "tools")
		delete(body, "tool_choice")
	}
}

// ReasoningEffortLevels are the effort values the upstream accepts, lowest
// first. "none" disables thinking and is only valid when the model permits it.
var ReasoningEffortLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// NormalizeReasoningLevel folds an arbitrary effort string onto a known level.
//
// "max" is mapped to "xhigh" because OpenAI-style clients send "max" while the
// upstream understands "xhigh"; without this alias a request for maximum
// reasoning would silently be dropped.
func NormalizeReasoningLevel(raw string) string {
	level := strings.ToLower(strings.TrimSpace(raw))
	switch level {
	case "":
		return ""
	case "max", "highest", "maximum":
		return "xhigh"
	}
	for _, known := range ReasoningEffortLevels {
		if level == known {
			return level
		}
	}
	return ""
}

// resolveReasoningLevel picks the effort to send, given a requested level and
// the model's capabilities.
//
// Two rules matter:
//
//   - A model that cannot disable thinking must never be sent "none": the
//     upstream errors. Fall back to the model's default instead.
//   - An unknown or empty level resolves to the model default, so the gateway
//     does not invent a level the model may not support.
func resolveReasoningLevel(requested string, options []string, defaultLevel string, canDisable bool) string {
	level := NormalizeReasoningLevel(requested)
	if level == "" {
		level = NormalizeReasoningLevel(defaultLevel)
	}
	if level == "" {
		return ""
	}
	if level == "none" && !canDisable {
		// Thinking is mandatory here; use the default rather than erroring.
		level = NormalizeReasoningLevel(defaultLevel)
		if level == "" || level == "none" {
			return ""
		}
	}
	// If the model advertises its options, respect them: sending an
	// unsupported level is a request error upstream.
	if len(options) > 0 && !containsLevel(options, level) {
		if fallback := NormalizeReasoningLevel(defaultLevel); fallback != "" && containsLevel(options, fallback) {
			return fallback
		}
		return ""
	}
	return level
}

func containsLevel(options []string, level string) bool {
	for _, option := range options {
		if NormalizeReasoningLevel(option) == level || option == level {
			return true
		}
	}
	return false
}

// applyChatReasoning sets the upstream "reasoning" object on a prepared body.
func applyChatReasoning(body map[string]any, requested string, options []string, defaultLevel string, canDisable bool) {
	if body == nil {
		return
	}
	level := resolveReasoningLevel(requested, options, defaultLevel, canDisable)
	if level == "" {
		delete(body, "reasoning")
		return
	}
	if level == "none" {
		delete(body, "reasoning")
		return
	}
	body["reasoning"] = map[string]any{"effort": level, "summary": "auto"}
}
