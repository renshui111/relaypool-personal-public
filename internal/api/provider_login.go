package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/browserlogin"
	"github.com/relaypool/relaypool/internal/store"
)

// 同一授权的轮询、回调与取消串行，避免重复兑换授权码或取消刚保存的账号。
type browserAttempt struct {
	mu      sync.Mutex
	session *provider.BrowserLoginSession
}

var errProviderLoginCredential = errors.New("授权结果缺少此渠道所需字段，请重新授权或导入完整登录态")

func (s *Server) startProviderLogin(w http.ResponseWriter, r *http.Request, req startLoginRequest, family string) {
	if !provider.BrowserLoginSupported(family) {
		writeError(w, 400, "unsupported_provider", "此平台没有已接入的浏览器授权，请使用官方 API 密钥或本人可用登录态")
		return
	}
	adapter, err := s.registry.Lookup(family)
	if err != nil {
		writeError(w, 400, "unsupported_provider", "平台未配置")
		return
	}
	login, ok := adapter.(provider.BrowserLoginAdapter)
	if !ok {
		writeError(w, 400, "unsupported_provider", "平台未实现浏览器授权")
		return
	}
	s.loginMu.Lock()
	full := len(s.logins) >= 128
	s.loginMu.Unlock()
	if full {
		writeError(w, 429, "too_many_logins", "正在进行的授权过多，请先取消或等待过期")
		return
	}
	o := req.BrowserLoginOptions
	o.Region = strings.ToLower(strings.TrimSpace(req.Region))
	if o.Region == "" {
		o.Region = "global"
		if family == "qoder" || family == "trae" {
			o.Region = "cn"
		}
	}
	if (family == "trae" && o.Region != "cn") || (family == "trae-intl" && o.Region != "global") || ((family == "gemini" || family == "codex" || family == "claude") && o.Region != "global") {
		writeError(w, 400, "invalid_region", "所选地区与平台不匹配")
		return
	}
	_, port, _ := net.SplitHostPort(s.cfg.Listen)
	path := "/auth/callback"
	if family == "trae" || family == "trae-intl" {
		path = "/authorize"
	}
	if family == "claude" {
		// 本机沿用官方注册的 localhost /callback；远程使用平台授权码页。
		if o.RedirectURI == "" && (o.ClientID == "" || o.ClientID == browserlogin.ClaudeClientID) {
			o.RedirectURI = browserlogin.ClaudeManualRedirectURI
			host := &url.URL{Host: r.Host}
			if host.Hostname() == "localhost" || host.Hostname() == "127.0.0.1" || host.Hostname() == "::1" {
				if host.Port() != "" {
					port = host.Port()
				}
				o.RedirectURI = "http://localhost:" + port + "/callback"
			}
		}
	} else {
		o.RedirectURI = "http://127.0.0.1:" + port + path
	}
	if len(o.RedirectURI) > 4096 || strings.ContainsAny(o.RedirectURI, "\r\n\x00") {
		writeError(w, 400, "invalid_request", "OAuth 回调地址格式无效")
		return
	}
	if len(o.ClientID) > 1024 || len(o.ClientSecret) > 4096 || len(o.ProjectID) > 256 || strings.ContainsAny(o.ClientID+o.ClientSecret+o.ProjectID, "\r\n\x00") {
		writeError(w, 400, "invalid_request", "OAuth 客户端配置格式无效")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	flow, err := login.BeginBrowserLogin(ctx, o)
	if err != nil {
		writeError(w, 400, "login_unavailable", err.Error())
		return
	}
	account, err := s.store.CreateAccount(ctx, store.Account{Name: defaultLoginName(req.Name, flow.Region), Provider: family, Region: flow.Region, AuthType: "oauth", Enabled: false})
	if err != nil {
		writeError(w, 500, "internal_error", "无法创建待授权账号")
		return
	}
	s.loginMu.Lock()
	if s.logins == nil {
		s.logins = map[string]pendingLogin{}
	}
	s.logins[account.ID] = pendingLogin{id: account.ID, account: account, provider: family, global: flow.Region == "global", started: time.Now(), flow: &browserAttempt{session: flow}}
	s.loginMu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 201, map[string]any{"login_id": account.ID, "auth_url": flow.AuthURL, "region": flow.Region, "mode": flow.Mode, "user_code": flow.UserCode, "callback_url": flow.Options.RedirectURI, "expires_in_seconds": int(time.Until(flow.ExpiresAt).Seconds()), "poll_interval_seconds": int(flow.PollInterval.Seconds())})
}

