package subscriptions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/compatible"
	"github.com/relaypool/relaypool/internal/provider/workbuddy"
)

const traeIntlPrefix = "/api/remote/v1"

// 国际 Web SOLO 协议依据固定 MIT 源码改写；每次调用建立独立会话，不创建项目。
func traeIntlHeaders(cred provider.Credential) http.Header {
	h := http.Header{"Content-Type": {"application/json"}, "Authorization": {"Cloud-IDE-JWT " + cred.Field("access_token")}, "X-Trae-Client-Type": {"web"}}
	h.Set("X-Preferenced-Language", cred.Field("app_language"))
	h.Set("X-User-Region", cred.Field("upstream_region"))
	h.Set("Origin", cred.Field("referer_origin"))
	h.Set("Referer", cred.Field("referer_origin")+"/")
	h.Set("User-Agent", "Mozilla/5.0 Chrome/149.0.0.0 Safari/537.36")
	if timezone := cred.Field("timezone"); timezone != "" {
		h.Set("X-Trae-User-Timezone", timezone)
	}
	return h
}

func (c *Client) traeIntlBody(cred provider.Credential, req *provider.ChatRequest) ([]byte, error) {
	if req == nil {
		return nil, failure(400, "国际会话请求不能为空")
	}
	neutral, err := compatible.BuildChatBody(req, true)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if json.Unmarshal(neutral, &body) != nil {
		return nil, failure(400, "国际会话请求格式无效")
	}
	if tools, _ := body["tools"].([]any); len(tools) > 0 {
		return nil, failure(400, "Trae 国际远程会话尚不支持客户端函数工具")
	}
	if req.DisableThinking || req.ReasoningEffort != "" || req.Temperature != nil || req.TopP != nil || req.MaxTokens != nil || len(req.Stop) > 0 {
		return nil, failure(400, "Trae 国际远程会话尚不支持采样、输出长度或思考控制")
	}
	var original map[string]any
	_ = json.Unmarshal(req.Raw, &original)
	if format := object(original["response_format"]); format["type"] != nil && format["type"] != "text" {
		return nil, failure(400, "国际会话不支持强制结构化输出")
	}
	if format := object(object(original["text"])["format"]); format["type"] != nil && format["type"] != "text" {
		return nil, failure(400, "国际会话不支持强制结构化输出")
	}
	if strings.Contains(string(req.NativeInput), "encrypted_content") {
		return nil, failure(400, "国际会话不支持加密推理回放")
	}
	var parts []string
	messages, _ := body["messages"].([]any)
	for _, value := range messages {
		message := object(value)
		role, _ := message["role"].(string)
		if calls, _ := message["tool_calls"].([]any); len(calls) > 0 || role == "tool" {
			return nil, failure(400, "国际会话不能丢弃客户端工具历史，请使用支持函数工具的渠道")
		}
		text, isText := message["content"].(string)
		if !isText {
			list, ok := message["content"].([]any)
			if !ok {
				return nil, failure(400, "国际会话仅接受可读文本历史")
			}
			for _, part := range list {
				p := object(part)
				if p["type"] != "text" {
					return nil, failure(400, "国际会话暂不支持图片或其他非文本输入")
				}
				t, _ := p["text"].(string)
				text += t
			}
		}
		switch role {
		case "system", "developer":
			text = "[System]\n" + text
		case "assistant":
			text = "[Assistant]\n" + text
		case "user":
		default:
			return nil, failure(400, "国际会话不支持该消息角色")
		}
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return nil, failure(400, "国际会话需要文本消息")
	}
	mode, strategy, model := "code", "manual", req.Model
	switch strings.ToLower(model) {
	case "auto", "":
		strategy, model = "auto", ""
	case "work", "auto-work", "solo-work":
		mode, strategy, model = "work", "auto", ""
	}
	common := map[string]any{"language": "en-us", "quality": "stable", "is_freshman": "0", "is_privacy_mode": 0, "privacy_mode": "off", "solo_chat_mode": mode}
	for _, name := range []string{"app_language", "app_version", "web_id", "user_identity", "biz_user_id", "user_unique_id", "scope", "tenant"} {
		common[name] = cred.Field(name)
	}
	common["region"], common["aiRegion"] = cred.Field("upstream_region"), cred.Field("upstream_region")
	query := string(marshal([]any{map[string]any{"type": "text", "data": map[string]any{"content": strings.Join(parts, "\n\n")}}}))
	return json.Marshal(map[string]any{"mode": mode, "environment_id": "default", "initial_message": map[string]any{"chat_session_id": "", "content": []any{}, "query": query, "model_name": model, "agent_type": "solo_agent_remote", "model_selection_strategy": strategy, "common_params": string(marshal(common))}, "env": "remote", "auto_create_project": false, "origin": "web"})
}

