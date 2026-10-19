package native

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/browserlogin"
)

// 用户主动导入的客户端 JSON 才进入解析，不扫描电脑上的登录文件。
func (c *Client) DecodeCredential(raw []byte) (provider.Credential, error) {
	var input map[string]any
	if json.Unmarshal(raw, &input) != nil || input == nil {
		return provider.Credential{}, fmt.Errorf("平台凭证必须是 JSON 对象")
	}
	for _, key := range []string{"claudeAiOauth", "tokens"} {
		if nested, ok := input[key].(map[string]any); ok {
			for k, v := range nested {
				if _, exists := input[k]; !exists {
					input[k] = v
				}
			}
		}
	}
	for _, pair := range [][2]string{{"accessToken", "access_token"}, {"refreshToken", "refresh_token"}, {"expiresAt", "expires_at"}, {"OPENAI_API_KEY", "api_key"}, {"expiry_date", "expires_at"}, {"expired", "expires_at"}} {
		if value, ok := input[pair[0]]; ok && input[pair[1]] == nil {
			input[pair[1]] = value
		}
	}
	defaults := map[string]string{"claude": "https://api.anthropic.com/v1", "gemini": "https://generativelanguage.googleapis.com/v1beta", "openai": "https://api.openai.com/v1", "codex": "https://chatgpt.com/backend-api/codex"}
	mode := str(input, "mode")
	if c.family == "gemini" && mode == "code_assist" {
		defaults[c.family] = "https://cloudcode-pa.googleapis.com/v1internal"
		if str(input, "project_id") == "" {
			return provider.Credential{}, fmt.Errorf("Gemini Code Assist 登录态必须填写 project_id")
		}
	}
	endpoint, err := baseURL(str(input, "base_url"), defaults[c.family])
	if err != nil {
		return provider.Credential{}, err
	}
	fields := map[string]string{"base_url": endpoint, "mode": mode}
	for _, key := range []string{"api_key", "access_token", "refresh_token", "id_token", "client_id", "client_secret", "project_id", "account_id", "organization_id", "scope"} {
		value := str(input, key)
		if strings.ContainsAny(value, "\r\n\x00") {
			return provider.Credential{}, fmt.Errorf("平台鉴权字段不能包含换行")
		}
		fields[key] = value
	}
	// Claude Code 导出的 scopes 数组与浏览器授权的 scope 字符串统一保存。
	if c.family == "claude" && fields["scope"] == "" && input["scopes"] != nil {
		var scopes []string
		if json.Unmarshal(marshal(input["scopes"]), &scopes) != nil || strings.ContainsAny(strings.Join(scopes, " "), "\r\n\x00") {
			return provider.Credential{}, fmt.Errorf("Claude scopes 必须是有效的权限名称数组")
		}
		fields["scope"] = strings.Join(scopes, " ")
	}
	if fields["api_key"] == "" && fields["access_token"] == "" {
		return provider.Credential{}, fmt.Errorf("需要 API 密钥或客户端登录态 access_token")
	}
	if c.family == "openai" && fields["api_key"] == "" {
		return provider.Credential{}, fmt.Errorf("OpenAI API 渠道需要 API 密钥；ChatGPT 登录态请选择 Codex")
	}
	if c.family == "codex" {
		if fields["access_token"] == "" {
			return provider.Credential{}, fmt.Errorf("Codex 渠道需要客户端登录态，不能使用 API 密钥代替")
		}
		if fields["account_id"] == "" {
			for _, token := range []string{fields["access_token"], fields["id_token"]} {
				claims := jwtClaims(token)
				if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
					fields["account_id"] = str(auth, "chatgpt_account_id")
					if fields["account_id"] != "" {
						break
					}
				}
			}
		}
		if fields["account_id"] == "" || strings.ContainsAny(fields["account_id"], "\r\n") {
			return provider.Credential{}, fmt.Errorf("Codex 登录态缺少 account_id")
		}
	}
	if c.family == "gemini" && mode != "" && mode != "api" && mode != "code_assist" {
		return provider.Credential{}, fmt.Errorf("Gemini mode 只支持 api 或 code_assist")
	}
	if mode == "code_assist" && fields["api_key"] != "" {
		return provider.Credential{}, fmt.Errorf("Code Assist 需要 OAuth 登录态，不能使用 API 密钥")
	}
	if fields["api_key"] != "" && fields["access_token"] != "" {
		return provider.Credential{}, fmt.Errorf("API 密钥和订阅登录态请分别建立账号")
	}
	var models []string
	if input["models"] != nil {
		encoded, _ := json.Marshal(input["models"])
		if json.Unmarshal(encoded, &models) != nil {
			return provider.Credential{}, fmt.Errorf("models 必须是模型名称数组")
		}
		if len(models) > 2048 {
			return provider.Credential{}, fmt.Errorf("模型名称数量超出上限")
		}
		for _, m := range models {
			if strings.TrimSpace(m) == "" {
				return provider.Credential{}, fmt.Errorf("模型名称不能为空")
			}
		}
	}
	fields["models"] = string(marshal(models))
	expires := expiry(input["expires_at"])
	if expires.IsZero() {
		expires = expiry(jwtClaims(fields["access_token"])["exp"])
	}
	return provider.Credential{Provider: c.family, Raw: append([]byte{}, raw...), Fields: fields, ExpiresAt: expires}, nil
}
func expiry(value any) time.Time {
	if text, ok := value.(string); ok {
		date, _ := time.Parse(time.RFC3339, text)
		return date
	}
	if number, ok := value.(float64); ok && number > 0 {
		if number > 1e12 {
			return time.UnixMilli(int64(number))
		}
		return time.Unix(int64(number), 0)
	}
	return time.Time{}
}
func jwtClaims(token string) map[string]any {
	// 这里只读取过期和账号提示；令牌真实性由供应商鉴权，不能据此授予本地权限。
	out := map[string]any{}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return out
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err == nil {
		_ = json.Unmarshal(body, &out)
	}
	return out
}
func (c *Client) Refresh(ctx context.Context, cred provider.Credential) (provider.Credential, error) {
	if cred.Field("refresh_token") == "" || cred.Field("api_key") != "" {
		return provider.Credential{}, provider.ErrUnsupported
	}
	sum := sha256.Sum256([]byte(cred.AccountID + "\x00" + cred.Field("refresh_token")))
	key := fmt.Sprintf("%x", sum)
	update, err := c.refreshOnce(ctx, key, cred)
	if err != nil {
		return provider.Credential{}, err
	}
	// 使用当前账号配置重建凭证，缓存只包含令牌更新，不覆盖后来的模型和地址编辑。
	var input map[string]any
	_ = json.Unmarshal(cred.Raw, &input)
	delete(input, "claudeAiOauth")
	delete(input, "tokens")
	for _, alias := range []string{"accessToken", "refreshToken", "expiresAt", "expiry_date", "expired", "OPENAI_API_KEY"} {
		delete(input, alias)
	}
	for k, v := range cred.Fields {
		if k != "models" {
			input[k] = v
		}
	}
	var models []string
	_ = json.Unmarshal([]byte(cred.Field("models")), &models)
	input["models"] = models
	for k, v := range update.fields {
		input[k] = v
	}
	updated, err := c.DecodeCredential(marshal(input))
	if err != nil {
		return provider.Credential{}, err
	}
	updated.AccountID = cred.AccountID
	updated.PreviousRaw = append([]byte{}, cred.Raw...)
	return updated, nil
}