func (s *Server) pollProviderLogin(w http.ResponseWriter, r *http.Request, p pendingLogin) {
	p.flow.mu.Lock()
	defer p.flow.mu.Unlock()
	if current, ok := s.getLogin(p.id); !ok || current.flow != p.flow {
		if s.replyCompletedClaudeLogin(w, r.Context(), p.id) {
			return
		}
		writeJSON(w, 200, map[string]any{"status": loginExpired})
		return
	}
	if time.Now().After(p.flow.session.ExpiresAt) {
		// 已持有授权锁，先释放再复用取消清理逻辑。
		s.finishLogin(p.id, false)
		s.discardIncompleteAccount(r.Context(), p.id)
		writeJSON(w, 200, map[string]any{"status": loginExpired})
		return
	}
	adapter, _ := s.registry.Lookup(p.provider)
	login := adapter.(provider.BrowserLoginAdapter)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	raw, done, err := login.PollBrowserLogin(ctx, p.flow.session)
	if err != nil {
		writeJSON(w, 200, map[string]any{"status": loginPending, "last_error": err.Error()})
		return
	}
	if !done {
		writeJSON(w, 200, map[string]any{"status": loginPending})
		return
	}
	account, err := s.saveProviderLogin(ctx, p, adapter, raw)
	if err != nil {
		if errors.Is(err, errProviderLoginCredential) {
			writeJSON(w, 200, map[string]any{"status": loginFailed, "message": err.Error()})
		} else {
			writeError(w, 500, "internal_error", err.Error())
		}
		return
	}
	writeJSON(w, 200, map[string]any{"status": loginDone, "account": accountView(account)})
}

// 调用者持有本次授权锁，取消和过期清理不能删除刚保存的凭证。
func (s *Server) saveProviderLogin(ctx context.Context, p pendingLogin, adapter provider.Provider, raw []byte) (store.Account, error) {
	decoded, err := adapter.DecodeCredential(raw)
	if err != nil || (decoded.Region != "" && string(decoded.Region) != p.account.Region) {
		return store.Account{}, errProviderLoginCredential
	}
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	if err := s.store.UpsertCredential(ctx, store.Credential{AccountID: p.id, Format: p.provider + "-oauth-v1", Payload: raw, AuthType: "oauth", ExpiresAt: decoded.ExpiresAt}); err != nil {
		return store.Account{}, fmt.Errorf("授权成功，但保存登录态失败，请重试")
	}
	if err := s.store.SetAccountEnabled(ctx, p.id, true); err != nil {
		return store.Account{}, fmt.Errorf("登录态已保存，但启用账号失败，请重试")
	}
	account, err := s.store.GetAccount(ctx, p.id)
	if err != nil {
		return store.Account{}, fmt.Errorf("无法读取已保存账号")
	}
	s.pool.Upsert(s.accountPoolItem(account))
	if len(p.flow.session.Options.Models) > 0 {
		s.pool.MergeModels(p.id, p.flow.session.Options.Models)
	}
	s.finishLogin(p.id, true)
	return account, nil
}

