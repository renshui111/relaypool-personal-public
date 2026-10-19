// Package native 将平台原生协议转换为号池统一的消息和流格式。
package native

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/workbuddy"
)

type Config struct {
	Timeout, HeaderTimeout, IdleTimeout time.Duration
	ProxyURL                            string
	// 仅供测试替换固定刷新地址；账号凭证不能指定刷新服务器。
	TokenURL string
}
type tokenUpdate struct {
	fields map[string]any
	at     time.Time
}
type tokenFlight struct {
	done   chan struct{}
	update tokenUpdate
	err    error
}
type Client struct {
	family        string
	http          *http.Client
	timeout, idle time.Duration
	tokenURL      string
	refreshMu     sync.Mutex
	refreshed     map[string]tokenUpdate
	flights       map[string]*tokenFlight
}

func newClient(family string, cfg Config) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = cfg.HeaderTimeout
	if cfg.ProxyURL != "" {
		if u, err := url.Parse(cfg.ProxyURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &Client{family: family, http: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, timeout: cfg.Timeout, idle: cfg.IdleTimeout, tokenURL: cfg.TokenURL, refreshed: map[string]tokenUpdate{}, flights: map[string]*tokenFlight{}}
}
func NewClaude(cfg Config) *Client  { return newClient("claude", cfg) }
func NewGemini(cfg Config) *Client  { return newClient("gemini", cfg) }
func NewOpenAI(cfg Config) *Client  { return newClient("openai", cfg) }
func NewCodex(cfg Config) *Client   { return newClient("codex", cfg) }
func (c *Client) Family() string    { return c.family }
func (*Client) Kind() provider.Kind { return provider.KindInProcess }
func (*Client) Checkin(context.Context, provider.Credential) (provider.CheckinResult, error) {
	return provider.CheckinResult{}, provider.ErrUnsupported
}

func baseURL(value, fallback string) (string, error) {
	if strings.TrimSpace(value) == "" {
		value = fallback
	}
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("上游地址须为无用户名、查询参数及片段的 HTTP(S) 地址")
	}
	return strings.TrimRight(u.String(), "/"), nil
}
func str(m map[string]any, key string) string {
	value, _ := m[key].(string)
	return strings.TrimSpace(value)
}
func textValue(m map[string]any, key string) string { value, _ := m[key].(string); return value }
func marshal(value any) json.RawMessage             { out, _ := json.Marshal(value); return out }
func cleanError(raw string, cred provider.Credential) string {
	for _, key := range []string{"api_key", "access_token", "refresh_token", "client_secret", "id_token"} {
		if value := cred.Field(key); value != "" {
			raw = strings.ReplaceAll(raw, value, "[已隐藏]")
		}
	}
	return raw
}
func (c *Client) request(ctx context.Context, cred provider.Credential, method, endpoint string, body []byte, stream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造平台请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "relaypool")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	switch c.family {
	case "claude":
		req.Header.Set("anthropic-version", "2023-06-01")
		if cred.Field("api_key") != "" {
			req.Header.Set("x-api-key", cred.Field("api_key"))
		} else {
			req.Header.Set("Authorization", "Bearer "+cred.Field("access_token"))
			req.Header.Set("anthropic-beta", "oauth-2025-04-20")
		}
	case "gemini":
		if cred.Field("api_key") != "" {
			req.Header.Set("x-goog-api-key", cred.Field("api_key"))
		} else {
			req.Header.Set("Authorization", "Bearer "+cred.Field("access_token"))
		}
	case "codex":
		req.Header.Set("Authorization", "Bearer "+cred.Field("access_token"))
		req.Header.Set("ChatGPT-Account-Id", cred.Field("account_id"))
	default:
		req.Header.Set("Authorization", "Bearer "+cred.Field("api_key"))
	}
	client := *c.http
	if !stream {
		client.Timeout = c.timeout
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, &provider.Failure{Status: 502, Message: "平台上游连接失败", Err: ctx.Err()}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		retry := time.Duration(0)
		if t, e := http.ParseTime(res.Header.Get("Retry-After")); e == nil {
			retry = time.Until(t)
		} else if d, e := time.ParseDuration(res.Header.Get("Retry-After") + "s"); e == nil {
			retry = d
		}
		return nil, &provider.Failure{Status: res.StatusCode, Message: "平台上游拒绝请求", Body: cleanError(string(raw), cred), RetryAfter: retry}
	}
	if stream && !strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "text/event-stream") {
		res.Body.Close()
		return nil, &provider.Failure{Status: 502, Message: "平台未返回 SSE 流"}
	}
	return res, nil
}
func decodeResponse(res *http.Response, target any) error {
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, (16<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 16<<20 || json.Unmarshal(raw, target) != nil {
		return &provider.Failure{Status: 502, Message: "平台响应格式无效或超过 16 MB"}
	}
	return nil
}
func (c *Client) FetchCatalog(ctx context.Context, cred provider.Credential) ([]provider.ModelInfo, error) {
	var names []string
	_ = json.Unmarshal([]byte(cred.Field("models")), &names)
	if len(names) > 0 {
		return modelInfos(names), nil
	}
	if c.family == "codex" || cred.Field("mode") == "code_assist" {
		return nil, fmt.Errorf("此订阅渠道需要填写该账号实际可用的模型名称")
	}
	endpoint := cred.Field("base_url") + "/models"
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name    string   `json:"name"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
		NextPageToken string `json:"nextPageToken"`
		HasMore       bool   `json:"has_more"`
		LastID        string `json:"last_id"`
	}
	// 分页有上限，防止远端循环游标造成无限读取。
	for page := 0; page < 32; page++ {
		res, err := c.request(ctx, cred, "GET", endpoint, nil, false)
		if err != nil {
			return nil, err
		}
		out.NextPageToken = ""
		out.HasMore = false
		out.Data = nil
		out.Models = nil
		if err := decodeResponse(res, &out); err != nil {
			return nil, err
		}
		for _, m := range out.Data {
			if m.ID != "" {
				names = append(names, m.ID)
			}
		}
		for _, m := range out.Models {
			for _, method := range m.Methods {
				if method == "generateContent" {
					names = append(names, strings.TrimPrefix(m.Name, "models/"))
					break
				}
			}
		}
		if c.family == "gemini" && out.NextPageToken != "" {
			endpoint = cred.Field("base_url") + "/models?pageToken=" + url.QueryEscape(out.NextPageToken)
			continue
		}
		if c.family == "claude" && out.HasMore && out.LastID != "" {
			endpoint = cred.Field("base_url") + "/models?after_id=" + url.QueryEscape(out.LastID)
			continue
		}
		return modelInfos(names), nil
	}
	return nil, fmt.Errorf("模型目录分页超出上限")
}
func modelInfos(names []string) []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			// 目录只证明模型存在，不据此虚构每个型号的工具或图片能力。
			out = append(out, provider.ModelInfo{NativeModel: name, PublicModel: name, DisplayName: name})
		}
	}
	return out
}
func (c *Client) ChatStream(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.Stream, error) {
	body, endpoint, err := c.chatRequest(cred, req, true)
	if err != nil {
		return nil, err
	}
	res, err := c.request(ctx, cred, "POST", endpoint, body, true)
	if err != nil {
		return nil, err
	}
	protocol := ""
	if c.family == "claude" {
		protocol = "anthropic"
	}
	if c.family == "openai" || c.family == "codex" {
		protocol = "responses"
	}
	return &provider.Stream{Body: newTranslatedStream(workbuddy.WithIdleTimeout(res.Body, c.idle), c.family, req.Model, cred), ContentType: "text/event-stream", Model: req.Model, NativeProtocol: protocol}, nil
}
func (c *Client) ChatNonStream(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.ChatResult, error) {
	if c.family == "codex" {
		// Codex 登录态接口只返回流；收集同一转换流供非流式客户端使用。
		if c.timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.timeout)
			defer cancel()
		}
		stream, err := c.ChatStream(ctx, cred, req)
		if err != nil {
			return nil, err
		}
		defer stream.Close()
		return collectStream(stream.Body, req.Model)
	}
	body, endpoint, err := c.chatRequest(cred, req, false)
	if err != nil {
		return nil, err
	}
	res, err := c.request(ctx, cred, "POST", endpoint, body, false)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := decodeResponse(res, &result); err != nil {
		return nil, err
	}
	converted, err := convertResult(c.family, result, req.Model)
	if failure, ok := provider.AsFailure(err); ok {
		failure.Body = cleanError(failure.Body, cred)
	}
	return converted, err
}
