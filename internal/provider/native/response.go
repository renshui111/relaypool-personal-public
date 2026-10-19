package native

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

func object(value any) map[string]any {
	out, _ := value.(map[string]any)
	if out == nil {
		return map[string]any{}
	}
	return out
}
func array(value any) []any { out, _ := value.([]any); return out }
func number(value any) int  { v, _ := value.(float64); return int(v) }
func usage(family string, value any) provider.Usage {
	m := object(value)
	var u provider.Usage
	switch family {
	case "claude":
		u.PromptTokens = number(m["input_tokens"]) + number(m["cache_creation_input_tokens"]) + number(m["cache_read_input_tokens"])
		u.CompletionTokens = number(m["output_tokens"])
	case "gemini":
		u.PromptTokens = number(m["promptTokenCount"])
		u.CompletionTokens = number(m["candidatesTokenCount"]) + number(m["thoughtsTokenCount"])
		u.TotalTokens = number(m["totalTokenCount"])
	default:
		u.PromptTokens = number(m["input_tokens"])
		u.CompletionTokens = number(m["output_tokens"])
		u.TotalTokens = number(m["total_tokens"])
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return u
}
func finish(value string) string {
	switch value {
	case "tool_use":
		return "tool_calls"
	case "max_tokens", "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	default:
		return "stop"
	}
}
func functionCall(id, name, args string, signature string) map[string]any {
	out := map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
	if signature != "" {
		out["extra_content"] = map[string]any{"google": map[string]any{"thought_signature": signature}}
	}
	return out
}
func convertResult(family string, root map[string]any, model string) (*provider.ChatResult, error) {
	if err, ok := root["error"]; ok {
		return nil, &provider.Failure{Status: 502, Message: "平台返回错误响应", Body: string(marshal(err))}
	}
	result := &provider.ChatResult{ID: str(root, "id"), Model: model, Created: time.Now().Unix(), Message: provider.Message{Role: "assistant"}, FinishReason: "stop"}
	text := strings.Builder{}
	reasoning := strings.Builder{}
	tools := []map[string]any{}
	switch family {
	case "claude":
		if str(root, "type") != "message" {
			return nil, &provider.Failure{Status: 502, Message: "Claude 响应缺少 message 类型"}
		}
		result.Model = str(root, "model")
		result.FinishReason = finish(str(root, "stop_reason"))
		result.Usage = usage(family, root["usage"])
		result.Message.NativeContent = marshal(root["content"])
		for _, item := range array(root["content"]) {
			part := object(item)
			switch str(part, "type") {
			case "text":
				text.WriteString(textValue(part, "text"))
			case "thinking":
				reasoning.WriteString(textValue(part, "thinking"))
			case "tool_use":
				tools = append(tools, functionCall(str(part, "id"), str(part, "name"), string(marshal(part["input"])), ""))
			}
		}
	case "gemini":
		if response, ok := root["response"].(map[string]any); ok {
			root = response
		}
		candidates := array(root["candidates"])
		if len(candidates) == 0 {
			return nil, &provider.Failure{Status: 502, Message: "Gemini 没有返回候选结果", Body: string(marshal(root["promptFeedback"]))}
		}
		candidate := object(candidates[0])
		result.ID = str(root, "responseId")
		result.Usage = usage(family, root["usageMetadata"])
		result.FinishReason = finish(str(candidate, "finishReason"))
		for _, item := range array(object(candidate["content"])["parts"]) {
			part := object(item)
			if thought, _ := part["thought"].(bool); !thought {
				text.WriteString(textValue(part, "text"))
			} else {
				reasoning.WriteString(textValue(part, "text"))
			}
			if fn, ok := part["functionCall"].(map[string]any); ok {
				id := str(fn, "id")
				if id == "" {
					id = "call_" + strconv.Itoa(len(tools))
				}
				tools = append(tools, functionCall(id, str(fn, "name"), string(marshal(fn["args"])), str(part, "thoughtSignature")))
			}
		}
	case "openai", "codex":
		status := str(root, "status")
		if status == "failed" {
			return nil, &provider.Failure{Status: 502, Message: "Responses 推理失败"}
		}
		if status == "incomplete" {
			result.FinishReason = "length"
		}
		if native := str(root, "model"); native != "" {
			result.Model = native
		}
		result.Usage = usage(family, root["usage"])
		if root["output"] == nil {
			return nil, &provider.Failure{Status: 502, Message: "Responses 响应缺少 output"}
		}
		result.NativeOutput = marshal(root["output"])
		for _, item := range array(root["output"]) {
			entry := object(item)
			switch str(entry, "type") {
			case "message":
				for _, part := range array(entry["content"]) {
					p := object(part)
					text.WriteString(textValue(p, "text"))
					if str(p, "type") == "refusal" {
						text.WriteString(textValue(p, "refusal"))
						result.FinishReason = "content_filter"
					}
				}
			case "function_call":
				tools = append(tools, functionCall(str(entry, "call_id"), str(entry, "name"), textValue(entry, "arguments"), ""))
			case "reasoning":
				for _, part := range array(entry["summary"]) {
					reasoning.WriteString(textValue(object(part), "text"))
				}
			}
		}
	default:
		return nil, fmt.Errorf("未实现的平台响应类型")
	}
	if result.Model == "" {
		result.Model = model
	}
	if len(tools) > 0 {
		result.Message.ToolCalls = marshal(tools)
		if result.FinishReason == "stop" {
			result.FinishReason = "tool_calls"
		}
	}
	result.Message.Content = marshal(text.String())
	result.Message.ReasoningContent = reasoning.String()
	return result, nil
}
