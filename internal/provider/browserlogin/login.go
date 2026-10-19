// Package browserlogin 实现平台授权，不读取个人客户端文件，不输出上游令牌。
package browserlogin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

// 这些是公开客户端标识，不是用户密钥；Gemini 使用用户自行配置的 OAuth 客户端。
const codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
const qoderClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"

func randomText(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成授权随机数失败")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomDeviceID(uuid bool) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成授权设备编号失败")
	}
	if !uuid {
		return hex.EncodeToString(b), nil
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
func stringField(m map[string]any, names ...string) string {
	for _, n := range names {
		switch v := m[n].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	return ""
}
func object(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	if v == nil {
		return map[string]any{}
	}
	return v
}
func call(ctx context.Context, client *http.Client, method, endpoint string, body []byte, h http.Header) (map[string]any, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("授权请求地址无效")
	}
	req.Header = h.Clone()
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("无法连接授权服务，请检查网络或代理")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, resp.StatusCode, fmt.Errorf("授权响应无法读取或超过限制")
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		if resp.StatusCode == 202 || resp.StatusCode == 403 || resp.StatusCode == 404 {
			return map[string]any{}, resp.StatusCode, nil
		}
		return nil, resp.StatusCode, fmt.Errorf("授权服务返回了非 JSON 响应")
	}
	return m, resp.StatusCode, nil
}
func encode(v any) []byte { b, _ := json.Marshal(v); return b }
func qoderAPI(s *provider.BrowserLoginSession) string {
	if s.Region == "global" {
		return "https://openapi.qoder.sh"
	}
	return "https://openapi.qoder.com.cn"
}
func traeAPI(s *provider.BrowserLoginSession) string {
	if s.Region == "global" {
		return "https://api.trae.ai"
	}
	return "https://api.trae.com.cn"
}

