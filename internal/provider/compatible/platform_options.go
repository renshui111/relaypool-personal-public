package compatible

import (
	"encoding/json"
	"strings"

	"github.com/relaypool/relaypool/internal/provider"
)

func supportsThinking(family string) bool {
	switch family {
	case "deepseek", "glm", "kimi", "doubao", "mimo":
		return true
	}
	return false
}

// 翻译显式的思考开关，保留厂商的其他思考选项，不擅自更改上游默认行为。
func adaptThinking(body map[string]any, req *provider.ChatRequest, family string) error {
	disabled := req.DisableThinking || req.ReasoningEffort == "none"
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(req.Raw, &raw)
	var enabled *bool
	_ = json.Unmarshal(raw["enable_thinking"], &enabled)
	if enabled == nil {
		_ = json.Unmarshal(raw["enable_reasoning"], &enabled)
	}
	delete(body, "enable_reasoning")
	if enabled != nil && req.ReasoningEffort == "medium" {
		delete(body, "reasoning_effort")
	}
	model := strings.ToLower(req.Model)
	if disabled && ((family == "kimi" && (strings.HasPrefix(model, "kimi-k3") || strings.HasPrefix(model, "kimi-k2.7-code"))) || (family == "glm" && strings.HasPrefix(model, "glm-5.3"))) {
		return &provider.Failure{Status: 400, Message: "所选模型始终思考，请使用该型号支持的 reasoning_effort 档位"}
	}
	if family == "qwen" {
		if v, ok := raw["thinking"]; ok {
			var choice struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(v, &choice) == nil {
				if choice.Type == "disabled" {
					body["enable_thinking"] = false
				} else if choice.Type == "enabled" {
					body["enable_thinking"] = true
				}
			}
		}
		if enabled != nil {
			body["enable_thinking"] = *enabled
		}
		if disabled {
			body["enable_thinking"] = false
			delete(body, "reasoning_effort")
		}
		return nil
	}
	delete(body, "enable_thinking")
	if supportsThinking(family) && (enabled != nil || disabled) {
		thinking, _ := body["thinking"].(map[string]any)
		if thinking == nil {
			thinking = map[string]any{}
		}
		thinking["type"] = "enabled"
		if disabled || (enabled != nil && !*enabled) {
			thinking["type"] = "disabled"
			delete(body, "reasoning_effort")
		}
		body["thinking"] = thinking
	}
	return nil
}

// 网关仅能跨协议翻译函数工具；内置搜索、代码执行等工具须走原生厂商接口。
func validateFunctionTools(raw json.RawMessage) error {
	var tools []map[string]any
	if json.Unmarshal(raw, &tools) != nil {
		return &provider.Failure{Status: 400, Message: "工具定义必须是数组"}
	}
	for _, tool := range tools {
		kind, _ := tool["type"].(string)
		if kind != "" && kind != "function" {
			return &provider.Failure{Status: 400, Message: "此渠道只支持 function 工具，无法转换厂商内置工具"}
		}
		fn := tool
		if nested, exists := tool["function"]; exists {
			fn, _ = nested.(map[string]any)
		}
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			return &provider.Failure{Status: 400, Message: "函数工具必须包含有效名称"}
		}
	}
	return nil
}
