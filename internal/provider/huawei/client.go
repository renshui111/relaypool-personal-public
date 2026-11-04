// Package huawei adapts CodeArts and OfficeAce model services to the local pool.
// Protocol references and third-party attributions are recorded in NOTICE.md.
package huawei

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/compatible"
	"github.com/relaypool/relaypool/internal/provider/native"
)

const (
	CodeArts      = "codearts"
	OfficeAce     = "officeace"
	snapBase      = "https://snap-access.cn-north-4.myhuaweicloud.com"
	officeControl = "https://agentarts.cn-southwest-2.myhuaweicloud.com"
	tokenURL      = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
)

type Client struct {
	*compatible.Client
	family  string
	timeout time.Duration
	http    *http.Client
	// Fixed endpoints are instance-owned so tests do not mutate global state.
	snap, control, token, portal, auth string
	refreshMu                          sync.Mutex
	refreshed                          map[string]refreshResult
}
type refreshResult struct {
	cred  provider.Credential
	until time.Time
}

func New(family string, cfg compatible.Config) *Client {
	c := &Client{family: family, timeout: cfg.Timeout, snap: snapBase, control: officeControl, token: tokenURL,
		portal: "https://codearts.huaweicloud.com/portal", auth: "https://auth.huaweicloud.com", refreshed: map[string]refreshResult{}}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = cfg.HeaderTimeout
	if cfg.ProxyURL != "" {
		if proxy, err := url.Parse(cfg.ProxyURL); err == nil {
			transport.Proxy = http.ProxyURL(proxy)
		}
	}
	c.http = &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cfg.Authenticate = c.authenticate
	cfg.RedactError = redact
	c.Client = compatible.NewFor(family, "", cfg)
	return c
}