func Begin(ctx context.Context, client *http.Client, family string, opts provider.BrowserLoginOptions) (*provider.BrowserLoginSession, error) {
	if family == "gemini" && (strings.TrimSpace(opts.ClientID) == "" || strings.TrimSpace(opts.ProjectID) == "") {
		return nil, fmt.Errorf("Gemini 浏览器授权需要自己的 OAuth 客户端 ID 和已开通 Code Assist 的项目 ID")
	}
	if len(opts.Models) > 2048 {
		return nil, fmt.Errorf("型号数量超过限制")
	}
	for _, m := range opts.Models {
		if strings.TrimSpace(m) == "" || len(m) > 256 {
			return nil, fmt.Errorf("型号不能为空或超过 256 字节")
		}
	}
	state, err := randomText(32)
	if err != nil {
		return nil, err
	}
	verifier, err := randomText(48)
	if err != nil {
		return nil, err
	}
	s := &provider.BrowserLoginSession{Family: family, Region: opts.Region, Options: opts, State: state, Verifier: verifier, Mode: "callback", ExpiresAt: time.Now().Add(10 * time.Minute), PollInterval: 3 * time.Second}
	if s.Region == "" {
		s.Region = "global"
	}
	if s.Region != "cn" && s.Region != "global" {
		return nil, fmt.Errorf("地区必须为 cn 或 global")
	}
	q := url.Values{}
	switch family {
	case "claude":
		if err := beginClaude(s); err != nil {
			return nil, err
		}
	case "codex":
		s.Region = "global"
		s.Mode = "device"
		s.Options.ClientID = codexClientID
		s.ExpiresAt = time.Now().Add(15 * time.Minute)
		m, status, err := call(ctx, client, http.MethodPost, "https://auth.openai.com/api/accounts/deviceauth/usercode", encode(map[string]string{"client_id": codexClientID}), http.Header{})
		if err != nil {
			return nil, err
		}
		if status != 200 {
			return nil, fmt.Errorf("Codex 设备授权未获允许（HTTP %d）；请检查 ChatGPT 的设备授权设置", status)
		}
		s.DeviceAuthID = stringField(m, "device_auth_id")
		s.UserCode = stringField(m, "user_code", "usercode")
		if s.DeviceAuthID == "" || s.UserCode == "" {
			return nil, fmt.Errorf("Codex 授权响应缺少设备编号或验证码")
		}
		if n, _ := strconv.Atoi(stringField(m, "interval")); n >= 3 && n <= 60 {
			s.PollInterval = time.Duration(n) * time.Second
		}
		s.AuthURL = "https://auth.openai.com/codex/device"
	case "qoder":
		s.Mode = "device"
		s.MachineID, err = randomDeviceID(true)
		if err != nil {
			return nil, err
		}
		s.State, err = randomDeviceID(true)
		if err != nil {
			return nil, err
		}
		state = s.State
		q.Set("challenge", challenge(verifier))
		q.Set("challenge_method", "S256")
		q.Set("nonce", state)
		q.Set("machine_id", s.MachineID)
		q.Set("client_id", qoderClientID)
		host := "https://qoder.com.cn"
		if s.Region == "global" {
			host = "https://qoder.com"
		}
		s.AuthURL = host + "/device/selectAccounts?" + q.Encode()
	case "gemini":
		s.Region = "global"
		q.Set("client_id", opts.ClientID)
		q.Set("redirect_uri", opts.RedirectURI)
		q.Set("response_type", "code")
		q.Set("state", state)
		q.Set("code_challenge", challenge(verifier))
		q.Set("code_challenge_method", "S256")
		q.Set("access_type", "offline")
		q.Set("prompt", "consent")
		q.Set("scope", "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email")
		s.AuthURL = "https://accounts.google.com/o/oauth2/v2/auth?" + q.Encode()
	case "trae", "trae-intl":
		s.Region = "cn"
		if family == "trae-intl" {
			s.Region = "global"
		}
		machine, err := randomDeviceID(false)
		if err != nil {
			return nil, err
		}
		device, err := randomDeviceID(false)
		if err != nil {
			return nil, err
		}
		s.MachineID = machine
		s.DeviceID = device
		clientID, host, plugin, authFrom := "ono9krqynydwx5", "https://www.trae.cn", "2.3.62834", "trae"
		if family == "trae-intl" {
			clientID, host, plugin, authFrom = "en1oxy7wnw8j9n", "https://www.trae.ai", "trae-handoff-1.0", "solo"
		}
		s.Options.ClientID = clientID
		q.Set("client_id", clientID)
		q.Set("login_version", "1")
		q.Set("auth_from", authFrom)
		q.Set("plugin_version", plugin)
		q.Set("login_channel", "native_ide")
		q.Set("auth_type", "local")
		q.Set("redirect", "0")
		q.Set("login_trace_id", state)
		q.Set("auth_callback_url", opts.RedirectURI)
		q.Set("machine_id", s.MachineID)
		q.Set("device_id", s.DeviceID)
		q.Set("x_machine_id", s.MachineID)
		q.Set("x_device_id", s.DeviceID)
		q.Set("x_app_version", "3.3.62")
		q.Set("x_app_type", "stable")
		q.Set("code_challenge", challenge(verifier))
		q.Set("code_challenge_method", "S256")
		s.AuthURL = host + "/authorization?" + q.Encode()
	default:
		return nil, provider.ErrUnsupported
	}
	return s, nil
}