func (s *Server) completeProviderLogin(ctx context.Context, p pendingLogin, callback string) error {
	p.flow.mu.Lock()
	defer p.flow.mu.Unlock()
	current, ok := s.getLogin(p.id)
	if !ok || current.flow != p.flow {
		return provider.ErrNotReady
	}
	adapter, err := s.registry.Lookup(p.provider)
	if err != nil {
		return provider.ErrUnsupported
	}
	raw, err := adapter.(provider.BrowserLoginAdapter).CompleteBrowserLogin(ctx, p.flow.session, callback)
	if err == nil && p.provider == "claude" {
		// Claude 回调收到后立即入池，保存不依赖用户继续打开轮询页面。
		_, err = s.saveProviderLogin(ctx, p, adapter, raw)
	}
	return err
}

func (s *Server) handleCompleteAccountLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CallbackURL string `json:"callback_url"`
	}
	if decodeJSONBody(w, r, &req, 1<<17) != nil {
		writeError(w, 400, "invalid_request", "回调地址格式无效")
		return
	}
	p, ok := s.getLogin(r.PathValue("id"))
	if !ok || p.flow == nil {
		if s.replyCompletedClaudeLogin(w, r.Context(), r.PathValue("id")) {
			return
		}
		writeError(w, 404, "not_found", "没有可接收回调的登录")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.completeProviderLogin(ctx, p, req.CallbackURL); err != nil {
		writeError(w, 400, "callback_failed", err.Error())
		return
	}
	if p.provider == "claude" && s.replyCompletedClaudeLogin(w, ctx, p.id) {
		return
	}
	writeJSON(w, 200, map[string]string{"status": loginPending, "message": "已接收授权，正在保存登录态"})
}

// 回调与状态轮询可能同时到达，已保存账号不能被晚到轮询误判为过期。
func (s *Server) replyCompletedClaudeLogin(w http.ResponseWriter, ctx context.Context, id string) bool {
	account, err := s.store.GetAccount(ctx, id)
	if err != nil || account.Provider != "claude" || !account.Enabled || !account.HasCredential || account.AuthType != "oauth" {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"status": loginDone, "account": accountView(account)})
	return true
}

// 回调不携带管理密钥；只允许一次性状态匹配的授权，不提供其他管理操作。
func (s *Server) handleBrowserCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	state := r.URL.Query().Get("state")
	if state == "" {
		state = r.URL.Query().Get("loginTraceID")
	}
	if state == "" {
		state = r.URL.Query().Get("login_trace_id")
	}
	var p pendingLogin
	found := false
	s.loginMu.Lock()
	for _, v := range s.logins {
		if v.flow != nil && v.flow.session.State == state && state != "" {
			p = v
			found = true
			break
		}
	}
	s.loginMu.Unlock()
	if !found {
		http.Error(w, "授权已过期或状态不匹配", 400)
		return
	}
	origin := r.Header.Get("Origin")
	allowed := ""
	if p.provider == "trae" {
		allowed = "https://www.trae.cn"
	}
	if p.provider == "trae-intl" {
		allowed = "https://www.trae.ai"
	}
	if origin != "" && origin != allowed {
		http.Error(w, "授权来源不匹配", 403)
		return
	}
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
	u, _ := url.Parse(p.flow.session.Options.RedirectURI)
	if r.URL.Path != u.Path {
		http.Error(w, "回调路径不匹配", 400)
		return
	}
	u.RawQuery = r.URL.RawQuery
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.completeProviderLogin(ctx, p, u.String()); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if p.provider == "trae" || p.provider == "trae-intl" {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, 200, map[string]any{"success": true, "message": "授权已接收，请返回号池"})
		return
	}
	if p.provider == "claude" {
		_, _ = w.Write([]byte("Claude 授权成功，登录态已加密保存，账号已启用。请返回号池页面。"))
		return
	}
	_, _ = w.Write([]byte("授权已接收，请返回号池页面等待保存完成。"))
}

func (s *Server) handleBrowserCallbackPreflight(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if (origin != "https://www.trae.cn" && origin != "https://www.trae.ai") || r.Header.Get("Access-Control-Request-Method") != "GET" {
		http.Error(w, "授权来源或回调方法不匹配", 403)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "GET")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Private-Network", "true")
	w.WriteHeader(http.StatusNoContent)
}
