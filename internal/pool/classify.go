package pool

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Failure kinds. Every upstream error is folded into exactly one of these,
// because the kind is what decides whether failing over is useful and how long
// the account should cool down.
const (
	// 额度耗尽按配置的共享、账号或模型范围处理，默认维持共享额度策略。
	KindQuota = "quota"
	// KindRateLimit means too many requests. Failing over helps.
	KindRateLimit = "rate_limit"
	// KindAuth means the credential is rejected. Failing over helps only if
	// another account has valid credentials.
	KindAuth = "auth"
	// KindNotReady means the upstream is up but the account is not usable
	// yet (still provisioning, for example). Short cooldown.
	KindNotReady = "not_ready"
	// KindUnavailable is the catch-all upstream failure.
	KindUnavailable = "unavailable"
	// KindInvalidRequest means the REQUEST is malformed or exceeds a limit.
	// Failing over is pointless (every account rejects it identically) and the
	// account itself is healthy, so the cooldown is zero.
	KindInvalidRequest = "invalid_request"
	// KindModelNotAvailable means the account does not carry the requested
	// model. Failing over helps, but the account must never be cooled down.
	KindModelNotAvailable = "model_not_available"
)

// Backoff ladder bounds. A first failure uses the kind's own duration; the
// ladder only extends it on repeats of the same kind.
const (
	backoffFloor    = 30 * time.Second
	backoffCeiling  = 6 * time.Hour
	backoffMaxLevel = 8
)

// DefaultMaxRetryAfter is the clamp applied to any upstream Retry-After hint.
//
// It is exported so provider adapters can apply the same bound when they parse
// a hint before the pool classifier sees it. Without a shared constant the two
// layers could disagree, and a provider that clamped at a different value would
// silently override the configured policy.
const DefaultMaxRetryAfter = 10 * time.Minute

// Classified is the outcome of classifying one upstream error.
type Classified struct {
	Kind     string
	Status   int
	Failover bool
	Cooldown time.Duration
	Code     string
	Type     string
	Message  string
	// RetryAfter mirrors Cooldown and is what a caller should surface.
	RetryAfter time.Duration
	// Model scopes the cooldown to one model. When set, only that model is
	// cooled and the account keeps serving everything else.
	Model string
}

// NextLocalMidnightCooldown returns the duration until the next local midnight.
//
// A quota that resets daily must cool until the reset rather than for a fixed
// interval: retrying a few seconds later just burns more of an allowance that
// is already gone.
func NextLocalMidnightCooldown() time.Duration {
	return nextLocalMidnightCooldown(time.Now())
}

func nextLocalMidnightCooldown(now time.Time) time.Duration {
	zone := now.Location()
	if zone == nil {
		zone = time.Local
	}
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, zone)
	d := next.Sub(now)
	if d <= 0 {
		// Defensive: a clock jump could otherwise produce a zero cooldown and
		// an immediate retry loop.
		return 24 * time.Hour
	}
	return d
}

// ParseRetryAfter reads a Retry-After hint, clamped to a sane maximum.
func ParseRetryAfter(raw string, fallback, max time.Duration) time.Duration {
	if parsed := parseRetryAfterValue(raw, time.Now()); parsed > 0 {
		return clampDuration(parsed, max)
	}
	return clampDuration(fallback, max)
}

// ParseRetryAfterHint checks the header first, then the body, then falls back.
//
// All three sources matter in practice: some upstreams send the header, some
// embed the reset time in the JSON body, and some send neither.
func ParseRetryAfterHint(body, header string, fallback, max time.Duration) time.Duration {
	now := time.Now()
	if parsed := parseRetryAfterValue(header, now); parsed > 0 {
		return clampDuration(parsed, max)
	}
	if parsed := retryAfterFromBody(body, now); parsed > 0 {
		return clampDuration(parsed, max)
	}
	return clampDuration(fallback, max)
}

