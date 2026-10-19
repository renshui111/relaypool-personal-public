package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

// Config configures the adapter's HTTP behaviour.
type Config struct {
	// Timeout bounds a whole non-streaming request.
	Timeout time.Duration
	// HeaderTimeout bounds waiting for response headers on a stream.
	HeaderTimeout time.Duration
	// IdleTimeout bounds the gap between stream chunks. Without it a stalled
	// upstream holds a connection and a goroutine forever.
	IdleTimeout time.Duration
	// ProxyURL routes upstream traffic through a proxy. Loopback hosts always
	// bypass it, which matters because a Clash-style TUN proxy can otherwise
	// intercept the gateway's own health checks.
	ProxyURL string
	// UserAgent overrides the default client identity.
	UserAgent string
}

// Client is the WorkBuddy adapter.
//
// One Client serves every WorkBuddy account: the adapter is stateless, and all
// per-account state travels in the Credential argument. That is what makes the
// in-process Kind cheap.
type Client struct {
	cfg      Config
	http     *http.Client
	streamer *http.Client

	// catalog caches the per-credential model list, keyed by the credential's
	// UID so two accounts on one client do not share entries. The catalogue
	// changes rarely, so a short TTL is safe and removes a fetch from the hot
	// path.
	mu      sync.RWMutex
	catalog map[string]catalogEntry
}

type catalogEntry struct {
	models  []provider.ModelInfo
	fetched time.Time
}

// catalogTTL is how long a cached catalogue is served before a refetch.
const catalogTTL = 10 * time.Minute

// New creates an adapter.
func New(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = 120 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 300 * time.Second
	}
	transport := newTransport(cfg)
	streamer := newTransport(cfg)
	// The streaming transport must not carry a whole-request timeout: a long
	// completion would be cut off mid-answer. Its bound is the response header
	// timeout plus the per-read idle timeout instead.
	streamer.ResponseHeaderTimeout = cfg.HeaderTimeout

	return &Client{
		cfg:      cfg,
		http:     &http.Client{Transport: transport, Timeout: cfg.Timeout, CheckRedirect: rejectRedirect},
		streamer: &http.Client{Transport: streamer, CheckRedirect: rejectRedirect},
		catalog:  map[string]catalogEntry{},
	}
}

// 上游鉴权头不得被重定向到其他地址，流式和普通请求使用同一规则。
func rejectRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// newTransport builds an HTTP transport honoring the proxy setting.
func newTransport(cfg Config) *http.Transport {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		ForceAttemptHTTP2:   true,
		Proxy:               proxyFunc(cfg.ProxyURL),
	}
	return transport
}

// proxyFunc returns a proxy selector that never proxies loopback.
//
// Bypassing loopback is not an optimization: with a TUN-mode proxy the
// gateway's own 127.0.0.1 traffic can be captured, which shows up as requests
// that hang indefinitely with no error.
func proxyFunc(proxyURL string) func(*http.Request) (*url.URL, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil
	}
	return func(req *http.Request) (*url.URL, error) {
		host := req.URL.Hostname()
		if host == "localhost" {
			return nil, nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil, nil
		}
		return parsed, nil
	}
}

// Family reports the provider family.
func (c *Client) Family() string { return Family }

// Kind reports the runtime shape. WorkBuddy is stateless HTTP, so it runs
// in-process.
func (c *Client) Kind() provider.Kind { return provider.KindInProcess }

// DecodeCredential parses a stored blob into the neutral credential form.
func (c *Client) DecodeCredential(raw []byte) (provider.Credential, error) {
	cred, err := DecodeCredential(raw)
	if err != nil {
		return provider.Credential{}, err
	}
	return cred.toProvider(""), nil
}

