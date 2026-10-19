package native

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/relaypool/relaypool/internal/provider"
)

type function struct {
	Name        string
	Description string
	Schema      any
	Strict      any
}
type call struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	Extra map[string]any `json:"extra_content"`
}

func functions(raw json.RawMessage) ([]function, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var tools []map[string]any
	if json.Unmarshal(raw, &tools) != nil {
		return nil, fmt.Errorf("工具列表格式无效")
	}
	out := make([]function, 0, len(tools))
	for _, tool := range tools {
		fn := tool
		if nested, ok := tool["function"].(map[string]any); ok {
			fn = nested
		}
		name := str(fn, "name")
		schema := fn["parameters"]
		if schema == nil {
			schema = fn["input_schema"]
		}
		if name == "" || schema == nil {
			return nil, &provider.Failure{Status: 400, KindHint: "invalid_request", Message: "该渠道目前支持带参数结构的函数工具；此工具无法转换"}
		}
		out = append(out, function{Name: name, Description: str(fn, "description"), Schema: schema, Strict: fn["strict"]})
	}
	return out, nil
}
func contentParts(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []map[string]any{}, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []map[string]any{{"type": "text", "text": text}}, nil
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		return nil, fmt.Errorf("消息内容格式无效")
	}
	return parts, nil
}
func calls(raw json.RawMessage) ([]call, error) {
	var out []call
	if len(raw) > 0 && json.Unmarshal(raw, &out) != nil {
		return nil, fmt.Errorf("工具调用格式无效")
	}
	return out, nil
}
func imagePart(part map[string]any) (string, string, string, error) {
	// 仅转换地址与内嵌数据，不在网关下载用户提供的图片 URL。
	imageURL := ""
	if u, ok := part["image_url"].(map[string]any); ok {
		imageURL = str(u, "url")
	} else {
		imageURL = str(part, "image_url")
	}
	if source, ok := part["source"].(map[string]any); ok {
		if str(source, "type") == "base64" {
			return str(source, "media_type"), str(source, "data"), "", nil
		}
		imageURL = str(source, "url")
	}
	if strings.HasPrefix(imageURL, "data:") {
		chunks := strings.SplitN(strings.TrimPrefix(imageURL, "data:"), ",", 2)
		if len(chunks) != 2 || !strings.HasSuffix(chunks[0], ";base64") {
			return "", "", "", fmt.Errorf("图片数据需要 Base64 data URL")
		}
		return strings.TrimSuffix(chunks[0], ";base64"), chunks[1], "", nil
	}
	u, err := url.Parse(imageURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return "", "", "", fmt.Errorf("图片地址无效")
	}
	return "", "", imageURL, nil
}
func (c *Client) chatRequest(cred provider.Credential, req *provider.ChatRequest, stream bool) ([]byte, string, error) {
	switch c.family {
	case "claude":
		body, err := claudeBody(req, stream)
		return body, cred.Field("base_url") + "/messages", err
	case "gemini":
		body, err := geminiBody(req)
		if err != nil {
			return nil, "", err
		}
		action := "generateContent"
		if stream {
			action = "streamGenerateContent"
		}
		model := strings.TrimPrefix(req.Model, "models/")
		if model == "" || strings.ContainsAny(model, "/?#\\") {
			return nil, "", &provider.Failure{Status: 400, KindHint: "invalid_request", Message: "Gemini 模型名称无效"}
		}
		endpoint := cred.Field("base_url") + "/models/" + url.PathEscape(model) + ":" + action
		if cred.Field("mode") == "code_assist" {
			body = marshal(map[string]any{"model": model, "project": cred.Field("project_id"), "request": json.RawMessage(body)})
			endpoint = cred.Field("base_url") + ":" + action
		}
		if stream {
			endpoint += "?alt=sse"
		}
		return body, endpoint, nil
	default:
		body, err := responsesBody(req, c.family == "codex", stream)
		return body, cred.Field("base_url") + "/responses", err
	}
}
func claudeBody(req *provider.ChatRequest, stream bool) ([]byte, error) {
	body := map[string]any{"model": req.Model, "stream": stream, "max_tokens": 4096}
	if req.MaxTokens != nil {
		body["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		body["stop_sequences"] = req.Stop
	}
	var raw map[string]any
	_ = json.Unmarshal(req.Raw, &raw)
	for _, key := range []string{"thinking", "output_config", "top_k", "metadata"} {
		if value, ok := raw[key]; ok {
			body[key] = value
		}
	}
	if req.DisableThinking {
		body["thinking"] = map[string]any{"type": "disabled"}
	}
	system := []map[string]any{}
	messages := []map[string]any{}
	for _, m := range req.Messages {
		content := m.Content
		if len(m.NativeContent) > 0 {
			content = m.NativeContent
		}
		parts, err := contentParts(content)
		if err != nil {
			return nil, err
		}
		if m.Role == "system" || m.Role == "developer" {
			system = append(system, parts...)
			continue
		}
		role := m.Role
		if role == "tool" {
			role = "user"
			if len(m.NativeContent) == 0 {
				parts = []map[string]any{{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": json.RawMessage(m.Content)}}
			}
		}
		for i, p := range parts {
			if str(p, "type") == "image_url" || str(p, "type") == "input_image" {
				mime, data, address, err := imagePart(p)
				if err != nil {
					return nil, err
				}
				source := map[string]any{"type": "url", "url": address}
				if data != "" {
					source = map[string]any{"type": "base64", "media_type": mime, "data": data}
				}
				parts[i] = map[string]any{"type": "image", "source": source}
			}
			if str(p, "type") == "input_text" || str(p, "type") == "output_text" {
				p["type"] = "text"
			}
		}
		toolCalls := m.ToolCalls
		if len(m.NativeContent) > 0 {
			toolCalls = nil
		}
		tc, err := calls(toolCalls)
		if err != nil {
			return nil, err
		}
		for _, call := range tc {
			var args any
			if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil {
				return nil, fmt.Errorf("工具参数必须是 JSON")
			}
			parts = append(parts, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": args})
		}
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("Claude 消息角色无效")
		}
		if len(messages) > 0 && messages[len(messages)-1]["role"] == role {
			messages[len(messages)-1]["content"] = append(messages[len(messages)-1]["content"].([]map[string]any), parts...)
		} else {
			messages = append(messages, map[string]any{"role": role, "content": parts})
		}
	}
	body["messages"] = messages
	if len(system) > 0 {
		body["system"] = system
	}
	tools, err := functions(req.Tools)
	if err != nil {
		return nil, err
	}
	if len(tools) > 0 {
		out := []map[string]any{}
		for _, fn := range tools {
			item := map[string]any{"name": fn.Name, "description": fn.Description, "input_schema": fn.Schema}
			if fn.Strict != nil {
				item["strict"] = fn.Strict
			}
			out = append(out, item)
		}
		body["tools"] = out
	}
	if len(req.ToolChoice) > 0 {
		var choice any
		_ = json.Unmarshal(req.ToolChoice, &choice)
		if value, ok := choice.(string); ok {
			if value == "required" {
				value = "any"
			}
			body["tool_choice"] = map[string]any{"type": value}
		} else if value, ok := choice.(map[string]any); ok {
			if fn, ok := value["function"].(map[string]any); ok {
				body["tool_choice"] = map[string]any{"type": "tool", "name": fn["name"]}
			} else {
				body["tool_choice"] = value
			}
		}
	}
	return json.Marshal(body)
}
func geminiBody(req *provider.ChatRequest) ([]byte, error) {
	body := map[string]any{}
	contents := []map[string]any{}
	system := []map[string]any{}
	names := map[string]string{}
	for _, m := range req.Messages {
		parts, err := contentParts(m.Content)
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, p := range parts {
			switch str(p, "type") {
			case "text", "input_text", "output_text":
				out = append(out, map[string]any{"text": p["text"]})
			case "image", "image_url", "input_image":
				mime, data, address, err := imagePart(p)
				if err != nil {
					return nil, err
				}
				if data != "" {
					out = append(out, map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": data}})
				} else {
					out = append(out, map[string]any{"fileData": map[string]any{"fileUri": address}})
				}
			default:
				return nil, &provider.Failure{Status: 400, KindHint: "invalid_request", Message: "Gemini 暂不支持此内容块转换"}
			}
		}
		if m.Role == "system" || m.Role == "developer" {
			system = append(system, out...)
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		tc, err := calls(m.ToolCalls)
		if err != nil {
			return nil, err
		}
		for _, call := range tc {
			var args any
			if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil {
				return nil, fmt.Errorf("工具参数必须是 JSON")
			}
			names[call.ID] = call.Function.Name
			part := map[string]any{"functionCall": map[string]any{"name": call.Function.Name, "args": args, "id": call.ID}}
			if google, ok := call.Extra["google"].(map[string]any); ok {
				if signature := str(google, "thought_signature"); signature != "" {
					part["thoughtSignature"] = signature
				}
			}
			out = append(out, part)
		}
		if m.Role == "tool" {
			name := m.Name
			if name == "" {
				name = names[m.ToolCallID]
			}
			if name == "" {
				return nil, fmt.Errorf("Gemini 工具结果缺少对应函数名称")
			}
			var value any
			_ = json.Unmarshal(m.Content, &value)
			if _, ok := value.(map[string]any); !ok {
				value = map[string]any{"result": value}
			}
			out = []map[string]any{{"functionResponse": map[string]any{"name": name, "id": m.ToolCallID, "response": value}}}
		}
		if len(contents) > 0 && contents[len(contents)-1]["role"] == role {
			contents[len(contents)-1]["parts"] = append(contents[len(contents)-1]["parts"].([]map[string]any), out...)
		} else {
			contents = append(contents, map[string]any{"role": role, "parts": out})
		}
	}
	body["contents"] = contents
	if len(system) > 0 {
		body["systemInstruction"] = map[string]any{"parts": system}
	}
	generation := map[string]any{}
	if req.MaxTokens != nil {
		generation["maxOutputTokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		generation["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		generation["topP"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		generation["stopSequences"] = req.Stop
	}
	var raw map[string]any
	_ = json.Unmarshal(req.Raw, &raw)
	if existing, ok := raw["generationConfig"].(map[string]any); ok {
		for k, v := range existing {
			if _, set := generation[k]; !set {
				generation[k] = v
			}
		}
	}
	if format, ok := raw["response_format"].(map[string]any); ok {
		switch str(format, "type") {
		case "json_object":
			generation["responseMimeType"] = "application/json"
		case "json_schema":
			generation["responseMimeType"] = "application/json"
			if spec, ok := format["json_schema"].(map[string]any); ok {
				generation["responseJsonSchema"] = spec["schema"]
			}
		}
	}
	if len(generation) > 0 {
		body["generationConfig"] = generation
	}
	tools, err := functions(req.Tools)
	if err != nil {
		return nil, err
	}
	if len(tools) > 0 {
		decl := []map[string]any{}
		for _, fn := range tools {
			decl = append(decl, map[string]any{"name": fn.Name, "description": fn.Description, "parametersJsonSchema": fn.Schema})
		}
		body["tools"] = []any{map[string]any{"functionDeclarations": decl}}
	}
	if len(req.ToolChoice) > 0 {
		var choice any
		_ = json.Unmarshal(req.ToolChoice, &choice)
		mode := "AUTO"
		name := ""
		if s, ok := choice.(string); ok {
			if s == "required" {
				mode = "ANY"
			}
			if s == "none" {
				mode = "NONE"
			}
		}
		if value, ok := choice.(map[string]any); ok {
			kind := str(value, "type")
			if kind == "none" {
				mode = "NONE"
			}
			if kind == "any" {
				mode = "ANY"
			}
			name = str(value, "name")
			if fn, ok := value["function"].(map[string]any); ok {
				name = str(fn, "name")
			}
			if name != "" {
				mode = "ANY"
			}
		}
		config := map[string]any{"mode": mode}
		if name != "" {
			config["allowedFunctionNames"] = []string{name}
		}
		body["toolConfig"] = map[string]any{"functionCallingConfig": config}
	}
	return json.Marshal(body)
}
func responsesBody(req *provider.ChatRequest, codex, stream bool) ([]byte, error) {
	body := map[string]any{"model": req.Model, "stream": stream}
	if codex && (req.MaxTokens != nil || req.Temperature != nil || req.TopP != nil || len(req.Stop) > 0) {
		return nil, &provider.Failure{Status: 400, KindHint: "invalid_request", Message: "Codex 登录态接口不支持 max_tokens、temperature、top_p 或 stop；请移除这些参数"}
	}
	var raw map[string]any
	_ = json.Unmarshal(req.Raw, &raw)
	if raw["previous_response_id"] != nil && raw["previous_response_id"] != "" || raw["conversation"] != nil {
		return nil, &provider.Failure{Status: 400, KindHint: "invalid_request", Message: "号池暂不支持上游会话编号续接；请通过 input 回传历史消息，避免换号后丢失上下文"}
	}
	for _, key := range []string{"reasoning", "text", "include", "parallel_tool_calls", "truncation", "store", "metadata"} {
		if value, ok := raw[key]; ok {
			body[key] = value
		}
	}
	if req.MaxTokens != nil {
		body["max_output_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.ReasoningEffort != "" {
		body["reasoning"] = map[string]any{"effort": req.ReasoningEffort}
	}
	input := []map[string]any{}
	instructions := []string{}
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			instructions = append(instructions, m.ContentText())
			continue
		}
		if m.Role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.ContentText()})
			continue
		}
		parts, err := contentParts(m.Content)
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, p := range parts {
			switch str(p, "type") {
			case "text", "input_text", "output_text":
				kind := "input_text"
				if m.Role == "assistant" {
					kind = "output_text"
				}
				out = append(out, map[string]any{"type": kind, "text": p["text"]})
			case "image", "image_url", "input_image":
				mime, data, address, err := imagePart(p)
				if err != nil {
					return nil, err
				}
				if data != "" {
					address = "data:" + mime + ";base64," + data
				}
				out = append(out, map[string]any{"type": "input_image", "image_url": address})
			default:
				return nil, fmt.Errorf("Responses 不支持此内容块转换")
			}
		}
		if len(out) > 0 {
			input = append(input, map[string]any{"type": "message", "role": m.Role, "content": out})
		}
		tc, err := calls(m.ToolCalls)
		if err != nil {
			return nil, err
		}
		for _, call := range tc {
			input = append(input, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
		}
	}
	body["instructions"] = strings.Join(instructions, "\n\n")
	body["input"] = input
	if len(req.NativeInput) > 0 {
		var original []map[string]any
		if json.Unmarshal(req.NativeInput, &original) == nil {
			preserved := []map[string]any{}
			for _, item := range original {
				if role := str(item, "role"); role != "system" && role != "developer" {
					preserved = append(preserved, item)
				}
			}
			body["input"] = preserved
		}
	}
	if codex {
		body["store"] = false
		body["stream"] = true
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		if json.Unmarshal(req.Tools, &tools) != nil {
			return nil, fmt.Errorf("工具列表无效")
		}
		for i, tool := range tools {
			if fn, ok := tool["function"].(map[string]any); ok {
				flat := map[string]any{"type": "function"}
				for _, key := range []string{"name", "description", "parameters", "strict"} {
					if value, ok := fn[key]; ok {
						flat[key] = value
					}
				}
				tools[i] = flat
			} else if tool["input_schema"] != nil {
				tools[i] = map[string]any{"type": "function", "name": tool["name"], "description": tool["description"], "parameters": tool["input_schema"]}
			}
		}
		body["tools"] = tools
	}
	if len(req.ToolChoice) > 0 {
		var choice any
		_ = json.Unmarshal(req.ToolChoice, &choice)
		if value, ok := choice.(map[string]any); ok {
			if fn, ok := value["function"].(map[string]any); ok {
				choice = map[string]any{"type": "function", "name": fn["name"]}
			} else {
				switch str(value, "type") {
				case "any":
					choice = "required"
				case "auto", "none":
					choice = value["type"]
				case "tool":
					choice = map[string]any{"type": "function", "name": value["name"]}
				}
			}
		}
		body["tool_choice"] = choice
	}
	return json.Marshal(body)
}