// parseRetryAfterValue accepts three encodings seen in the wild:
// a Go duration, a number of seconds, and an absolute epoch timestamp in
// milliseconds (which is what a JS backend naturally emits).
func parseRetryAfterValue(raw string, now time.Time) time.Duration {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0
	}
	if d, err := time.ParseDuration(text); err == nil && d > 0 {
		return d
	}
	if deadline, err := http.ParseTime(text); err == nil {
		return deadline.Sub(now)
	}
	if value, err := strconv.ParseFloat(text, 64); err == nil && value > 0 {
		if math.IsInf(value, 0) || math.IsNaN(value) {
			return 0
		}
		// Plausible epoch-milliseconds range (roughly 2001..5138).
		if value >= 1e12 && value < 1e14 {
			ms := int64(value)
			return time.UnixMilli(ms).Sub(now)
		}
		// Plausible epoch-seconds range (roughly 2001..5138).
		if value >= 1e9 && value < 1e11 {
			sec := int64(value)
			return time.Unix(sec, 0).Sub(now)
		}
		// Otherwise treat it as a plain second count.
		if value >= float64(math.MaxInt64)/float64(time.Second) {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(value * float64(time.Second))
	}
	return 0
}

// retryAfterFromBody digs a reset hint out of a JSON error body.
func retryAfterFromBody(body string, now time.Time) time.Duration {
	text := strings.TrimSpace(body)
	if text == "" || !strings.HasPrefix(text, "{") {
		return 0
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(text), &parsed) != nil {
		return 0
	}
	// Check the fields most likely to carry a reset, shallow then nested.
	for _, key := range []string{"retry_after", "retryAfter", "reset_at", "resetAt", "reset_time", "resetTime"} {
		if d := retryAfterFromJSONValue(parsed[key], now); d > 0 {
			return d
		}
	}
	if errObj, ok := parsed["error"].(map[string]any); ok {
		for _, key := range []string{"retry_after", "retryAfter", "reset_at", "resetAt"} {
			if d := retryAfterFromJSONValue(errObj[key], now); d > 0 {
				return d
			}
		}
		if data, ok := errObj["data"].(map[string]any); ok {
			for _, key := range []string{"retry_after", "retryAfter", "reset_at", "resetAt"} {
				if d := retryAfterFromJSONValue(data[key], now); d > 0 {
					return d
				}
			}
		}
	}
	return 0
}

func retryAfterFromJSONValue(value any, now time.Time) time.Duration {
	switch v := value.(type) {
	case float64:
		return parseRetryAfterValue(strconv.FormatFloat(v, 'f', -1, 64), now)
	case int64:
		return parseRetryAfterValue(strconv.FormatInt(v, 10), now)
	case string:
		return parseRetryAfterValue(v, now)
	}
	return 0
}

// clampDuration bounds a value to max (when positive) and never returns
// negative, so a stale timestamp cannot produce an instant retry.
func clampDuration(d, max time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if max > 0 && d > max {
		return max
	}
	return d
}

// ClassifyOptions carries the tunables Classify needs so the decision table
// stays a pure function.
type ClassifyOptions struct {
	// DefaultCooldown is used when no kind-specific or header value applies.
	DefaultCooldown time.Duration
	// MaxRetryAfter clamps any hint.
	MaxRetryAfter time.Duration
	// MinRateLimitCooldown is the floor for a rate-limit cooldown. Without a
	// floor, an upstream reporting "0 seconds" would produce a hot retry loop.
	MinRateLimitCooldown time.Duration
	// Model scopes the resulting cooldown to one model.
	Model string
	// 空值按旧版共享额度处理，只有显式独立额度才允许换号。
	QuotaScope string
}