// decodeCredential parses a blob while preserving the account ID.
func (c *Client) decodeCredential(req provider.Credential) (Credential, error) {
	// Prefer the raw blob, falling back to the decoded fields so a credential
	// built in memory (tests, import flows) works without a round trip.
	if len(req.Raw) > 0 {
		return DecodeCredential(req.Raw)
	}
	cred := Credential{
		AccessToken:  req.Field("access_token"),
		RefreshToken: req.Field("refresh_token"),
		UID:          req.Field("uid"),
		EnterpriseID: req.Field("enterprise_id"),
		Domain:       req.Field("domain"),
		Nickname:     req.Field("nickname"),
	}
	if !cred.Ready() && req.Field("access_token") == "" {
		return Credential{}, fmt.Errorf("%w: missing access token", provider.ErrNotReady)
	}
	return cred, nil
}

// FetchCatalog lists the models this credential can serve.
//
// A failure is deliberately NOT fatal to the account: an unknown catalogue
// fails open in the scheduler, so a transient catalogue outage must not take a
// working account out of rotation.
func (c *Client) FetchCatalog(ctx context.Context, req provider.Credential) ([]provider.ModelInfo, error) {
	cred, err := c.decodeCredential(req)
	if err != nil {
		return nil, err
	}
	if !cred.Ready() {
		return nil, fmt.Errorf("%w: workbuddy credential needs an access token and uid", provider.ErrNotReady)
	}

	key := catalogKey(cred)
	if cached, ok := c.cachedCatalog(key); ok {
		return cached, nil
	}

	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()

	endpoint := strings.TrimSuffix(cred.ChatBase(), "/") + cred.catalogPath()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	SetCatalogHeaders(httpReq.Header, cred)
	if c.cfg.UserAgent != "" {
		httpReq.Header.Set("User-Agent", c.cfg.UserAgent)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, newFailure(0, providerFamily, "", "catalogue request failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body := readErrorBody(resp.Body)
		return nil, classifyHTTP(resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}

	models, err := parseCatalog(resp.Body)
	if err != nil {
		return nil, newFailure(resp.StatusCode, providerFamily, "", "catalogue response was not usable", err)
	}
	c.storeCatalog(key, models)
	return models, nil
}

// providerFamily is the kind hint used when a failure is not specific enough
// for the classifier to name a different one.
const providerFamily = ""

// catalogKey scopes a catalogue cache entry to one account.
func catalogKey(cred Credential) string {
	if cred.UID != "" {
		return Family + "|" + string(cred.Region()) + "|" + cred.UID
	}
	// Without a UID, fall back to the token's tail so two accounts do not
	// collide. Using the whole token would put a secret in a map key where it
	// could reach a log line.
	token := cred.AccessToken
	if len(token) > 12 {
		token = token[len(token)-12:]
	}
	return Family + "|" + string(cred.Region()) + "|token:" + token
}

func (c *Client) cachedCatalog(key string) ([]provider.ModelInfo, bool) {
	c.mu.RLock()
	entry, ok := c.catalog[key]
	c.mu.RUnlock()
	if !ok || time.Since(entry.fetched) > catalogTTL {
		return nil, false
	}
	return entry.models, true
}

func (c *Client) storeCatalog(key string, models []provider.ModelInfo) {
	c.mu.Lock()
	if c.catalog == nil {
		c.catalog = map[string]catalogEntry{}
	}
	c.catalog[key] = catalogEntry{models: models, fetched: time.Now()}
	c.mu.Unlock()
}

// InvalidateCatalog drops a cached catalogue so the next call refetches. Called
// after a model-not-available error, since that is evidence the cache is stale.
func (c *Client) InvalidateCatalog(req provider.Credential) {
	cred, err := c.decodeCredential(req)
	if err != nil {
		return
	}
	key := catalogKey(cred)
	c.mu.Lock()
	delete(c.catalog, key)
	c.mu.Unlock()
}

// ChatNonStream performs a completion and aggregates it locally.
//
// The upstream is always asked to stream (see PrepareBody), so a "non-streaming"
// call is really a streamed call that this function folds into one object.
//
// REASONING-RETRY: a reasoning model given a small max_tokens can spend the
// entire budget thinking and return no answer at all. Rather than hand back an
// empty 200, this retries ONCE with a larger budget, because the caller's
// request was reasonable and the failure is an artifact of how thinking models
// account for tokens. If the retry also produces no answer, a typed error is
// returned so the caller can report something truthful.
func (c *Client) ChatNonStream(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.ChatResult, error) {
	result, reasoningOnly, err := c.chatNonStreamOnce(ctx, cred, req)
	if err == nil && !reasoningOnly {
		return result, nil
	}
	if err != nil {
		return nil, err
	}

	if req.StrictTokenLimit {
		return nil, &ReasoningOnlyError{}
	}

	// The thinking-only case: retry with a budget large enough to answer after
	// thinking. The multiplier is generous because thinking length is not
	// predictable and a second retry would waste more than it saves.
	retryReq := cloneChatRequest(req)
	retryBudget := reasoningRetryBudget(req.MaxTokens)
	retryReq.MaxTokens = &retryBudget
	retryReq.ReasoningEffort = lowerReasoningEffort(req.ReasoningEffort)

	retryResult, retryReasoningOnly, retryErr := c.chatNonStreamOnce(ctx, cred, retryReq)
	if retryErr == nil && !retryReasoningOnly {
		return retryResult, nil
	}
	if retryErr != nil {
		// The retry hit a different problem; report that, since it is the more
		// recent and usually more informative failure.
		return nil, retryErr
	}
	return nil, &ReasoningOnlyError{}
}

// reasoningRetryBudget computes a larger token budget for the retry.
//
// A floor is applied because the common cause is a very small caller-supplied
// budget (a smoke test asking for 16 tokens). Scaling 16 by a multiplier still
// yields too little to both think and answer.
func reasoningRetryBudget(requested *int) int {
	const floor = 2048
	if requested == nil {
		return floor
	}
	scaled := *requested * 8
	if scaled < floor {
		return floor
	}
	// Cap the raise so a caller who asked for a huge budget does not get an
	// even huger one.
	if scaled > 32768 {
		return 32768
	}
	return scaled
}

// lowerReasoningEffort reduces the thinking effort for the retry.
//
// Less thinking leaves more of the budget for the answer, which is the point of
// the retry. An empty or already-low effort is left alone.
func lowerReasoningEffort(current string) string {
	switch NormalizeReasoningLevel(current) {
	case "xhigh", "high":
		return "low"
	case "medium":
		return "low"
	default:
		return current
	}
}

// chatNonStreamOnce performs one streamed call and folds it into a result.
//
// It reports reasoningOnly separately from err so the caller can distinguish
// "the upstream failed" from "the upstream succeeded but produced no answer".
func (c *Client) chatNonStreamOnce(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.ChatResult, bool, error) {
	stream, err := c.ChatStream(ctx, cred, req)
	if err != nil {
		return nil, false, err
	}
	defer stream.Close()

	aggregated, err := Aggregate(stream.Body)
	if err != nil {
		return nil, false, newFailure(0, providerFamily, "", "failed to read the completion stream", err)
	}
	if reasoningOnly, preview, finishReason := IsReasoningOnly(aggregated); reasoningOnly {
		_ = preview
		_ = finishReason
		return nil, true, nil
	}
	result, err := chatResultFromAggregate(aggregated)
	if err != nil {
		return nil, false, err
	}
	return result, false, nil
}

// cloneChatRequest copies a request so a retry cannot mutate the caller's copy.
//
// The Raw body is deliberately rebuilt from the neutral fields rather than
// reused: the retry changes max_tokens, and leaving the original Raw in place
// would let the adapter re-apply the old budget over the new one.
func cloneChatRequest(req *provider.ChatRequest) *provider.ChatRequest {
	if req == nil {
		return &provider.ChatRequest{}
	}
	out := *req
	out.Raw = nil
	out.Stop = append([]string(nil), req.Stop...)
	out.Messages = append([]provider.Message(nil), req.Messages...)
	return &out
}

// ChatStream opens a streaming completion.
func (c *Client) ChatStream(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.Stream, error) {
	decoded, err := c.decodeCredential(cred)
	if err != nil {
		return nil, err
	}
	if !decoded.Ready() {
		return nil, fmt.Errorf("%w: workbuddy credential needs an access token and uid", provider.ErrNotReady)
	}

	body, err := c.buildChatBody(decoded, req)
	if err != nil {
		return nil, err
	}

	endpoint := strings.TrimSuffix(decoded.ChatBase(), "/") + pathChat
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	SetChatHeaders(httpReq.Header, decoded)
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.cfg.UserAgent != "" {
		httpReq.Header.Set("User-Agent", c.cfg.UserAgent)
	}

	resp, err := c.streamer.Do(httpReq)
	if err != nil {
		return nil, newFailure(0, providerFamily, "", "chat request failed", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody := readErrorBody(resp.Body)
		return nil, classifyHTTP(resp.StatusCode, resp.Header.Get("Retry-After"), errBody)
	}

	// Wrap the body in the idle-timeout reader so a stalled upstream is cut
	// off rather than holding the connection open indefinitely.
	//
	// The raw upstream body is passed through rather than wrapped in
	// io.NopCloser: closing the returned stream has to close the CONNECTION, and
	// NopCloser would silently discard that, leaking a connection and the
	// sanitizer goroutine for every stream the client abandons.
	body2 := newIdleTimeoutReader(resp.Body, c.cfg.IdleTimeout)
	sanitized := rewriteChatStream(&http.Response{
		Body:   body2,
		Header: resp.Header,
	})

	model := resp.Header.Get("X-Model")
	if model == "" {
		model = req.Model
	}
	return &provider.Stream{
		Body:        sanitized.Body,
		ContentType: "text/event-stream; charset=utf-8",
		Model:       model,
	}, nil
}

// buildChatBody renders the upstream request body.
func (c *Client) buildChatBody(cred Credential, req *provider.ChatRequest) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("workbuddy: chat request is required")
	}
	// Start from the caller's original body when available so pass-through
	// fields survive, then overlay the neutral fields this adapter models.
	base := map[string]any{}
	if len(req.Raw) > 0 {
		if err := json.Unmarshal(req.Raw, &base); err != nil {
			base = map[string]any{}
		}
	}

	if req.Model != "" {
		base["model"] = req.Model
	}
	if len(req.Messages) > 0 {
		base["messages"] = renderMessages(req.Messages)
	}
	if req.Temperature != nil {
		base["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		base["top_p"] = *req.TopP
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		base["max_tokens"] = *req.MaxTokens
	}
	if len(req.Stop) > 0 {
		base["stop"] = req.Stop
	}
	if len(req.Tools) > 0 {
		base["tools"] = json.RawMessage(req.Tools)
	}
	if len(req.ToolChoice) > 0 {
		base["tool_choice"] = json.RawMessage(req.ToolChoice)
	}

	// Apply the reasoning object from the model's own capabilities when the
	// catalogue has been fetched; otherwise use the conservative default.
	caps := c.capsFor(cred, req.Model)
	requested := req.ReasoningEffort
	if req.DisableThinking && requested == "" {
		requested = "none"
	}
	applyChatReasoning(base, requested, caps.ReasoningOptions, caps.ReasoningDefault, caps.CanDisableThinking)

	raw, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	// PrepareBody forces streaming and applies the upstream-specific fixes.
	return PrepareBody(raw), nil
}

// renderMessages converts neutral messages back to JSON.
func renderMessages(messages []provider.Message) []any {
	out := make([]any, 0, len(messages))
	for _, msg := range messages {
		entry := map[string]any{"role": msg.Role}
		if len(msg.Content) > 0 {
			entry["content"] = json.RawMessage(msg.Content)
		} else {
			entry["content"] = ""
		}
		if msg.Name != "" {
			entry["name"] = msg.Name
		}
		if len(msg.ToolCalls) > 0 {
			entry["tool_calls"] = json.RawMessage(msg.ToolCalls)
		}
		if msg.ToolCallID != "" {
			entry["tool_call_id"] = msg.ToolCallID
		}
		out = append(out, entry)
	}
	return out
}

// capsFor looks up a cached model's capabilities.
func (c *Client) capsFor(cred Credential, model string) provider.ModelInfo {
	models, ok := c.cachedCatalog(catalogKey(cred))
	if !ok {
		return provider.ModelInfo{}
	}
	want := strings.ToLower(strings.TrimSpace(model))
	for _, m := range models {
		if strings.ToLower(m.NativeModel) == want || strings.ToLower(m.PublicModel) == want {
			return m
		}
		// Public IDs may arrive underscored or mixed-case; the canonical form
		// is the stable join key.
		if canonicalModelID(m.NativeModel) == canonicalModelID(model) {
			return m
		}
	}
	return provider.ModelInfo{}
}

// Checkin attempts the daily check-in.
func (c *Client) Checkin(ctx context.Context, req provider.Credential) (provider.CheckinResult, error) {
	cred, err := c.decodeCredential(req)
	if err != nil {
		return provider.CheckinResult{}, err
	}
	if !cred.Ready() {
		return provider.CheckinResult{}, fmt.Errorf("%w: workbuddy credential needs an access token and uid", provider.ErrNotReady)
	}

	ctx, cancel := context.WithTimeout(ctx, checkinTimeout)
	defer cancel()

	endpoint := strings.TrimSuffix(cred.BillingBase(), "/") + pathDailyCheckin
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		return provider.CheckinResult{}, err
	}
	SetBillingHeaders(httpReq.Header, cred)
	if c.cfg.UserAgent != "" {
		httpReq.Header.Set("User-Agent", c.cfg.UserAgent)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return provider.CheckinResult{Status: provider.CheckinError, Message: err.Error()},
			newFailure(0, providerFamily, "", "check-in request failed", err)
	}
	defer resp.Body.Close()

	// 领奖响应读取失败或被截断时不能确认成功，避免把代理错误页记成已领取。
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody+1))
	if readErr != nil || len(raw) > maxErrorBody {
		return provider.CheckinResult{Status: provider.CheckinError}, fmt.Errorf("WorkBuddy 签到响应无法读取或超过限制")
	}
	body := string(raw)

	// "Already checked in today" arrives as a 400 with a specific message. It
	// is a SUCCESS for the scheduler's purposes: the day's reward is claimed,
	// so treating it as an error would keep retrying all day.
	if resp.StatusCode == http.StatusBadRequest && alreadyCheckedIn(body) {
		return provider.CheckinResult{Status: provider.CheckinAlready, Message: alreadyCheckedInMessage}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return provider.CheckinResult{Status: provider.CheckinError, Message: truncate(body, 200)},
			classifyHTTP(resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	// 部分计费服务会用 HTTP 200 包装业务失败，不能因此记录领取成功。
	var result map[string]any
	if json.Unmarshal([]byte(body), &result) != nil || result == nil {
		return provider.CheckinResult{Status: provider.CheckinError}, fmt.Errorf("WorkBuddy 签到服务返回了无效响应，未确认领取成功")
	}
	if alreadyCheckedIn(body) {
		return provider.CheckinResult{Status: provider.CheckinAlready, Message: alreadyCheckedInMessage}, nil
	}
	failed := result["error"] != nil
	if code, ok := result["code"]; ok && fmt.Sprint(code) != "0" {
		failed = true
	}
	if success, ok := result["success"].(bool); ok && !success {
		failed = true
	}
	if failed {
		return provider.CheckinResult{Status: provider.CheckinError}, fmt.Errorf("WorkBuddy 签到服务未确认领取成功")
	}

	return provider.CheckinResult{Status: provider.CheckinSuccess, Message: "OK"}, nil
}

// Refresh renews the access token.
func (c *Client) Refresh(ctx context.Context, req provider.Credential) (provider.Credential, error) {
	cred, err := c.decodeCredential(req)
	if err != nil {
		return provider.Credential{}, err
	}
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return provider.Credential{}, fmt.Errorf("%w: no refresh token stored", provider.ErrUnsupported)
	}

	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	endpoint := strings.TrimSuffix(cred.ChatBase(), "/") + pathTokenRefresh
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		return provider.Credential{}, err
	}
	SetRefreshHeaders(httpReq.Header, cred)
	if c.cfg.UserAgent != "" {
		httpReq.Header.Set("User-Agent", c.cfg.UserAgent)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return provider.Credential{}, newFailure(0, providerFamily, "", "token refresh failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body := readErrorBody(resp.Body)
		return provider.Credential{}, classifyHTTP(resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}

	var payload struct {
		AccessToken  string `json:"accessToken"`
		AccessToken2 string `json:"access_token"`
		ExpiresAt    int64  `json:"expiresAt"`
		ExpiresAt2   int64  `json:"expires_at"`
	}
	limited := io.LimitReader(resp.Body, 1<<20)
	if err := json.NewDecoder(limited).Decode(&payload); err != nil {
		return provider.Credential{}, newFailure(resp.StatusCode, providerFamily, "", "refresh response was not usable", err)
	}
	cred.AccessToken = firstNonEmpty(payload.AccessToken, payload.AccessToken2)
	if cred.AccessToken == "" {
		return provider.Credential{}, newFailure(resp.StatusCode, providerFamily, "", "refresh response carried no access token", nil)
	}
	cred.ExpiresAt = firstNonZero(payload.ExpiresAt, payload.ExpiresAt2)
	return cred.toProvider(req.AccountID), nil
}

// chatResultFromAggregate converts the aggregated map into a ChatResult.
func chatResultFromAggregate(aggregated map[string]any) (*provider.ChatResult, error) {
	result := &provider.ChatResult{}

	if id, ok := aggregated["id"].(string); ok {
		result.ID = id
	}
	if model, ok := aggregated["model"].(string); ok {
		result.Model = model
	}
	switch created := aggregated["created"].(type) {
	case float64:
		result.Created = int64(created)
	case json.Number:
		if v, err := created.Int64(); err == nil {
			result.Created = v
		}
	}
	if result.Created == 0 {
		result.Created = time.Now().Unix()
	}

	choices, _ := aggregated["choices"].([]map[string]any)
	if len(choices) == 0 {
		if generic, ok := aggregated["choices"].([]any); ok && len(generic) > 0 {
			if first, ok := generic[0].(map[string]any); ok {
				choices = []map[string]any{first}
			}
		}
	}
	if len(choices) == 0 {
		return nil, fmt.Errorf("workbuddy: aggregated stream had no choices")
	}
	choice := choices[0]
	if reason, ok := choice["finish_reason"].(string); ok {
		result.FinishReason = reason
	}
	if result.FinishReason == "" {
		result.FinishReason = "stop"
	}

	message, _ := choice["message"].(map[string]any)
	if message != nil {
		role, _ := message["role"].(string)
		if role == "" {
			role = "assistant"
		}
		result.Message.Role = role
		if content, ok := message["content"].(string); ok {
			encoded, _ := json.Marshal(content)
			result.Message.Content = encoded
		}
		if calls, ok := message["tool_calls"]; ok && calls != nil {
			encoded, err := json.Marshal(calls)
			if err == nil {
				result.Message.ToolCalls = encoded
			}
		}
	}

	if usage, ok := aggregated["usage"].(map[string]any); ok {
		result.Usage = usageFrom(usage)
	}
	return result, nil
}

// usageFrom reads the upstream usage object, accepting both naming styles.
func usageFrom(usage map[string]any) provider.Usage {
	out := provider.Usage{}
	out.PromptTokens = intField(usage, "prompt_tokens", "promptTokens", "input_tokens")
	out.CompletionTokens = intField(usage, "completion_tokens", "completionTokens", "output_tokens")
	out.TotalTokens = intField(usage, "total_tokens", "totalTokens")
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	if credits, ok := usage["credits"].(float64); ok {
		out.Credits = credits
	}
	return out
}

func intField(m map[string]any, keys ...string) int {
	for _, key := range keys {
		switch v := m[key].(type) {
		case float64:
			return int(v)
		case json.Number:
			if parsed, err := v.Int64(); err == nil {
				return int(parsed)
			}
		case int:
			return v
		case int64:
			return int(v)
		}
	}
	return 0
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