// DecodeCredential accepts explicit credentials only, never scans a user's files.
// Aliases cover exported CodeArts sessions and OfficeAce gateway exports.
func (c *Client) DecodeCredential(raw []byte) (provider.Credential, error) {
	var data map[string]json.RawMessage
	if len(raw) > 1<<20 || json.Unmarshal(raw, &data) != nil || data == nil {
		return provider.Credential{}, fmt.Errorf("华为凭证须为 JSON 对象")
	}
	fields := map[string]string{}
	pick := func(keys ...string) string {
		for _, k := range keys {
			var v string
			if json.Unmarshal(data[k], &v) == nil && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	fields["access_key"] = pick("access_key", "access_key_id", "accessKey", "ak")
	fields["secret_key"] = pick("secret_key", "secret_access_key", "secretKey", "sk")
	fields["security_token"] = pick("security_token", "securitytoken", "securityToken")
	fields["refresh_token"] = pick("refresh_token")
	fields["code_verifier"] = pick("code_verifier")
	fields["credential_kind"] = pick("credential_kind")
	fields["app_key"] = pick("model_app_key", "app_key", "appKey")
	fields["app_secret"] = pick("model_app_secret", "app_secret", "appSecret")
	fields["project_id"] = pick("project_id")
	fields["base_url"] = pick("model_base_url", "model_api_url_base", "base_url", "baseUrl")
	for _, k := range []string{"dpop_private_jwk"} {
		if v := data[k]; len(v) > 0 && string(v) != "null" {
			fields[k] = string(v)
		}
	}
	if len(data["models"]) > 0 {
		var models []string
		if json.Unmarshal(data["models"], &models) != nil || len(models) > 2048 {
			return provider.Credential{}, fmt.Errorf("models 须为型号字符串数组")
		}
		for _, m := range models {
			if strings.TrimSpace(m) == "" || len(m) > 256 || strings.ContainsAny(m, "\r\n") {
				return provider.Credential{}, fmt.Errorf("型号格式无效")
			}
		}
		fields["models"] = string(data["models"])
	}
	var expiry time.Time
	if v := pick("expires_at", "expiration"); v != "" {
		var err error
		expiry, err = time.Parse(time.RFC3339, v)
		if err != nil {
			return provider.Credential{}, fmt.Errorf("expires_at 须为带时区的 RFC3339 时间")
		}
	}
	if c.family == CodeArts {
		if fields["access_key"] == "" || fields["secret_key"] == "" {
			return provider.Credential{}, fmt.Errorf("码道需要 access_key 和 secret_key，不接受普通 Bearer API Key")
		}
		if fields["credential_kind"] == "aksk" {
			if fields["security_token"] != "" {
				return provider.Credential{}, fmt.Errorf("永久 AK/SK 不应混入临时 security_token")
			}
			expiry = time.Time{}
		} else if fields["security_token"] == "" || expiry.IsZero() {
			return provider.Credential{}, fmt.Errorf("临时码道凭证需要 security_token 和 expires_at；永久凭证须显式声明 credential_kind=aksk")
		}
		if fields["base_url"] == "" {
			fields["base_url"] = c.snap + "/api/v2"
		}
	} else {
		if fields["app_key"] == "" || fields["app_secret"] == "" || fields["base_url"] == "" {
			return provider.Credential{}, fmt.Errorf("果办需要 model_app_key、model_app_secret 和账号下发的 model_base_url；不能用码道 AK/SK 代替")
		}
		// STS expiry does not expire the separate OfficeAce inference credential.
		fields["sts_expires_at"] = expiry.Format(time.RFC3339)
		expiry = time.Time{}
		base := strings.TrimRight(fields["base_url"], "/")
		if !strings.HasSuffix(base, "/v2") {
			base += "/v2"
		}
		fields["base_url"] = base
	}
	for k, v := range fields {
		if k != "models" && k != "dpop_private_jwk" && strings.ContainsAny(v, "\r\n\x00") {
			return provider.Credential{}, fmt.Errorf("凭证字段不能包含控制字符")
		}
	}
	u, err := url.Parse(fields["base_url"])
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"))) {
		return provider.Credential{}, fmt.Errorf("上游地址须为 HTTPS；仅本机测试允许 HTTP，不得包含用户信息、参数或片段")
	}
	fields["base_url"] = strings.TrimRight(u.String(), "/")
	return provider.Credential{Provider: c.family, Region: provider.RegionCN, Raw: append([]byte(nil), raw...), Fields: fields, ExpiresAt: expiry}, nil
}

func redact(s string, cred provider.Credential) string {
	secrets := []string{}
	for _, k := range []string{"access_key", "secret_key", "security_token", "refresh_token", "app_key", "app_secret", "code_verifier", "dpop_private_jwk"} {
		if v := cred.Field(k); v != "" {
			secrets = append(secrets, v)
		}
	}
	if cred.Field("app_key") != "" {
		secrets = append(secrets, base64.StdEncoding.EncodeToString([]byte(cred.Field("app_key")+":"+cred.Field("app_secret"))))
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, v := range secrets {
		s = strings.ReplaceAll(s, v, "[已隐藏]")
	}
	return s
}

func (c *Client) authenticate(r *http.Request, cred provider.Credential, body []byte) error {
	r.Header.Del("Authorization")
	if c.family == OfficeAce {
		r.SetBasicAuth(cred.Field("app_key"), cred.Field("app_secret"))
		return nil
	}
	if !cred.ExpiresAt.IsZero() && !time.Now().Before(cred.ExpiresAt) {
		return &provider.Failure{Status: 401, KindHint: "auth", Message: "码道临时凭证已过期，请重新授权或更新登录态"}
	}
	var request struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &request)
	session := sha256.Sum256([]byte(cred.AccountID + ":" + cred.Field("access_key")))
	for k, v := range map[string]string{"app-id": "CodeAgent3.0", "plugin-name": "codemate_vscode", "plugin-version": "26.9.902", "ide-name": "VSCode", "ide-version": "26.9.902", "x-ot-function": "agent-general", "x-ot-client-type": "IDE", "x-ot-client-version": "26.9.902", "x-language": "zh-cn", "x-ot-session-id": "ses_" + hex.EncodeToString(session[:12]), "user-session-id": "ses_" + hex.EncodeToString(session[:12]), "is_confidential": "false"} {
		r.Header.Set(k, v)
	}
	if request.Model != "" {
		r.Header.Set("model-id", request.Model)
		r.Header.Set("model-name", request.Model)
	}
	trace, err := randomText(16)
	if err != nil {
		return fmt.Errorf("生成请求标识失败")
	}
	span, err := randomText(8)
	if err != nil {
		return fmt.Errorf("生成请求标识失败")
	}
	for k, v := range map[string]string{"x-ot-trace-id": trace, "x-ot-span-id": span, "x-snap-traceid": trace + "_" + span, "user-msg-id": "msg_" + trace, "created-time": time.Now().UTC().Format(time.RFC3339)} {
		r.Header.Set(k, v)
	}
	return sign(r, body, cred)
}

func (c *Client) jsonRequest(ctx context.Context, cred provider.Credential, method, endpoint string, body []byte, control bool) ([]byte, error) {
	r, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("无法构造华为请求")
	}
	r.Header.Set("Content-Type", "application/json")
	if control {
		err = sign(r, body, cred)
	} else {
		err = c.authenticate(r, cred, body)
	}
	if err != nil {
		return nil, err
	}
	if c.family == CodeArts && strings.HasSuffix(r.URL.Path, "/model/builtin") {
		r.Header.Set("Agent-Type", "PromptCenter")
		if err = sign(r, body, cred); err != nil {
			return nil, err
		}
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, &provider.Failure{Status: 502, Message: "华为上游连接失败"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("华为响应读取失败")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, failure(resp.StatusCode, redact(string(raw), cred))
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, &provider.Failure{Status: 502, Message: "华为上游返回无效 JSON"}
	}
	if e := envelope["error"]; len(e) > 0 && string(e) != "null" {
		return nil, failure(502, redact(string(raw), cred))
	}
	return raw, nil
}

func (c *Client) FetchCatalog(ctx context.Context, cred provider.Credential) ([]provider.ModelInfo, error) {
	var configured []string
	_ = json.Unmarshal([]byte(cred.Field("models")), &configured)
	if len(configured) > 0 {
		out := []provider.ModelInfo{}
		for _, m := range configured {
			out = append(out, provider.ModelInfo{NativeModel: m, PublicModel: m, DisplayName: m})
		}
		return out, nil
	}
	endpoint := cred.Field("base_url") + "/models"
	if c.family == CodeArts {
		u, _ := url.Parse(cred.Field("base_url"))
		u.Path = "/v1/model/builtin"
		endpoint = u.String()
	}
	raw, err := c.jsonRequest(ctx, cred, "GET", endpoint, nil, false)
	if err != nil {
		return nil, err
	}
	var root struct {
		Builtin []json.RawMessage `json:"builtinModels"`
		Data    []json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &root)
	items := root.Data
	if c.family == CodeArts {
		items = root.Builtin
	}
	if items == nil {
		return nil, &provider.Failure{Status: 502, Message: "华为模型目录缺少预期的数组字段"}
	}
	out := []provider.ModelInfo{}
	seen := map[string]bool{}
	for _, raw := range items {
		var m struct {
			ID       string `json:"id"`
			ModelID  string `json:"model_id"`
			Name     string `json:"model_name"`
			Enabled  *bool  `json:"enable"`
			Display  *bool  `json:"display_enabled"`
			Category string `json:"model_category"`
			Images   bool   `json:"supports_images"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if m.Enabled != nil && !*m.Enabled || m.Display != nil && !*m.Display {
			continue
		}
		id := m.ID
		if c.family == CodeArts {
			id = m.ModelID
		}
		if id == "" || seen[id] || strings.Contains(strings.ToLower(m.Category), "embedding") {
			continue
		}
		seen[id] = true
		out = append(out, provider.ModelInfo{NativeModel: id, PublicModel: id, DisplayName: m.Name, Images: m.Images})
	}
	return out, nil
}

func (c *Client) ChatStream(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.Stream, error) {
	stream, err := c.Client.ChatStream(ctx, cred, req)
	if err != nil {
		if f, ok := provider.AsFailure(err); ok {
			body := f.Body
			if body == "" {
				body = f.Message
			}
			classified := failure(f.Status, redact(body, cred))
			classified.RetryAfter = f.RetryAfter
			return nil, classified
		}
		return nil, err
	}
	body := newCheckedStream(stream.Body, cred)
	if err := body.prime(); err != nil {
		body.Close()
		return nil, err
	}
	stream.Body = body
	return stream, nil
}
func (c *Client) ChatNonStream(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.ChatResult, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	// Both upstreams speak OpenAI SSE. Aggregate that same path to keep error,
	// tool, reasoning and usage semantics identical in all client protocols.
	request := *req
	request.Stream = true
	stream, err := c.ChatStream(ctx, cred, &request)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return native.CollectChatStream(io.LimitReader(stream.Body, 32<<20), req.Model)
}

func failure(status int, body string) *provider.Failure {
	kind := ""
	code := "huawei_upstream_error"
	switch {
	case strings.Contains(body, "81004"), strings.Contains(body, "81009"):
		kind = "model_not_available"
		status = 404
	case strings.Contains(body, "81111"), strings.Contains(body, "81114"):
		kind = "rate_limit"
		status = 429
	case strings.Contains(body, "81113"):
		kind = "invalid_request"
		status = 400
	case strings.Contains(body, "81001"):
		kind = "invalid_request"
		status = 400
	case strings.Contains(strings.ToLower(body), "apig.1009"), strings.Contains(strings.ToLower(body), "apig.0303"):
		kind = "auth"
		status = 401
	}
	if len(body) > 1024 {
		body = body[:1024]
	}
	return &provider.Failure{Status: status, KindHint: kind, Code: code, Body: body, Message: body}
}
