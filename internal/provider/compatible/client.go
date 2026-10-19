// Package compatible 将官方 API 密钥渠道接入统一号池。
package compatible

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/workbuddy"
)

const Family = "openai-compatible"

type Config struct {
	Timeout, HeaderTimeout, IdleTimeout time.Duration
	ProxyURL                            string
}
type Client struct {
	family, defaultBaseURL string
	profile                Profile
	http                   *http.Client
	timeout, idle          time.Duration
}

func New(cfg Config) *Client {
	return NewFor(Family, "", cfg)
}

// 已核验协议的官方平台复用传输实现，保留独立渠道和默认地址。
func NewFor(family, defaultBaseURL string, cfg Config) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = cfg.HeaderTimeout
	if proxy, err := url.Parse(cfg.ProxyURL); err == nil && cfg.ProxyURL != "" {
		transport.Proxy = http.ProxyURL(proxy)
	}
	return &Client{family: family, defaultBaseURL: defaultBaseURL, profile: profileFor(family), http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, timeout: cfg.Timeout, idle: cfg.IdleTimeout}
}
func (c *Client) Family() string    { return c.family }
func (*Client) Kind() provider.Kind { return provider.KindInProcess }

// 地址与模型列表由账号配置提供；密钥仅在发给该地址时使用，不跟随重定向。
func (c *Client) DecodeCredential(raw []byte) (provider.Credential, error) {
	var input struct {
		APIKey       string   `json:"api_key"`
		BaseURL      string   `json:"base_url"`
		Models       []string `json:"models"`
		Organization string   `json:"organization"`
		Project      string   `json:"project"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return provider.Credential{}, fmt.Errorf("API 凭证必须是合法 JSON")
	}
	if strings.TrimSpace(input.BaseURL) == "" {
		input.BaseURL = c.defaultBaseURL
	}
	endpoint, err := url.Parse(strings.TrimSpace(input.BaseURL))
	if err != nil || endpoint == nil || endpoint.Hostname() == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return provider.Credential{}, fmt.Errorf("API 地址必须是无用户名、查询参数和片段的 HTTP(S) 地址")
	}
	if strings.TrimSpace(input.APIKey) == "" || strings.ContainsAny(input.APIKey+input.Organization+input.Project, "\r\n") {
		return provider.Credential{}, fmt.Errorf("API 密钥不能为空，鉴权字段不能含换行")
	}
	if strings.ContainsAny(endpoint.String(), "{}") {
		return provider.Credential{}, fmt.Errorf("请将 API 地址中的业务空间占位符替换为实际 ID")
	}
	if len(input.Models) > 2048 {
		return provider.Credential{}, fmt.Errorf("模型名称数量超出上限")
	}
	if c.profile.ModelsRequired && len(input.Models) == 0 {
		return provider.Credential{}, fmt.Errorf("该平台需要填写账号实际可用的模型 ID")
	}
	for _, model := range input.Models {
		if strings.TrimSpace(model) == "" {
			return provider.Credential{}, fmt.Errorf("模型名称不能为空")
		}
	}
	models, _ := json.Marshal(input.Models)
	return provider.Credential{Provider: c.family, Raw: append([]byte{}, raw...), Fields: map[string]string{"api_key": input.APIKey, "base_url": strings.TrimRight(endpoint.String(), "/"), "models": string(models), "organization": input.Organization, "project": input.Project}}, nil
}

func (c *Client) request(ctx context.Context, cred provider.Credential, method, path string, body []byte, stream bool) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, cred.Field("base_url")+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造 API 请求失败")
	}
	if header := c.profile.KeyHeader; header != "" {
		request.Header.Set(header, cred.Field("api_key"))
	} else {
		request.Header.Set("Authorization", "Bearer "+cred.Field("api_key"))
	}
	request.Header.Set("Content-Type", "application/json")
	if value := cred.Field("organization"); value != "" {
		request.Header.Set("OpenAI-Organization", value)
	}
	if value := cred.Field("project"); value != "" {
		request.Header.Set("OpenAI-Project", value)
	}
	client := *c.http
	if !stream {
		client.Timeout = c.timeout
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, &provider.Failure{Status: 502, Message: "API 上游连接失败", Err: ctx.Err()}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		// 上游可能回显请求，清除当前密钥后才能进入错误日志。
		clean := strings.ReplaceAll(string(raw), cred.Field("api_key"), "[已隐藏]")
		retry := time.Duration(0)
		if date, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil {
			retry = time.Until(date)
		} else if seconds, err := time.ParseDuration(response.Header.Get("Retry-After") + "s"); err == nil {
			retry = seconds
		}
		return nil, &provider.Failure{Status: response.StatusCode, Message: "API 上游请求失败", Body: clean, RetryAfter: retry}
	}
	return response, nil
}

func (c *Client) FetchCatalog(ctx context.Context, cred provider.Credential) ([]provider.ModelInfo, error) {
	var configured []string
	_ = json.Unmarshal([]byte(cred.Field("models")), &configured)
	if len(configured) == 0 {
		if c.family == "qwen" && strings.HasSuffix(cred.Field("base_url"), "/compatible-mode/v1") {
			return c.fetchQwenCatalog(ctx, cred)
		}
		response, err := c.request(ctx, cred, "GET", "/models", nil, false)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		var listing struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&listing); err != nil {
			return nil, fmt.Errorf("API 模型目录格式无效")
		}
		for _, model := range listing.Data {
			if model.ID != "" {
				configured = append(configured, model.ID)
			}
		}
	}
	out := make([]provider.ModelInfo, 0, len(configured))
	for _, model := range configured {
		out = append(out, provider.ModelInfo{NativeModel: model, PublicModel: model, DisplayName: model, Tools: true})
	}
	return out, nil
}

// 重新序列化中立消息，保证破甲替换、工具调用及消息清洗确实传到官方上游。
func chatBody(req *provider.ChatRequest, stream bool) ([]byte, error) {
	return chatBodyFor(req, stream, "")
}

// BuildChatBody 复用中立消息和函数工具校验，供订阅渠道转换其专有协议。
func BuildChatBody(req *provider.ChatRequest, stream bool) ([]byte, error) {
	return chatBody(req, stream)
}

func chatBodyFor(req *provider.ChatRequest, stream bool, family string) ([]byte, error) {
	body := map[string]any{}
	if len(req.Raw) > 0 {
		if err := json.Unmarshal(req.Raw, &body); err != nil {
			return nil, err
		}
	}
	if body == nil {
		body = map[string]any{}
	}
	// 号池可能换号，不能丢弃上一响应编号后伪装成延续对话；要求客户端回传完整历史。
	for _, key := range []string{"previous_response_id", "conversation"} {
		if value, ok := body[key]; ok && value != nil && value != "" {
			return nil, &provider.Failure{Status: 400, Message: "号池不支持上游会话编号，请在 input 或 messages 中发送完整对话历史"}
		}
	}
	if background, ok := body["background"].(bool); ok && background {
		return nil, &provider.Failure{Status: 400, Message: "本入口不支持后台异步响应，请使用同步或流式请求"}
	}
	// 网关的另外两种入口先转为中立消息，不能把其专有字段直接传给官方接口。
	for _, key := range []string{"input", "instructions", "max_output_tokens", "previous_response_id", "include", "truncation", "text", "reasoning", "system", "stop_sequences", "anthropic_version", "thinking"} {
		// DeepSeek 的原生思考开关有独立语义，不能当作 Claude 字段删除。
		if key == "thinking" && supportsThinking(family) {
			continue
		}
		delete(body, key)
	}
	messages := make([]map[string]any, 0, len(req.Messages))
	for _, message := range req.Messages {
		entry := map[string]any{"role": message.Role, "content": json.RawMessage("null")}
		if len(message.Content) > 0 {
			entry["content"] = message.Content
		}
		if message.Name != "" {
			entry["name"] = message.Name
		}
		if len(message.ToolCalls) > 0 {
			entry["tool_calls"] = message.ToolCalls
		}
		if message.ToolCallID != "" {
			entry["tool_call_id"] = message.ToolCallID
		}
		if message.ReasoningContent != "" {
			entry["reasoning_content"] = message.ReasoningContent
		}
		messages = append(messages, entry)
	}
	body["model"], body["messages"], body["stream"] = req.Model, messages, stream
	if len(req.Tools) > 0 {
		if err := validateFunctionTools(req.Tools); err != nil {
			return nil, err
		}
		body["tools"] = normalizeTools(req.Tools)
	}
	if len(req.ToolChoice) > 0 {
		body["tool_choice"] = normalizeToolChoice(req.ToolChoice)
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.MaxTokens != nil {
		if _, exists := body["max_completion_tokens"]; !exists {
			body["max_tokens"] = *req.MaxTokens
		}
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}
	if req.ReasoningEffort != "" {
		body["reasoning_effort"] = req.ReasoningEffort
	}
	if !stream {
		delete(body, "stream_options")
	}
	if err := adaptThinking(body, req, family); err != nil {
		return nil, err
	}
	return json.Marshal(body)
}

func normalizeTools(raw json.RawMessage) json.RawMessage {
	var tools []map[string]any
	if json.Unmarshal(raw, &tools) != nil {
		return raw
	}
	for i, tool := range tools {
		if _, nested := tool["function"]; nested {
			continue
		}
		fn := map[string]any{"name": tool["name"]}
		if desc, ok := tool["description"]; ok {
			fn["description"] = desc
		}
		if schema, ok := tool["input_schema"]; ok {
			fn["parameters"] = schema
		} else if schema, ok := tool["parameters"]; ok {
			fn["parameters"] = schema
		}
		if strict, ok := tool["strict"]; ok {
			fn["strict"] = strict
		}
		tools[i] = map[string]any{"type": "function", "function": fn}
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		return raw
	}
	return encoded
}
func normalizeToolChoice(raw json.RawMessage) json.RawMessage {
	var choice map[string]any
	if json.Unmarshal(raw, &choice) != nil {
		return raw
	}
	switch choice["type"] {
	case "any":
		return json.RawMessage(`"required"`)
	case "auto":
		return json.RawMessage(`"auto"`)
	case "none":
		return json.RawMessage(`"none"`)
	case "tool", "function":
		if _, nested := choice["function"]; nested {
			return raw
		}
		encoded, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": choice["name"]}})
		return encoded
	}
	return raw
}

func (c *Client) ChatNonStream(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.ChatResult, error) {
	body, err := chatBodyFor(req, false, c.family)
	if err != nil {
		return nil, err
	}
	response, err := c.request(ctx, cred, "POST", "/chat/completions", body, false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var result struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Message struct {
				Role             string          `json:"role"`
				Content          json.RawMessage `json:"content"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
				ReasoningContent string          `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Total      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&result) != nil || len(result.Choices) == 0 {
		return nil, &provider.Failure{Status: 502, Message: "API 上游返回了无效的完成响应"}
	}
	choice := result.Choices[0]
	return &provider.ChatResult{ID: result.ID, Model: result.Model, Created: result.Created, FinishReason: choice.FinishReason, Message: provider.Message{Role: choice.Message.Role, Content: choice.Message.Content, ToolCalls: choice.Message.ToolCalls, ReasoningContent: choice.Message.ReasoningContent}, Usage: provider.Usage{PromptTokens: result.Usage.Prompt, CompletionTokens: result.Usage.Completion, TotalTokens: result.Usage.Total}}, nil
}

func (c *Client) ChatStream(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.Stream, error) {
	body, err := chatBodyFor(req, true, c.family)
	if err != nil {
		return nil, err
	}
	response, err := c.request(ctx, cred, "POST", "/chat/completions", body, true)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		response.Body.Close()
		return nil, &provider.Failure{Status: 502, Message: "API 上游未返回 SSE 流"}
	}
	return &provider.Stream{Body: workbuddy.WithIdleTimeout(response.Body, c.idle), ContentType: "text/event-stream", Model: req.Model}, nil
}
func (*Client) Checkin(context.Context, provider.Credential) (provider.CheckinResult, error) {
	return provider.CheckinResult{}, provider.ErrUnsupported
}
func (*Client) Refresh(context.Context, provider.Credential) (provider.Credential, error) {
	return provider.Credential{}, provider.ErrUnsupported
}
