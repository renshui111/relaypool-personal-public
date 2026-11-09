package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// login states are held in memory, keyed by account id.
//
// They are deliberately NOT persisted: a state is only meaningful while the user
// has a browser tab open on the authorization page, so one that outlives a
// restart is dead weight, and a restart mid-login should simply start over.
var (
	loginMu     sync.Mutex
	loginStates = map[string]*loginProgress{}
)

type loginProgress struct {
	mu         sync.Mutex
	state      string
	credential Credential
	ready      bool
}

// StartLogin asks the upstream for a fresh authorization state and returns the
// URL the user must open in a browser.
//
// WHY THIS EXISTS: the alternative is asking an operator to extract a token by
// hand from developer tools, which is error-prone and leaks the token into shell
// history and chat logs. Driving the documented plugin login flow keeps the
// token inside this process.
//
// This is the first step of the browser login flow:
//
//  1. POST /v2/plugin/auth/state  -> {state, authUrl}   (here)
//  2. the user opens authUrl and signs in
//  3. GET  /v2/plugin/auth/token?state=...  -> tokens   (PollLogin)
//  4. GET  /v2/plugin/login/account?state=... -> account identity (Bearer)
func (c *Client) StartLogin(ctx context.Context, accountID string, global bool) (authURL, state string, err error) {
	base := ChatBaseCN
	if global {
		base = ChatBaseGlobal
	}

	body, status, err := c.loginRequest(ctx, http.MethodPost,
		base+pathAuthState+"?platform=CLI", nil, base)
	if err != nil {
		return "", "", err
	}
	if status >= 300 {
		return "", "", fmt.Errorf("WorkBuddy 授权状态服务返回 HTTP %d", status)
	}

	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", "", fmt.Errorf("workbuddy: auth state response is not JSON: %w", err)
	}
	// A non-zero code here is an upstream refusal, not a transport problem, so it
	// is reported with the upstream's own message.
	if envelope.Code != 0 {
		return "", "", fmt.Errorf("WorkBuddy 授权状态服务拒绝请求（代码 %d）", envelope.Code)
	}

	var data struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		return "", "", fmt.Errorf("workbuddy: auth state payload: %w", err)
	}
	if data.State == "" || data.AuthURL == "" {
		return "", "", fmt.Errorf("workbuddy: auth state response is missing state or authUrl")
	}

	loginMu.Lock()
	loginStates[accountID] = &loginProgress{state: data.State}
	loginMu.Unlock()

	return data.AuthURL, data.State, nil
}

