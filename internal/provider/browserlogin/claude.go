package browserlogin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

// 公开客户端标识不是账号密钥；授权与刷新必须使用相同的客户端。
const (
	ClaudeClientID          = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	ClaudeTokenURL          = "https://platform.claude.com/v1/oauth/token"
	ClaudeManualRedirectURI = "https://platform.claude.com/oauth/code/callback"
	ClaudeScope             = "user:profile user:inference"
)

// 仅本机 HTTP 或 HTTPS 注册地址可用于回调，不跟随用户提交的地址发起请求。
func validClaudeRedirect(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path == "" {
		return false
	}
	loopback := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	return u.Scheme == "https" || (u.Scheme == "http" && loopback)
}

func beginClaude(s *provider.BrowserLoginSession) error {
	s.Region = "global"
	s.Options.ClientID = strings.TrimSpace(s.Options.ClientID)
	s.Options.RedirectURI = strings.TrimSpace(s.Options.RedirectURI)
	if len(s.Options.ClientID) > 1024 || len(s.Options.ClientSecret) > 4096 || len(s.Options.RedirectURI) > 4096 || strings.ContainsAny(s.Options.ClientID+s.Options.ClientSecret+s.Options.RedirectURI, "\r\n\x00") {
		return fmt.Errorf("Claude OAuth 客户端配置格式无效")
	}
	if s.Options.ClientID == "" {
		s.Options.ClientID = ClaudeClientID
	}
	if s.Options.RedirectURI == "" {
		if s.Options.ClientID != ClaudeClientID {
			return fmt.Errorf("自有 Claude OAuth 客户端须填写已登记的回调地址")
		}
		s.Options.RedirectURI = ClaudeManualRedirectURI
	}
	if !validClaudeRedirect(s.Options.RedirectURI) {
		return fmt.Errorf("Claude 回调须为无查询参数的 HTTPS 地址或本机 HTTP 地址")
	}
	if s.Options.RedirectURI == ClaudeManualRedirectURI {
		s.Mode = "code"
	}
	q := url.Values{
		"code": {"true"}, "client_id": {s.Options.ClientID}, "response_type": {"code"},
		"redirect_uri": {s.Options.RedirectURI}, "scope": {ClaudeScope},
		"state": {s.State}, "code_challenge": {challenge(s.Verifier)}, "code_challenge_method": {"S256"},
	}
	s.AuthURL = "https://claude.ai/oauth/authorize?" + q.Encode()
	return nil
}