// 只合并同账号同令牌的刷新；不同账号独立请求，等待者可随时取消。
func (c *Client) refreshOnce(ctx context.Context, key string, cred provider.Credential) (tokenUpdate, error) {
	c.refreshMu.Lock()
	if cached, ok := c.refreshed[key]; ok && time.Since(cached.at) < 5*time.Minute {
		c.refreshMu.Unlock()
		return cached, nil
	}
	if flight, ok := c.flights[key]; ok {
		c.refreshMu.Unlock()
		select {
		case <-ctx.Done():
			return tokenUpdate{}, ctx.Err()
		case <-flight.done:
			return flight.update, flight.err
		}
	}
	if len(c.flights) >= 128 {
		c.refreshMu.Unlock()
		return tokenUpdate{}, &provider.Failure{Status: 503, Message: "平台同时刷新数量达到上限"}
	}
	flight := &tokenFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.refreshMu.Unlock()
	fields, err := c.refreshHTTP(ctx, cred)
	c.refreshMu.Lock()
	flight.update = tokenUpdate{fields: fields, at: time.Now()}
	flight.err = err
	if err == nil {
		if len(c.refreshed) >= 128 {
			for k := range c.refreshed {
				delete(c.refreshed, k)
			}
		}
		c.refreshed[key] = flight.update
	}
	delete(c.flights, key)
	close(flight.done)
	c.refreshMu.Unlock()
	return flight.update, flight.err
}
func (c *Client) refreshHTTP(ctx context.Context, cred provider.Credential) (map[string]any, error) {
	endpoints := map[string]string{"claude": browserlogin.ClaudeTokenURL, "gemini": "https://oauth2.googleapis.com/token", "codex": "https://auth.openai.com/oauth/token"}
	endpoint := endpoints[c.family]
	if c.tokenURL != "" {
		endpoint = c.tokenURL
	}
	if endpoint == "" {
		return nil, provider.ErrUnsupported
	}
	payload := map[string]any{"grant_type": "refresh_token", "refresh_token": cred.Field("refresh_token")}
	clientID := cred.Field("client_id")
	// OAuth 公共客户端标识不是账号、密码或密钥；Google 的客户端密钥必须由用户配置。
	if clientID == "" {
		if c.family == "claude" {
			clientID = browserlogin.ClaudeClientID
		}
		if c.family == "codex" {
			clientID = "app_EMoamEEZ73f0CkXaXp7hrann"
		}
	}
	payload["client_id"] = clientID
	if c.family == "claude" {
		scope := cred.Field("scope")
		if scope == "" {
			scope = browserlogin.ClaudeScope
		}
		payload["scope"] = scope
		if cred.Field("client_secret") != "" {
			payload["client_secret"] = cred.Field("client_secret")
		}
	}
	content := string(marshal(payload))
	contentType := "application/json"
	if c.family != "claude" {
		if c.family == "gemini" && (clientID == "" || cred.Field("client_secret") == "") {
			return nil, fmt.Errorf("Gemini 自动刷新需要对应 OAuth client_id 与 client_secret；请在凭证中配置")
		}
		form := url.Values{}
		for k, v := range payload {
			form.Set(k, v.(string))
		}
		if c.family == "gemini" {
			form.Set("client_secret", cred.Field("client_secret"))
		}
		content = form.Encode()
		contentType = "application/x-www-form-urlencoded"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("构造令牌刷新请求失败")
	}
	req.Header.Set("Content-Type", contentType)
	client := *c.http
	client.Timeout = 30 * time.Second
	res, err := client.Do(req)
	if err != nil {
		return nil, &provider.Failure{Status: 502, Message: "令牌刷新连接失败", Err: ctx.Err()}
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, fmt.Errorf("令牌刷新响应无效")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, &provider.Failure{Status: res.StatusCode, Message: "令牌刷新被拒绝"}
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil || str(out, "access_token") == "" {
		return nil, fmt.Errorf("令牌刷新未返回 access_token")
	}
	expires, ok := out["expires_in"].(float64)
	if !ok || expires <= 0 || expires > 31536000 {
		return nil, fmt.Errorf("令牌刷新未返回有效期限")
	}
	out["expires_at"] = time.Now().Add(time.Duration(expires) * time.Second).UTC().Format(time.RFC3339)
	if str(out, "refresh_token") == "" {
		out["refresh_token"] = cred.Field("refresh_token")
	}
	// 刷新服务器只允许更新令牌及期限，不能改写账号地址、模型或客户端配置。
	filtered := map[string]any{}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "expires_at"} {
		if value, ok := out[key]; ok {
			filtered[key] = value
		}
	}
	return filtered, nil
}
