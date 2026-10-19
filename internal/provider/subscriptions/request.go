package subscriptions

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/compatible"
)

func (c *Client) chatBody(cred provider.Credential, req *provider.ChatRequest) ([]byte, error) {
	if req == nil {
		return nil, failure(400, "订阅请求不能为空")
	}
	// 加密推理项属于原生供应商，跨渠道不能把它丢弃后假装成功续接。
	if strings.Contains(string(req.NativeInput), "encrypted_content") {
		return nil, failure(400, "订阅渠道不支持其他平台加密推理回放，请发送完整可读历史")
	}
	encoded, err := compatible.BuildChatBody(req, true)
	if err != nil {
		return nil, err
	}
	var neutral map[string]any
	if json.Unmarshal(encoded, &neutral) != nil {
		return nil, failure(400, "订阅请求格式无效")
	}
	var original map[string]any
	_ = json.Unmarshal(req.Raw, &original)
	if format := object(original["response_format"]); format["type"] != nil && format["type"] != "text" {
		return nil, failure(400, "订阅渠道尚不支持强制结构化输出格式")
	}
	if format := object(object(original["text"])["format"]); format["type"] != nil && format["type"] != "text" {
		return nil, failure(400, "订阅渠道尚不支持强制结构化输出格式")
	}
	// 仅转发有明确转换规则的字段，避免将客户端原始 body 中的内部参数传入上游。
	body := map[string]any{}
	for _, key := range []string{"model", "messages", "tools", "tool_choice", "temperature", "top_p", "max_tokens", "stop", "reasoning_effort"} {
		if value, ok := neutral[key]; ok {
			body[key] = value
		}
	}
	body["stream"] = true
	if req.MaxTokens != nil {
		body["max_tokens"] = *req.MaxTokens
	}
	if c.family == "trae" {
		for _, message := range body["messages"].([]any) {
			m := object(message)
			if m["role"] == "developer" {
				m["role"] = "system"
			}
			if text, ok := m["content"].(string); ok {
				m["content"] = []any{map[string]any{"type": "text", "text": text}}
			} else if parts, ok := m["content"].([]any); ok {
				for _, p := range parts {
					if object(p)["type"] != "text" {
						return nil, failure(400, "Trae 国内聊天入口目前仅支持文本和函数工具")
					}
				}
			}
			if calls, ok := m["tool_calls"].([]any); ok {
				for _, call := range calls {
					entry := object(call)
					entry["function_call"] = entry["function"]
					delete(entry, "function")
				}
			}
		}
		if tools, ok := body["tools"].([]any); ok {
			for _, tool := range tools {
				fn := object(object(tool)["function"])
				if params, ok := fn["parameters"]; ok {
					raw, _ := json.Marshal(params)
					fn["parameters"] = string(raw)
				}
			}
		}
		if choice := body["tool_choice"]; choice == "none" {
			delete(body, "tool_choice")
			delete(body, "tools")
		} else if object(choice)["type"] == "function" {
			body["tool_choice"] = object(object(choice)["function"])["name"]
		}
		body["config_name"], body["function"] = req.Model, cred.Field("scene")
		if req.DisableThinking {
			body["reasoning_effort"] = "none"
		}
		return json.Marshal(body)
	}
	var prompt string
	for _, message := range req.Messages {
		if message.Role == "user" {
			prompt = message.ContentText()
		}
	}
	if prompt == "" {
		return nil, failure(400, "Qoder 需要至少一条带文本的用户消息")
	}
	for _, message := range body["messages"].([]any) {
		m := object(message)
		if m["role"] == "developer" {
			m["role"] = "system"
		}
		if m["role"] == "assistant" && m["tool_calls"] != nil && (m["content"] == nil || m["content"] == "") {
			m["content"] = "调用工具。"
		}
	}
	nid := uuid.NewString()
	images := false
	for _, message := range req.Messages {
		if strings.Contains(string(message.Content), `"image_url"`) {
			images = true
		}
	}
	config := map[string]any{"key": req.Model, "display_name": "", "model": "", "format": "openai", "is_vl": images, "is_reasoning": true, "api_key": "", "url": "", "source": "system"}
	parameters := map[string]any{"max_tokens": 32768}
	for _, key := range []string{"max_tokens", "temperature", "top_p", "stop", "reasoning_effort"} {
		if value, ok := body[key]; ok {
			parameters[key] = value
			delete(body, key)
		}
	}
	if req.DisableThinking {
		parameters["enable_thinking"] = false
		parameters["reasoning_effort"] = "none"
		config["is_reasoning"] = false
	} else if req.ReasoningEffort != "" {
		parameters["enable_thinking"] = true
	}
	for k, v := range map[string]any{"request_id": nid, "chat_record_id": nid, "request_set_id": uuid.NewString(), "session_id": uuid.NewString(), "agent_id": "agent_common", "chat_task": "FREE_INPUT", "chat_context": map[string]any{"chatPrompt": "", "text": map[string]any{"type": "text", "text": prompt}, "features": []any{}, "imageUrls": nil, "extra": map[string]any{"context": []any{}, "modelConfig": config, "originalContent": map[string]any{"type": "text", "text": prompt}}}, "image_urls": nil, "is_reply": true, "is_retry": false, "code_language": "", "source": 1, "version": "3", "chat_prompt": "", "parameters": parameters, "aliyun_user_type": cred.Field("user_type"), "session_type": "qodercli", "task_id": "common", "model_config": config, "business": map[string]any{"product": "cli", "version": "0.1.43", "type": "agent", "id": uuid.NewString(), "name": "聊天", "begin_at": time.Now().UnixMilli(), "stage": "start"}} {
		body[k] = v
	}
	return json.Marshal(body)
}