// Classify maps one upstream failure onto a failure kind and the action to
// take. This is the decision table the whole failure policy rests on.
//
// The ORDER of the checks is the design, not an implementation detail:
// quota is checked before auth because upstreams commonly return 403 for an
// exhausted allowance, and treating that as an auth failure would cool for
// seconds and immediately retry into the same exhausted quota.
func Classify(status int, body, retryAfter, kindHint string, opts ClassifyOptions) Classified {
	msg, code, typ, nestedKind := extractError(body)

	kind := strings.TrimSpace(firstNonEmpty(kindHint, nestedKind))
	lower := strings.ToLower(msg + " " + code + " " + typ)

	if kind == "" {
		switch {
		case quotaLike(lower, code, typ):
			kind = KindQuota
		case modelNotAvailableLike(lower, code):
			kind = KindModelNotAvailable
		case notReadyLike(lower):
			kind = KindNotReady
		case IsInvalidRequestText(lower):
			kind = KindInvalidRequest
		case authLike(lower) && !quotaLike(lower, code, typ) && !rateLike(lower):
			kind = KindAuth
		case rateLike(lower) || code == "429" || status == 429:
			kind = KindRateLimit
		case status == 401 || status == 403:
			kind = KindAuth
		default:
			kind = KindUnavailable
		}
	}

	// Post-hoc corrections that override the first-pass guess. A prompt-limit
	// rejection is always a request problem, and a quota word always means
	// quota regardless of which status code carried it.
	if promptLimitLike(lower) {
		kind = KindInvalidRequest
	}
	if kind == KindAuth && quotaLike(lower, code, typ) {
		kind = KindQuota
	}
	if kind == KindRateLimit && quotaLike(lower, code, typ) {
		kind = KindQuota
	}

	out := Classified{
		Kind:    kind,
		Message: strings.TrimSpace(msg),
		Code:    firstNonEmpty(code, kind),
		Type:    firstNonEmpty(typ, "api_error"),
		Model:   opts.Model,
	}

	switch kind {
	case KindQuota:
		out.Status = 429
		out.Failover = opts.QuotaScope == "account" || opts.QuotaScope == "model"
		out.Cooldown = ParseRetryAfterHint(body, retryAfter, NextLocalMidnightCooldown(), 24*time.Hour)
		if opts.QuotaScope == "account" {
			out.Model = ""
		}
		out.Code = firstNonEmpty(code, "insufficient_quota")
		out.Type = "insufficient_quota"

	case KindRateLimit:
		out.Status = 429
		out.Failover = true
		out.Cooldown = ParseRetryAfterHint(body, retryAfter, 60*time.Second, opts.MaxRetryAfter)
		if opts.MinRateLimitCooldown > 0 && out.Cooldown < opts.MinRateLimitCooldown {
			out.Cooldown = opts.MinRateLimitCooldown
		}

	case KindAuth:
		// 同一份凭证服务多个模型，失效时必须冷却整个账号。
		out.Model = ""
		if status == 401 {
			out.Status = 401
		} else {
			out.Status = 403
		}
		out.Failover = true
		out.Cooldown = ParseRetryAfterHint(body, retryAfter, 30*time.Second, opts.MaxRetryAfter)
		out.Code = firstNonEmpty(code, "unauthorized")

	case KindNotReady:
		out.Model = ""
		out.Status = 503
		out.Failover = true
		out.Cooldown = ParseRetryAfterHint(body, retryAfter, 10*time.Second, opts.MaxRetryAfter)
		out.Code = firstNonEmpty(code, "not_ready")

	case KindInvalidRequest:
		// The request was rejected on its merits. Another account would
		// reject it identically, so failing over only burns quota, and the
		// account itself is healthy so it must not be cooled.
		if promptLimitLike(lower) {
			out.Status = 400
		} else {
			out.Status = status
			if out.Status < 400 {
				out.Status = 400
			}
		}
		out.Failover = false
		out.Cooldown = 0
		out.Type = firstNonEmpty(typ, "invalid_request_error")
		out.Code = firstNonEmpty(code, "invalid_request")

	case KindModelNotAvailable:
		// The account is healthy; only its catalog lacks this model, and a
		// stale snapshot may be the cause. Failing over can help, but cooling
		// the account down would be wrong.
		out.Status = 400
		out.Failover = true
		out.Cooldown = 0
		out.Type = firstNonEmpty(typ, "invalid_request_error")
		out.Code = firstNonEmpty(code, "model_not_available")

	default:
		switch {
		case status >= 400:
			out.Status = status
		default:
			out.Status = 502
		}
		out.Failover = true
		out.Cooldown = ParseRetryAfterHint(body, retryAfter, opts.DefaultCooldown, opts.MaxRetryAfter)
		out.Code = firstNonEmpty(code, "upstream_error")
	}

	out.RetryAfter = out.Cooldown
	if out.Message == "" {
		out.Message = out.Code
	}
	return out
}

