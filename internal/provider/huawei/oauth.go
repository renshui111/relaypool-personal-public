package huawei

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

type privateJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d,omitempty"`
}

func randomText(n int) (string, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}
func newJWK() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	encode := func(n *big.Int) string { return base64.RawURLEncoding.EncodeToString(n.FillBytes(make([]byte, 32))) }
	raw, _ := json.Marshal(privateJWK{"EC", "P-256", encode(key.X), encode(key.Y), encode(key.D)})
	return string(raw), nil
}
func proof(raw, endpoint string) (string, error) {
	var jwk privateJWK
	if json.Unmarshal([]byte(raw), &jwk) != nil || jwk.Kty != "EC" || jwk.Crv != "P-256" {
		return "", fmt.Errorf("DPoP 私钥格式无效")
	}
	d, err := base64.RawURLEncoding.DecodeString(jwk.D)
	if err != nil || len(d) != 32 {
		return "", fmt.Errorf("DPoP 私钥格式无效")
	}
	scalar := new(big.Int).SetBytes(d)
	curve := elliptic.P256()
	if scalar.Sign() <= 0 || scalar.Cmp(curve.Params().N) >= 0 {
		return "", fmt.Errorf("DPoP 私钥范围无效")
	}
	x, y := curve.ScalarBaseMult(d)
	encode := func(n *big.Int) string { return base64.RawURLEncoding.EncodeToString(n.FillBytes(make([]byte, 32))) }
	if jwk.X != encode(x) || jwk.Y != encode(y) {
		return "", fmt.Errorf("DPoP 公私钥不匹配")
	}
	public := jwk
	public.D = ""
	header, _ := json.Marshal(map[string]any{"alg": "ES256", "typ": "dpop+jwt", "jwk": public})
	jti, err := randomText(32)
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]any{"htm": "POST", "htu": endpoint, "iat": time.Now().Unix(), "jti": jti})
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: scalar}, digest[:])
	if err != nil {
		return "", err
	}
	half := new(big.Int).Rsh(new(big.Int).Set(curve.Params().N), 1)
	if s.Cmp(half) > 0 {
		s.Sub(curve.Params().N, s)
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return input + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (c *Client) clientID() string {
	if c.family == OfficeAce {
		return "pdp5_for_agentarts"
	}
	return "codearts-agent"
}
func (c *Client) BeginBrowserLogin(ctx context.Context, o provider.BrowserLoginOptions) (*provider.BrowserLoginSession, error) {
	if o.Region != "" && o.Region != "cn" {
		return nil, fmt.Errorf("当前华为接入仅支持中国站")
	}
	verifier, err := randomText(32)
	if err != nil {
		return nil, err
	}
	state, err := randomText(32)
	if err != nil {
		return nil, err
	}
	jwk, err := newJWK()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	session := &provider.BrowserLoginSession{Family: c.family, Region: "cn", State: state, Verifier: verifier, MachineID: jwk, Options: o, ExpiresAt: time.Now().Add(10 * time.Minute), PollInterval: 3 * time.Second, Mode: "callback"}
	if c.family == OfficeAce {
		// OfficeAce's cloud issues and owns the login state; no local app is needed.
		r, _ := http.NewRequestWithContext(ctx, "POST", c.control+"/v1/claw/auth/state", strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(r)
		if err != nil {
			return nil, fmt.Errorf("果办登录状态申请失败")
		}
		defer resp.Body.Close()
		var data struct {
			State string `json:"state"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data)
		if resp.StatusCode != 200 || err != nil || data.State == "" || len(data.State) > 4096 {
			return nil, fmt.Errorf("果办未返回有效登录状态")
		}
		session.State = data.State
		session.Mode = "device"
		session.Options.RedirectURI = c.control + "/v1/claw/auth/callback"
		params := url.Values{"client_id": {c.clientID()}, "response_type": {"code"}, "redirect_uri": {session.Options.RedirectURI}, "scope": {"openid"}, "state": {session.State}, "code_challenge": {challenge}, "code_challenge_method": {"SHA-256"}}
		inner := c.auth + "/authui/v1/oauth2/authorize?" + params.Encode()
		session.AuthURL = c.auth + "/authui/login.html?" + url.Values{"service": {inner}}.Encode()
	} else {
		// Portal requires a callback port >=10000. In Docker the user pastes the
		// failed local callback URL; no listener or extra host port is opened.
		session.Options.RedirectURI = "http://127.0.0.1:12800/oauth/callback"
		params := url.Values{"theme": {"2"}, "locale": {"zh-cn"}, "uri_scheme": {c.clientID()}, "client_id": {c.clientID()}, "port": {"12800"}, "code_challenge": {challenge}, "code_challenge_method": {"SHA-256"}, "ticket_id": {state}, "plugin-name": {"snap_AIIDE"}, "plugin-version": {"26.9.902"}}
		session.AuthURL = c.portal + "/authorize?" + params.Encode()
	}
	return session, nil
}

func (c *Client) exchange(ctx context.Context, form url.Values, jwk string) ([]byte, error) {
	signed, err := proof(jwk, c.token)
	if err != nil {
		return nil, err
	}
	r, _ := http.NewRequestWithContext(ctx, "POST", c.token, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("DPoP", signed)
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, fmt.Errorf("华为令牌服务连接失败")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("华为令牌响应读取失败")
	}
	if resp.StatusCode != 200 {
		return nil, &provider.Failure{Status: 401, KindHint: "auth", Message: "华为令牌交换失败，请重新授权；授权码与刷新令牌不可重复使用"}
	}
	return raw, nil
}

func (c *Client) tokenCredential(raw []byte, jwk, verifier string) (map[string]any, error) {
	var data struct {
		Credentials struct {
			Access  string `json:"access_key_id"`
			Secret  string `json:"secret_access_key"`
			Token   string `json:"security_token"`
			Expiry  string `json:"expiration"`
			Project string `json:"project_id"`
		} `json:"credentials"`
		Refresh string          `json:"refresh_token"`
		Project string          `json:"project_id"`
		Result  json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return nil, fmt.Errorf("华为令牌结构无效")
	}
	if data.Credentials.Access == "" && len(data.Result) > 0 {
		return c.tokenCredential(data.Result, jwk, verifier)
	}
	if data.Credentials.Access == "" || data.Credentials.Secret == "" || data.Credentials.Token == "" || data.Credentials.Expiry == "" {
		return nil, fmt.Errorf("华为令牌响应缺少临时凭证或到期时间")
	}
	var private json.RawMessage = json.RawMessage(jwk)
	if data.Project == "" {
		data.Project = data.Credentials.Project
	}
	return map[string]any{"access_key": data.Credentials.Access, "secret_key": data.Credentials.Secret, "security_token": data.Credentials.Token, "expires_at": data.Credentials.Expiry, "refresh_token": data.Refresh, "project_id": data.Project, "dpop_private_jwk": private, "code_verifier": verifier}, nil
}

func (c *Client) finishLogin(ctx context.Context, s *provider.BrowserLoginSession) ([]byte, error) {
	data, err := c.tokenCredential(s.Exchanged, s.MachineID, s.Verifier)
	if err != nil {
		return nil, err
	}
	if c.family == OfficeAce {
		fields := map[string]string{}
		for _, k := range []string{"access_key", "secret_key", "security_token", "project_id"} {
			fields[k], _ = data[k].(string)
		}
		raw, err := c.jsonRequest(ctx, provider.Credential{Fields: fields}, "GET", c.control+"/v1/claw/client-permission-validate", nil, true)
		if err != nil {
			return nil, fmt.Errorf("果办开通校验失败，请重试；若未开通，请先在官方客户端开通")
		}
		var root struct {
			Info         json.RawMessage `json:"model_info"`
			Subscription struct {
				Info json.RawMessage `json:"model_info"`
			} `json:"subscription"`
		}
		_ = json.Unmarshal(raw, &root)
		info := root.Info
		if len(info) == 0 || string(info) == "null" {
			info = root.Subscription.Info
		}
		var model struct {
			Base string `json:"model_api_url_base"`
			Auth struct {
				Key    string `json:"model_app_key"`
				Secret string `json:"model_app_secret"`
			} `json:"model_auth_info"`
		}
		_ = json.Unmarshal(info, &model)
		if model.Auth.Key == "" || model.Auth.Secret == "" || model.Base == "" {
			return nil, fmt.Errorf("此账号尚未获得果办模型服务，请先在官方客户端开通")
		}
		if !strings.Contains(model.Base, "://") {
			model.Base = "https://" + model.Base
		}
		data["model_base_url"] = model.Base
		data["model_app_key"] = model.Auth.Key
		data["model_app_secret"] = model.Auth.Secret
	}
	if len(s.Options.Models) > 0 {
		data["models"] = s.Options.Models
	}
	encoded, _ := json.Marshal(data)
	if _, err := c.DecodeCredential(encoded); err != nil {
		return nil, err
	}
	s.Raw = encoded
	return encoded, nil
}

func (c *Client) PollBrowserLogin(ctx context.Context, s *provider.BrowserLoginSession) ([]byte, bool, error) {
	if time.Now().After(s.ExpiresAt) {
		return nil, false, fmt.Errorf("华为授权已过期")
	}
	if len(s.Raw) > 0 {
		return s.Raw, true, nil
	}
	if len(s.Exchanged) > 0 {
		raw, err := c.finishLogin(ctx, s)
		return raw, err == nil, err
	}
	if c.family == CodeArts {
		return nil, false, nil
	}
	if time.Now().Before(s.NextPoll) {
		return nil, false, nil
	}
	s.NextPoll = time.Now().Add(3 * time.Second)
	r, _ := http.NewRequestWithContext(ctx, "GET", c.control+"/v1/claw/auth/code?"+url.Values{"state": {s.State}}.Encode(), nil)
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, false, fmt.Errorf("果办授权状态读取失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 202 {
		return nil, false, nil
	}
	var data struct {
		Code              string `json:"code"`
		AuthorizationCode string `json:"authorization_code"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data)
	if resp.StatusCode != 200 || err != nil {
		return nil, false, fmt.Errorf("果办暂未返回授权码，请稍后重试")
	}
	if data.Code == "" {
		data.Code = data.AuthorizationCode
	}
	if data.Code == "" {
		return nil, false, nil
	}
	s.Exchanged, err = c.exchange(ctx, url.Values{"client_id": {c.clientID()}, "grant_type": {"authorization_code"}, "code": {data.Code}, "code_verifier": {s.Verifier}, "redirect_uri": {s.Options.RedirectURI}}, s.MachineID)
	if err != nil {
		return nil, false, err
	}
	raw, err := c.finishLogin(ctx, s)
	return raw, err == nil, err
}

func (c *Client) CompleteBrowserLogin(ctx context.Context, s *provider.BrowserLoginSession, callback string) ([]byte, error) {
	if time.Now().After(s.ExpiresAt) {
		return nil, fmt.Errorf("华为授权已过期")
	}
	if len(s.Raw) > 0 {
		return s.Raw, nil
	}
	if c.family != CodeArts {
		return nil, fmt.Errorf("果办使用浏览器授权后的自动轮询，无需粘贴回调")
	}
	if len(s.Exchanged) > 0 {
		return c.finishLogin(ctx, s)
	}
	u, err := url.Parse(strings.TrimSpace(callback))
	if err != nil || u.Scheme != "http" || u.Host != "127.0.0.1:12800" || u.User != nil || u.Path != "/oauth/callback" || u.Fragment != "" {
		return nil, fmt.Errorf("请粘贴本次码道登录的完整本机回调地址")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("码道回调参数格式无效")
	}
	// CodeArts receives our nonce as ticket_id, not OAuth state. The portal's
	// state is owned by Huawei and must not be compared with that ticket.
	// Like the official authentication extension, bind the authorization code
	// through this authenticated login session's PKCE verifier at token exchange.
	for _, key := range []string{"code", "secret", "redirect", "ticket_id"} {
		if len(q[key]) > 1 {
			return nil, fmt.Errorf("码道回调包含重复参数，请重新复制完整地址")
		}
	}
	if ticket := q.Get("ticket_id"); ticket != "" && ticket != s.State {
		return nil, fmt.Errorf("码道回调不属于本次登录，请使用当前授权链接重新登录")
	}
	if q.Get("code") == "" && q.Get("secret") != "" && q.Get("redirect") != "" {
		target, err := url.Parse(q.Get("redirect"))
		if err != nil || target.Scheme != "https" || target.User != nil || !(target.Hostname() == "auth.huaweicloud.com" || target.Hostname() == "codearts.huaweicloud.com") {
			return nil, fmt.Errorf("码道后续登录地址无效")
		}
		// Never request a user-provided redirect on the server; return an allowlisted
		// browser navigation. The following callback still belongs to this login.
		s.AuthURL = target.String()
		return nil, nil
	}
	if len(q["code"]) != 1 || q.Get("code") == "" || u.Port() != "12800" {
		return nil, fmt.Errorf("码道回调缺少唯一授权码或端口不匹配")
	}
	s.Exchanged, err = c.exchange(ctx, url.Values{"client_id": {c.clientID()}, "grant_type": {"authorization_code"}, "code": {q.Get("code")}, "code_verifier": {s.Verifier}, "redirect_uri": {s.Options.RedirectURI}}, s.MachineID)
	if err != nil {
		return nil, err
	}
	return c.finishLogin(ctx, s)
}

func (c *Client) NeedsRefresh(cred provider.Credential) bool {
	return c.family == CodeArts && !cred.ExpiresAt.IsZero() && time.Until(cred.ExpiresAt) < time.Minute && cred.Field("refresh_token") != "" && cred.Field("dpop_private_jwk") != ""
}

func (c *Client) NeedsCheckinRefresh(cred provider.Credential) bool {
	if c.family == CodeArts {
		return c.NeedsRefresh(cred)
	}
	expires, err := time.Parse(time.RFC3339, cred.Field("sts_expires_at"))
	return err == nil && !expires.IsZero() && time.Until(expires) < time.Minute && cred.Field("refresh_token") != "" && cred.Field("dpop_private_jwk") != ""
}

func (c *Client) Refresh(ctx context.Context, cred provider.Credential) (provider.Credential, error) {
	needsRefresh := c.NeedsRefresh(cred)
	if c.family == OfficeAce {
		expires, err := time.Parse(time.RFC3339, cred.Field("sts_expires_at"))
		needsRefresh = err == nil && !expires.IsZero() && time.Until(expires) < 6*time.Hour && cred.Field("refresh_token") != "" && cred.Field("dpop_private_jwk") != ""
	}
	if !needsRefresh {
		return cred, nil
	}
	// Serialize rotated one-use refresh tokens and reuse the result for concurrent
	// callers. Store CAS still prevents an old refresh overwriting an explicit edit.
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	key := hash(cred.Raw)
	if cached, ok := c.refreshed[key]; ok && time.Now().Before(cached.until) {
		out := cached.cred
		out.AccountID = cred.AccountID
		return out, nil
	}
	form := url.Values{"client_id": {c.clientID()}, "grant_type": {"refresh_token"}, "refresh_token": {cred.Field("refresh_token")}, "code_verifier": {cred.Field("code_verifier")}}
	raw, err := c.exchange(ctx, form, cred.Field("dpop_private_jwk"))
	if err != nil {
		return provider.Credential{}, err
	}
	data, err := c.tokenCredential(raw, cred.Field("dpop_private_jwk"), cred.Field("code_verifier"))
	if err != nil {
		return provider.Credential{}, err
	}
	var previous map[string]any
	_ = json.Unmarshal(cred.Raw, &previous)
	if previous == nil {
		previous = map[string]any{}
	}
	for k, v := range data {
		if k == "refresh_token" && v == "" {
			continue
		}
		previous[k] = v
	}
	encoded, _ := json.Marshal(previous)
	updated, err := c.DecodeCredential(encoded)
	if err != nil {
		return provider.Credential{}, err
	}
	updated.AccountID = cred.AccountID
	updated.PreviousRaw = cred.Raw
	for k, v := range c.refreshed {
		if time.Now().After(v.until) {
			delete(c.refreshed, k)
		}
	}
	if len(c.refreshed) >= 256 {
		oldestKey := ""
		oldest := time.Now().Add(24 * time.Hour)
		for k, v := range c.refreshed {
			if v.until.Before(oldest) {
				oldest = v.until
				oldestKey = k
			}
		}
		delete(c.refreshed, oldestKey)
	}
	c.refreshed[key] = refreshResult{updated, time.Now().Add(2 * time.Minute)}
	return updated, nil
}