// 授权码只在当前会话中兑换；完整地址和 code#state 均须绑定本次随机状态。
func claudeCallback(s *provider.BrowserLoginSession, callback string) (string, error) {
	text := strings.TrimSpace(callback)
	if text == "" || len(text) > 65536 || strings.ContainsAny(text, "\r\n\x00") {
		return "", fmt.Errorf("Claude 授权信息为空或格式无效")
	}
	var code, state string
	if strings.Contains(text, "://") {
		u, err := url.Parse(text)
		expected, expectedErr := url.Parse(s.Options.RedirectURI)
		if err != nil || expectedErr != nil || u.User != nil || u.Fragment != "" || u.Scheme != expected.Scheme || !strings.EqualFold(u.Host, expected.Host) || u.EscapedPath() != expected.EscapedPath() {
			return "", fmt.Errorf("回调地址与本次 Claude 登录地址不匹配")
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(q["code"]) != 1 || len(q["state"]) != 1 || q.Get("error") != "" {
			return "", fmt.Errorf("Claude 授权未完成，请复制本次完整回调地址或授权码")
		}
		code, state = q.Get("code"), q.Get("state")
	} else {
		parts := strings.Split(text, "#")
		if len(parts) != 2 {
			return "", fmt.Errorf("请粘贴 Claude 返回的完整授权码（包含 # 后的状态）或完整回调地址")
		}
		code, state = parts[0], parts[1]
	}
	if code == "" || strings.ContainsAny(code, " \t\r\n\x00") || len(code) > 8192 {
		return "", fmt.Errorf("Claude 授权码格式无效")
	}
	if len(state) != len(s.State) || subtle.ConstantTimeCompare([]byte(state), []byte(s.State)) != 1 {
		return "", fmt.Errorf("授权状态不匹配，请使用本次 Claude 登录产生的授权信息")
	}
	return code, nil
}

func hasClaudeInference(scope string) bool {
	for _, value := range strings.Fields(scope) {
		if value == "user:inference" {
			return true
		}
	}
	return false
}

func completeClaude(ctx context.Context, client *http.Client, s *provider.BrowserLoginSession, callback string) ([]byte, error) {
	code, err := claudeCallback(s, callback)
	if err != nil {
		return nil, err
	}
	if len(s.Raw) > 0 {
		return s.Raw, nil
	}
	body := map[string]string{"grant_type": "authorization_code", "code": code, "state": s.State, "code_verifier": s.Verifier, "client_id": s.Options.ClientID, "redirect_uri": s.Options.RedirectURI}
	if s.Options.ClientSecret != "" {
		body["client_secret"] = s.Options.ClientSecret
	}
	var m map[string]any
	if len(s.Exchanged) > 0 {
		if json.Unmarshal(s.Exchanged, &m) != nil || m == nil {
			return nil, fmt.Errorf("Claude 授权会话无效，请重新开始")
		}
	} else {
		var status int
		m, status, err = call(ctx, client, http.MethodPost, ClaudeTokenURL, encode(body), http.Header{})
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK || stringField(m, "access_token") == "" {
			return nil, fmt.Errorf("Claude 授权码兑换失败（HTTP %d），请重新登录", status)
		}
		delete(m, "expires_at")
		setExpiry(m)
	}
	expires, expiryErr := time.Parse(time.RFC3339, stringField(m, "expires_at"))
	if stringField(m, "refresh_token") == "" || expiryErr != nil || !expires.After(time.Now()) {
		return nil, fmt.Errorf("Claude 授权结果缺少刷新令牌或有效期限，请重新登录")
	}
	if !hasClaudeInference(stringField(m, "scope")) {
		return nil, fmt.Errorf("Claude 授权未授予模型调用权限，请重新授权")
	}
	if kind := stringField(m, "token_type"); kind != "" && !strings.EqualFold(kind, "Bearer") {
		return nil, fmt.Errorf("Claude 返回了不支持的令牌类型")
	}
	for _, key := range []string{"access_token", "refresh_token", "scope"} {
		if strings.ContainsAny(stringField(m, key), "\r\n\x00") {
			return nil, fmt.Errorf("Claude 返回了无效鉴权字段")
		}
	}
	s.Exchanged = encode(m)
	accountID := stringField(object(m, "account"), "uuid")
	orgID := stringField(object(m, "organization"), "uuid")
	if accountID == "" {
		profile, status, err := call(ctx, client, http.MethodGet, "https://api.anthropic.com/api/oauth/profile", nil, http.Header{"Authorization": {"Bearer " + stringField(m, "access_token")}})
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("Claude 无法确认登录账号（HTTP %d）", status)
		}
		accountID = stringField(object(profile, "account"), "uuid")
		orgID = stringField(object(profile, "organization"), "uuid")
	}
	if accountID == "" || strings.ContainsAny(accountID+orgID, "\r\n\x00") {
		return nil, fmt.Errorf("Claude 授权结果缺少有效账号编号")
	}
	// 上游只能返回令牌和身份，不得覆盖用户选择的模型、地址或客户端配置。
	tokens := map[string]any{"account_id": accountID, "organization_id": orgID, "client_id": s.Options.ClientID, "scope": stringField(m, "scope"), "models": s.Options.Models}
	for _, key := range []string{"access_token", "refresh_token", "expires_at"} {
		tokens[key] = stringField(m, key)
	}
	if s.Options.ClientSecret != "" {
		tokens["client_secret"] = s.Options.ClientSecret
	}
	s.Raw = encode(tokens)
	s.Exchanged = nil
	return s.Raw, nil
}
