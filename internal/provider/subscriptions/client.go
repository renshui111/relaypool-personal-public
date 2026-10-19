// Package subscriptions 实现 Qoder 桌面协议与 Trae 国内、国际聊天登录态。
package subscriptions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/native"
	"github.com/relaypool/relaypool/internal/provider/workbuddy"
)

type Config struct {
	Timeout, HeaderTimeout, IdleTimeout time.Duration
	ProxyURL                            string
}

type Client struct {
	family        string
	http          *http.Client
	timeout, idle time.Duration
	mu            sync.Mutex
	refreshes     map[string]*refreshCall
	sessions      map[string]sessionEntry
}

type refreshCall struct {
	done       chan struct{}
	credential provider.Credential
	err        error
	finished   time.Time
}

type sessionEntry struct {
	session *cosySession
	used    time.Time
}

func NewQoder(cfg Config) *Client    { return newClient("qoder", cfg) }
func NewTrae(cfg Config) *Client     { return newClient("trae", cfg) }
func NewTraeIntl(cfg Config) *Client { return newClient("trae-intl", cfg) }
func newClient(family string, cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = 30 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: cfg.HeaderTimeout, MaxIdleConns: 100, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second}
	if cfg.ProxyURL != "" {
		if u, err := url.Parse(cfg.ProxyURL); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &Client{family: family, http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, timeout: cfg.Timeout, idle: cfg.IdleTimeout, refreshes: map[string]*refreshCall{}, sessions: map[string]sessionEntry{}}
}
func (c *Client) Family() string    { return c.family }
func (*Client) Kind() provider.Kind { return provider.KindInProcess }

// 刷新策略由渠道声明，新增渠道无需继续扩展网关中的平台名称判断。
func (*Client) NeedsRefresh(cred provider.Credential) bool {
	canRefresh := cred.Field("refresh_token") != "" || cred.Field("personal_token") != ""
	return cred.Field("access_token") == "" || (!cred.ExpiresAt.IsZero() && time.Until(cred.ExpiresAt) < time.Minute && canRefresh)
}

func credentialKey(cred provider.Credential) string {
	sum := sha256.Sum256(cred.Raw)
	return cred.AccountID + ":" + string(cred.Region) + ":" + hex.EncodeToString(sum[:])
}

func failure(status int, message string) error {
	f := &provider.Failure{Status: status, Message: message}
	// 本地能力校验失败属于请求问题，不能冷却正常账号或继续换号。
	if status == http.StatusBadRequest {
		f.KindHint, f.Code = "invalid_request", "invalid_request"
	}
	return f
}

func (c *Client) do(ctx context.Context, cred provider.Credential, method, endpoint string, body []byte, headers http.Header, stream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, failure(400, "订阅渠道请求地址无效")
	}
	req.Header = headers
	client := *c.http
	if !stream {
		client.Timeout = c.timeout
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, &provider.Failure{Status: 502, Message: "订阅上游连接失败", Err: ctx.Err()}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return nil, c.upstreamFailure(cred, response.StatusCode, "", payload, response.Header)
	}
	return response, nil
}

func (c *Client) upstreamFailure(cred provider.Credential, status int, code string, payload []byte, headers http.Header) error {
	// 错误正文可能回显登录态，只保留分类信息，不将正文交给页面或日志。
	kind := ""
	text := strings.ToLower(string(payload))
	if reported, err := strconv.Atoi(code); err == nil && reported >= 400 && reported <= 599 {
		status = reported
	}
	if status < 400 {
		status = 502
	}
	// 错误码也是不可信上游数据，仅保留协议中的数值分类，防止回显令牌或任意正文。
	safeCode := "subscription_upstream_error"
	if reported, err := strconv.Atoi(code); err == nil && ((reported >= 400 && reported <= 599) || code == "4010" || code == "4001" || code == "6001") {
		safeCode = strconv.Itoa(reported)
	}
	switch {
	case status == 401 || code == "401" || code == "4010" || strings.Contains(text, "token expired") || strings.Contains(text, "invalid token"):
		status, kind = 401, "auth"
	case strings.Contains(text, "quota") || strings.Contains(text, "insufficient") || code == "6001":
		kind = "quota"
	case status == 429:
		kind = "rate_limit"
	case status == 400 || code == "4001":
		status, kind = 400, "invalid_request"
	}
	retry := time.Duration(0)
	if headers != nil {
		if seconds, err := time.ParseDuration(headers.Get("Retry-After") + "s"); err == nil {
			retry = seconds
		} else if date, err := http.ParseTime(headers.Get("Retry-After")); err == nil {
			retry = time.Until(date)
		}
	}
	return &provider.Failure{Status: status, KindHint: kind, Code: safeCode, Message: fmt.Sprintf("%s 上游请求失败，请检查登录态、型号与套餐权限", c.family), RetryAfter: retry}
}

func decodeLimited(body io.Reader, out any) error {
	raw, err := io.ReadAll(io.LimitReader(body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 || json.Unmarshal(raw, out) != nil {
		return failure(502, "订阅上游返回了无效或过大的数据")
	}
	return nil
}

func (c *Client) ChatNonStream(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.ChatResult, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	stream, err := c.ChatStream(ctx, cred, req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	result, err := native.CollectChatStream(stream.Body, req.Model)
	if err != nil && c.family == "trae-intl" {
		return nil, remoteCommitted(err)
	}
	return result, err
}

func (c *Client) ChatStream(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.Stream, error) {
	if cred.Field("access_token") == "" || cred.Field("uid") == "" {
		return nil, provider.ErrNotReady
	}
	if c.family == "trae-intl" {
		return c.chatTraeIntl(ctx, cred, req)
	}
	body, err := c.chatBody(cred, req)
	if err != nil {
		return nil, err
	}
	endpoint, headers := "", http.Header{}
	if c.family == "qoder" {
		endpoint = cred.Field("base_url") + qoderChatPath
		body = []byte(qoderEncode(body))
		headers, err = c.qoderHeaders(cred, endpoint, body, req.Model)
	} else {
		endpoint = cred.Field("base_url") + traeChatPath
		headers = traeHeaders(cred, true)
	}
	if err != nil {
		return nil, err
	}
	response, err := c.do(ctx, cred, "POST", endpoint, body, headers, true)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		response.Body.Close()
		return nil, failure(502, "订阅上游未返回 SSE 流")
	}
	reader := newSubscriptionStream(workbuddy.WithIdleTimeout(response.Body, c.idle), c, cred, req.Model)
	// 首帧错误在 HTTP 成功发给客户端前上报，使号池可以执行换号。
	if err := reader.prime(); err != nil {
		reader.Close()
		return nil, err
	}
	return &provider.Stream{Body: reader, Model: req.Model, ContentType: "text/event-stream"}, nil
}