// 会话已经创建或受理结果不明时，禁止整个请求自动换号，避免重复远程任务。
func remoteCommitted(err error) error {
	if f, ok := provider.AsFailure(err); ok {
		copy := *f
		copy.NoRetry = true
		copy.Message = "国际远程会话已受理或结果不明，未自动重新创建；" + f.Message
		return &copy
	}
	return &provider.Failure{Status: 502, Code: "remote_session_interrupted", Message: "国际远程会话已受理或结果不明，未自动重新创建", NoRetry: true}
}

func remoteID(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

type remoteBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *remoteBody) Close() error { b.cancel(); return b.ReadCloser.Close() }

func (c *Client) chatTraeIntl(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.Stream, error) {
	body, err := c.traeIntlBody(cred, req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	success := false
	defer func() {
		if !success {
			cancel()
		}
	}()
	response, err := c.do(ctx, cred, "POST", cred.Field("base_url")+traeIntlPrefix+"/chat_sessions", body, traeIntlHeaders(cred), false)
	if err != nil {
		if f, ok := provider.AsFailure(err); ok && f.Code == "" {
			return nil, remoteCommitted(err)
		}
		return nil, err
	}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Session string `json:"chat_session_id"`
			Message string `json:"message_id"`
		} `json:"data"`
	}
	err = decodeLimited(response.Body, &envelope)
	response.Body.Close()
	if err != nil {
		return nil, remoteCommitted(err)
	}
	if envelope.Code != 0 {
		return nil, c.upstreamFailure(cred, 502, string(marshal(envelope.Code)), nil, nil)
	}
	if !remoteID(envelope.Data.Session) || !remoteID(envelope.Data.Message) {
		return nil, remoteCommitted(failure(502, "国际会话响应缺少有效会话或消息编号"))
	}
	endpoint := cred.Field("base_url") + traeIntlPrefix + "/chat_sessions/" + url.PathEscape(envelope.Data.Session) + "/events?reply_to_message_id=" + url.QueryEscape(envelope.Data.Message)
	headers := traeIntlHeaders(cred)
	headers.Set("Accept", "text/event-stream")
	response, err = c.do(ctx, cred, "GET", endpoint, nil, headers, true)
	if err != nil {
		return nil, remoteCommitted(err)
	}
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		response.Body.Close()
		return nil, remoteCommitted(failure(502, "国际会话事件接口未返回 SSE"))
	}
	reader := newSubscriptionStream(&remoteBody{ReadCloser: workbuddy.WithIdleTimeout(response.Body, c.idle), cancel: cancel}, c, cred, req.Model)
	reader.plans = map[string]string{}
	if err := reader.prime(); err != nil {
		reader.Close()
		return nil, remoteCommitted(err)
	}
	success = true
	return &provider.Stream{Body: reader, Model: req.Model, ContentType: "text/event-stream"}, nil
}

func (c *Client) traeIntlCatalog(ctx context.Context, cred provider.Credential) ([]provider.ModelInfo, error) {
	response, err := c.do(ctx, cred, "GET", cred.Field("base_url")+traeIntlPrefix+"/models", nil, traeIntlHeaders(cred), false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var result struct {
		Code int `json:"code"`
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := decodeLimited(response.Body, &result); err != nil {
		return nil, err
	}
	if result.Code != 0 {
		return nil, c.upstreamFailure(cred, 502, string(marshal(result.Code)), nil, nil)
	}
	if len(result.Data) == 0 {
		return nil, failure(502, "国际会话型号目录为空，未猜测套餐权限")
	}
	out := []provider.ModelInfo{}
	seen := map[string]bool{}
	for _, entry := range result.Data {
		if entry.Name != "" && !seen[entry.Name] {
			seen[entry.Name] = true
			out = append(out, provider.ModelInfo{NativeModel: entry.Name, PublicModel: entry.Name, DisplayName: entry.Name})
		}
	}
	if len(out) == 0 {
		return nil, failure(502, "国际会话型号目录没有有效名称")
	}
	for _, alias := range []string{"auto", "work"} {
		if !seen[alias] {
			out = append(out, provider.ModelInfo{NativeModel: alias, PublicModel: alias, DisplayName: alias + "（上游自动选择）"})
		}
	}
	return out, nil
}