func Poll(ctx context.Context, client *http.Client, s *provider.BrowserLoginSession) ([]byte, bool, error) {
	if len(s.Raw) > 0 {
		return s.Raw, true, nil
	}
	if time.Now().After(s.ExpiresAt) {
		return nil, false, fmt.Errorf("授权已过期，请重新开始")
	}
	if s.Mode != "device" || time.Now().Before(s.NextPoll) {
		return nil, false, nil
	}
	s.NextPoll = time.Now().Add(s.PollInterval)
	switch s.Family {
	case "codex":
		m, status, err := call(ctx, client, http.MethodPost, "https://auth.openai.com/api/accounts/deviceauth/token", encode(map[string]string{"device_auth_id": s.DeviceAuthID, "user_code": s.UserCode}), http.Header{})
		if err != nil {
			return nil, false, err
		}
		if status == 403 || status == 404 || status == 202 {
			return nil, false, nil
		}
		if status == 429 {
			s.NextPoll = time.Now().Add(30 * time.Second)
			return nil, false, nil
		}
		if status != 200 {
			return nil, false, fmt.Errorf("Codex 授权未完成（HTTP %d）", status)
		}
		code, verifier := stringField(m, "authorization_code"), stringField(m, "code_verifier")
		if code == "" || verifier == "" {
			return nil, false, fmt.Errorf("Codex 授权结果缺少兑换字段")
		}
		tokens, err := oauthToken(ctx, client, "https://auth.openai.com/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {s.Options.ClientID}, "code_verifier": {verifier}, "redirect_uri": {"https://auth.openai.com/deviceauth/callback"}})
		if err != nil {
			return nil, false, err
		}
		tokens["models"] = s.Options.Models
		tokens["client_id"] = s.Options.ClientID
		s.Raw = encode(tokens)
		return s.Raw, true, nil
	case "qoder":
		q := url.Values{"nonce": {s.State}, "verifier": {s.Verifier}, "challenge_method": {"S256"}}
		m, status, err := call(ctx, client, http.MethodGet, qoderAPI(s)+"/api/v1/deviceToken/poll?"+q.Encode(), nil, http.Header{})
		if err != nil {
			return nil, false, err
		}
		if status == 404 || status == 202 {
			return nil, false, nil
		}
		if status == 429 {
			s.NextPoll = time.Now().Add(30 * time.Second)
			return nil, false, nil
		}
		if status != 200 {
			return nil, false, fmt.Errorf("Qoder 授权未完成（HTTP %d）", status)
		}
		token := stringField(m, "token", "device_token", "access_token")
		if token == "" {
			return nil, false, fmt.Errorf("Qoder 授权响应缺少令牌")
		}
		uid := stringField(m, "user_id", "uid")
		if uid == "" {
			info, status, err := call(ctx, client, http.MethodGet, qoderAPI(s)+"/api/v1/userinfo", nil, http.Header{"Authorization": {"Bearer " + token}})
			if err != nil {
				return nil, false, err
			}
			if status != 200 {
				return nil, false, fmt.Errorf("Qoder 无法确认登录账号")
			}
			uid = stringField(info, "user_id", "uid", "id")
		}
		if uid == "" {
			return nil, false, fmt.Errorf("Qoder 响应缺少账号编号")
		}
		m["access_token"] = token
		m["uid"] = uid
		m["refresh_token"] = stringField(m, "refresh_token", "refreshToken")
		m["region"] = s.Region
		m["models"] = s.Options.Models
		if stringField(m, "machine_id") == "" {
			m["machine_id"] = s.MachineID
		}
		if stringField(m, "expires_at") == "" {
			setExpiry(m)
		}
		s.Raw = encode(m)
		return s.Raw, true, nil
	}
	return nil, false, provider.ErrUnsupported
}

func oauthToken(ctx context.Context, client *http.Client, endpoint string, values url.Values) (map[string]any, error) {
	m, status, err := call(ctx, client, http.MethodPost, endpoint, []byte(values.Encode()), http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
	if err != nil {
		return nil, err
	}
	if status != 200 || stringField(m, "access_token") == "" {
		return nil, fmt.Errorf("授权码兑换失败（HTTP %d），请重新登录", status)
	}
	setExpiry(m)
	return m, nil
}
func setExpiry(m map[string]any) {
	if seconds, err := strconv.ParseFloat(stringField(m, "expires_in"), 64); err == nil && seconds > 0 && seconds <= 365*86400 {
		m["expires_at"] = time.Now().Add(time.Duration(seconds * float64(time.Second))).UTC().Format(time.RFC3339)
	}
}

func Complete(ctx context.Context, client *http.Client, s *provider.BrowserLoginSession, callback string) ([]byte, error) {
	if (s.Mode != "callback" && !(s.Family == "claude" && s.Mode == "code")) || time.Now().After(s.ExpiresAt) {
		return nil, fmt.Errorf("当前授权不接受回调或已过期")
	}
	if len(callback) > 65536 {
		return nil, fmt.Errorf("回调地址超过限制")
	}
	if s.Family == "claude" {
		return completeClaude(ctx, client, s, callback)
	}
	u, err := url.Parse(strings.TrimSpace(callback))
	expected, _ := url.Parse(s.Options.RedirectURI)
	if err != nil || u.User != nil || u.Fragment != "" || u.Scheme != expected.Scheme || !strings.EqualFold(u.Host, expected.Host) || u.Path != expected.Path {
		return nil, fmt.Errorf("回调地址与本次登录地址不匹配")
	}
	q := u.Query()
	state := q.Get("state")
	if s.Family == "trae" || s.Family == "trae-intl" {
		state = q.Get("loginTraceID")
		if state == "" {
			state = q.Get("login_trace_id")
		}
	}
	if len(state) != len(s.State) || subtle.ConstantTimeCompare([]byte(state), []byte(s.State)) != 1 {
		return nil, fmt.Errorf("授权状态不匹配，请使用本次登录产生的回调地址")
	}
	if len(s.Raw) > 0 {
		return s.Raw, nil
	}
	var tokens map[string]any
	if s.Family == "gemini" {
		if q.Get("code") == "" {
			return nil, fmt.Errorf("回调缺少授权码，或用户取消了授权")
		}
		values := url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "client_id": {s.Options.ClientID}, "redirect_uri": {s.Options.RedirectURI}, "code_verifier": {s.Verifier}}
		if s.Options.ClientSecret != "" {
			values.Set("client_secret", s.Options.ClientSecret)
		}
		tokens, err = oauthToken(ctx, client, "https://oauth2.googleapis.com/token", values)
		if err != nil {
			return nil, err
		}
		tokens["mode"] = "code_assist"
		tokens["project_id"] = s.Options.ProjectID
		tokens["client_id"] = s.Options.ClientID
		tokens["client_secret"] = s.Options.ClientSecret
	} else if s.Family == "trae" || s.Family == "trae-intl" {
		code := q.Get("code")
		if code == "" {
			code = q.Get("authCode")
		}
		if code == "" {
			var info map[string]any
			_ = json.Unmarshal([]byte(q.Get("authCodeInfo")), &info)
			code = stringField(info, "AuthCode", "authCode")
		}
		if code != "" {
			body := encode(map[string]any{"ClientID": s.Options.ClientID, "AuthCode": code, "CodeVerifier": s.Verifier, "IDEVersion": "3.3.62", "DeviceInfo": map[string]string{"MachineID": s.MachineID, "DeviceID": s.DeviceID, "PlatformCode": "IDE_PC", "DeviceType": "PC", "DeviceName": "relaypool"}})
			m, status, e := call(ctx, client, http.MethodPost, traeAPI(s)+"/trae/api/v3/oauth/ExchangeToken", body, http.Header{})
			if e != nil {
				return nil, e
			}
			if status != 200 {
				return nil, fmt.Errorf("Trae 授权码兑换失败（HTTP %d）", status)
			}
			result := object(m, "Result")
			tokens = map[string]any{"access_token": stringField(result, "Token"), "refresh_token": stringField(result, "RefreshToken"), "expires_at": result["TokenExpireAt"]}
		} else {
			// 国际授权页可能返回令牌；仍须匹配一次性状态并向平台查询账号身份。
			tokens = map[string]any{"access_token": q.Get("userJwt"), "refresh_token": q.Get("refreshToken")}
		}
		if stringField(tokens, "access_token") == "" {
			return nil, fmt.Errorf("Trae 回调缺少可用的授权码或登录态")
		}
		m, status, e := call(ctx, client, http.MethodPost, traeAPI(s)+"/cloudide/api/v3/trae/GetUserInfo", encode(map[string]string{"ReqSource": "IDE", "IDEVersion": "3.3.62"}), http.Header{"X-Cloudide-Token": {stringField(tokens, "access_token")}})
		if e != nil {
			return nil, e
		}
		if status != 200 {
			return nil, fmt.Errorf("Trae 无法确认登录账号")
		}
		uid := stringField(object(m, "Result"), "UserID")
		if uid == "" {
			return nil, fmt.Errorf("Trae 登录结果缺少账号编号")
		}
		tokens["uid"] = uid
		tokens["region"] = s.Region
		tokens["machine_id"] = s.MachineID
		tokens["device_id"] = s.DeviceID
		tokens["refresh_client_id"] = s.Options.ClientID
		tokens["api_host"] = traeAPI(s)
		if s.Family == "trae" {
			tokens["variant"] = "cn"
			tokens["scene"] = "chat_v3"
			tokens["ide_version"] = "3.3.62"
		}
	} else {
		return nil, provider.ErrUnsupported
	}
	tokens["models"] = s.Options.Models
	s.Raw = encode(tokens)
	return s.Raw, nil
}