// extractError pulls message/code/type/kind out of the many error envelopes
// upstreams use. It tolerates the flat form, the {error:{...}} form, and the
// {error:{data:{...}}} nesting seen from Tencent services.
func extractError(body string) (msg, code, typ, kind string) {
	text := strings.TrimSpace(body)
	if text == "" || !strings.HasPrefix(text, "{") {
		// A non-JSON body (an HTML gateway error page, say) is still worth
		// keeping a short excerpt of for diagnostics.
		if text != "" && len(text) <= 200 {
			return text, "", "", ""
		}
		return "", "", "", ""
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(text), &parsed) != nil {
		return "", "", "", ""
	}

	if errObj, ok := parsed["error"].(map[string]any); ok {
		msg, _ = errObj["message"].(string)
		if msg == "" {
			msg, _ = errObj["msg"].(string)
		}
		code = stringifyJSONCode(errObj["code"])
		typ, _ = errObj["type"].(string)
		kind, _ = errObj["kind"].(string)
		if data, ok := errObj["data"].(map[string]any); ok {
			if msg == "" {
				msg, _ = data["message"].(string)
				if msg == "" {
					msg, _ = data["msg"].(string)
				}
			}
			if code == "" {
				code = stringifyJSONCode(data["code"])
			}
			if typ == "" {
				typ, _ = data["type"].(string)
			}
			if kind == "" {
				kind, _ = data["kind"].(string)
			}
		}
		return msg, code, typ, kind
	}

	if m, ok := parsed["message"].(string); ok {
		msg = m
	}
	if msg == "" {
		if m, ok := parsed["msg"].(string); ok {
			msg = m
		}
	}
	code = stringifyJSONCode(parsed["code"])
	if t, ok := parsed["type"].(string); ok {
		typ = t
	}
	if k, ok := parsed["kind"].(string); ok {
		kind = k
	}
	return msg, code, typ, kind
}

// stringifyJSONCode renders a code that may arrive as a string or a number.
// Comparing codes as strings keeps the numeric codes (6004, 11128, 12153)
// usable without a second code path.
func stringifyJSONCode(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	case json.Number:
		return t.String()
	}
	return ""
}

// quotaLike reports whether the text describes an exhausted allowance.
//
// The wording matters: these phrases appear in 403 and 429 responses, and
// misclassifying them as auth or rate-limit is what produces the "retry every
// 30 seconds into a dead quota" failure mode.
func quotaLike(lower, code, typ string) bool {
	if typ == "insufficient_quota" {
		return true
	}
	for _, needle := range []string{
		"insufficient_quota", "insufficient quota",
		"quota exceeded", "quota_exceeded", "exceeded your current quota",
		"out of credits", "credit exhausted", "credits exhausted",
		"balance insufficient", "insufficient balance",
		"额度不足", "额度已用完", "余额不足", "积分不足", "已用完",
		"usage limit", "usage_limit", "limit reached",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// promptLimitLike reports whether the request exceeded a prompt or context
// limit. This is a request problem, never an account problem.
func promptLimitLike(lower string) bool {
	for _, needle := range []string{
		"prompt is too long", "prompt too long", "maximum context length",
		"context length exceeded", "context_length_exceeded",
		"too many tokens", "reduce the length", "input is too long",
		"exceeds the maximum", "请求过长", "超出上下文",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// IsInvalidRequestText reports whether the error text indicates a malformed or
// unacceptable request rather than an infrastructure problem.
func IsInvalidRequestText(lower string) bool {
	for _, needle := range []string{
		"invalid_request_error", "invalid request",
		"messages: at least one message", "must be a valid",
		"unsupported parameter", "unknown parameter",
		"missing required", "required field",
		"invalid type", "invalid format",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// rateLike reports whether the text describes request-rate limiting. The
// phrases are matched on a lowercased haystack, so "429" is checked as a code
// separately by the caller.
func rateLike(lower string) bool {
	for _, needle := range []string{
		"rate limit", "rate_limit", "ratelimit", "too many requests",
		"slow down", "requests per", "tpm", "rpm",
		"频率限制", "请求过于频繁", "限流",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// AuthLike reports whether the text describes a credential problem.
func authLike(lower string) bool {
	for _, needle := range []string{
		"unauthorized", "invalid api key", "invalid_api_key", "invalid token",
		"token expired", "token_expired", "expired token", "authentication",
		"not logged in", "login required", "credential",
		"offline user session not found", "session not found",
		"未登录", "登录已过期", "凭证",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func notReadyLike(lower string) bool {
	for _, needle := range []string{
		"not ready", "not_ready", "initializing", "starting up",
		"service unavailable", "temporarily unavailable",
		"未就绪", "服务不可用",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// modelNotAvailableLike reports whether the account simply does not carry the
// requested model.
func modelNotAvailableLike(lower, code string) bool {
	for _, needle := range []string{
		"model_not_found", "model not found", "no such model",
		"model is not available", "model not available",
		"unsupported model", "does not support model",
		"模型不存在", "模型不可用", "不支持该模型",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