// PollLogin exchanges the stored state for tokens.
//
// It reports three outcomes rather than an error for the pending case, because
// "the user has not finished signing in" is the normal state of a poll loop and
// must not look like a failure:
//
//	done=false, err=nil  -> still waiting, poll again
//	done=true,  err=nil  -> credential obtained and returned
//	err != nil           -> the attempt genuinely failed
//
// The credential is RETURNED rather than stored here: this package knows the
// upstream protocol but nothing about the account store, and keeping that
// boundary is what lets the login flow be tested without a database.
func (c *Client) PollLogin(ctx context.Context, accountID string, global bool) (cred Credential, done bool, err error) {
	loginMu.Lock()
	progress := loginStates[accountID]
	loginMu.Unlock()
	if progress == nil {
		return Credential{}, false, fmt.Errorf("workbuddy: no login in progress for account %s", accountID)
	}
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.ready {
		return progress.credential, true, nil
	}
	state := progress.state

	base := ChatBaseCN
	if global {
		base = ChatBaseGlobal
	}

	if progress.credential.AccessToken == "" {
		body, status, err := c.loginRequest(ctx, http.MethodGet, base+pathAuthToken+"?state="+url.QueryEscape(state), nil, base)
		if err != nil {
			return Credential{}, false, err
		}
		data, pending, err := loginPayload(body, status, "令牌", 11217)
		if err != nil || pending {
			return Credential{}, false, err
		}
		token, err := decodeTokenData(data, time.Now())
		if err != nil {
			return Credential{}, false, fmt.Errorf("WorkBuddy 令牌响应缺少有效登录态，请重新授权")
		}
		token.Domain = orDefault(token.Domain, domainFor(global))
		progress.credential = token
	}
	cred = progress.credential
	// The token endpoint normally does not contain uid. Fetch the separate
	// account stage with the SAME state and token, rather than polling forever.
	if cred.UID == "" {
		body, status, err := c.loginRequest(ctx, http.MethodGet, base+pathAuthAccount+"?state="+url.QueryEscape(state), nil, base, cred.AccessToken)
		if err != nil {
			return Credential{}, false, err
		}
		data, pending, err := loginPayload(body, status, "账号信息", 12151)
		if err != nil || pending {
			return Credential{}, false, err
		}
		var info struct {
			UID          string `json:"uid"`
			AccountUID   string `json:"account_uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			Account      struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
		}
		if json.Unmarshal(data, &info) != nil {
			return Credential{}, false, fmt.Errorf("WorkBuddy 账号信息格式无效")
		}
		cred.UID = firstNonEmpty(info.Account.UID, info.UID, info.AccountUID)
		cred.EnterpriseID = firstNonEmpty(info.Account.EnterpriseID, info.EnterpriseID, cred.EnterpriseID)
		cred.Nickname = firstNonEmpty(info.Account.Nickname, info.Nickname, cred.Nickname)
		if !cred.Ready() {
			return Credential{}, false, fmt.Errorf("WorkBuddy 已授权，但账号信息缺少账号 ID，请重试")
		}
	}
	if cred.IsGlobal() != global {
		return Credential{}, false, fmt.Errorf("WorkBuddy 授权地区与所选地区不一致")
	}
	// Keep the acquired token in memory until the API confirms persistence.
	// A database failure must not force a second one-time token exchange.
	progress.credential, progress.ready = cred, true
	return cred, true, nil
}

func loginPayload(body []byte, status int, stage string, pendingCode int) (json.RawMessage, bool, error) {
	if status != http.StatusOK {
		return nil, false, fmt.Errorf("WorkBuddy %s服务返回 HTTP %d，请重试或重新授权", stage, status)
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Code == nil {
		return nil, false, fmt.Errorf("WorkBuddy %s服务返回了无效响应", stage)
	}
	if *envelope.Code == pendingCode {
		return nil, true, nil
	}
	if *envelope.Code != 0 {
		return nil, false, fmt.Errorf("WorkBuddy %s服务拒绝请求（代码 %d），请重新授权", stage, *envelope.Code)
	}
	return envelope.Data, false, nil
}

// CancelLogin forgets an abandoned or durably saved login, including cached
// tokens. The API calls this only after persistence or explicit cancellation.
func (c *Client) CancelLogin(accountID string) {
	loginMu.Lock()
	delete(loginStates, accountID)
	loginMu.Unlock()
}

// loginRequest performs one login call with the shared headers.
func (c *Client) loginRequest(ctx context.Context, method, endpoint string, payload []byte, base string, bearer ...string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	var bodyReader *strings.Reader
	if payload == nil {
		bodyReader = strings.NewReader("")
	} else {
		bodyReader = strings.NewReader(string(payload))
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("workbuddy: build login request: %w", err)
	}
	setLoginHeaders(req.Header, base == ChatBaseGlobal)
	if len(bearer) > 0 && bearer[0] != "" {
		req.Header.Set("Authorization", "Bearer "+bearer[0])
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("WorkBuddy 授权服务连接失败，请检查网络")
	}
	defer resp.Body.Close()

	// Bounded like every other response read in this package: a hostile or
	// broken upstream must not be able to make us allocate without limit.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("workbuddy: read login response: %w", err)
	}
	return raw, resp.StatusCode, nil
}

// setLoginHeaders sets the browser-like headers the plugin login endpoints
// expect. They are the same shape as the CLI's, minus the chat-only identity
// headers that cannot exist before a token does.
func setLoginHeaders(header http.Header, global bool) {
	origin := "https://www.codebuddy.cn"
	if global {
		origin = "https://www.workbuddy.ai"
	}
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/json, text/plain, */*")
	header.Set("X-Requested-With", "XMLHttpRequest")
	header.Set("Origin", origin)
	header.Set("Referer", origin+"/")
	header.Set("User-Agent", UserAgent)
}

// domainFor maps a region to the credential domain the adapter expects.
func domainFor(global bool) string {
	if global {
		return DomainGlobal
	}
	return DomainCN
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// truncateForError keeps an upstream body out of the logs at full size.
func truncateForError(raw []byte) string {
	const limit = 300
	text := strings.TrimSpace(string(raw))
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}

// LoginPollInterval is how often the console should re-poll while waiting.
const LoginPollInterval = 2 * time.Second

// LoginTimeout bounds how long a started login stays usable.
const LoginTimeout = 10 * time.Minute
