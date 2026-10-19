package workbuddy

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
)

// readErrorBody reads a capped error body.
//
// The cap matters: an error path that reads an unbounded body is a
// memory-exhaustion vector, and only the first few kilobytes are ever useful
// for classification.
func readErrorBody(reader io.Reader) string {
	limited := io.LimitReader(reader, maxErrorBody)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return ""
	}
	return string(raw)
}

// newFailure builds a provider.Failure with the adapter's context attached.
func newFailure(status int, kindHint, code, message string, cause error) *provider.Failure {
	return &provider.Failure{
		Status:   status,
		KindHint: kindHint,
		Code:     code,
		Message:  message,
		Err:      cause,
	}
}

// classifyHTTP turns a non-2xx upstream response into a Failure with a kind
// hint.
//
// This is where the adapter's protocol knowledge is applied. The generic
// classifier in the pool package can only read prose; here we can use the
// actual WorkBuddy codes, which are far more reliable than wording.
//
// The classification is NOT authoritative on its own: the executor still runs
// the body through the pool classifier, and this hint is only one input. That
// is deliberate, because the upstream sometimes omits the code and ships a
// message that contradicts the status.
func classifyHTTP(status int, retryAfterHeader, body string) *provider.Failure {
	failure := &provider.Failure{
		Status: status,
		Body:   body,
	}

	code, message, messageType, kindField := decodeUpstreamError(body)
	failure.Code = code
	failure.Message = message
	failure.Type = messageType
	failure.KindHint = kindField
	failure.RetryAfter = parseRetryAfter(retryAfterHeader, body)

	switch {
	// A dead session must NOT be cooled like an auth failure: it needs a token
	// refresh, and cooling it for 30 seconds would just repeat the same error.
	case code == strconv.Itoa(codeSessionDead) || containsAny(message, textSessionDead):
		failure.KindHint = "auth"
		if failure.Message == "" {
			failure.Message = "WorkBuddy session expired; the account needs to sign in again"
		}

	// A usage limit carries an absolute reset time. The generic fallback would
	// retry seconds later and burn more of an already-spent allowance.
	case code == strconv.Itoa(codeRateLimit):
		failure.KindHint = "quota"
		if failure.Message == "" {
			failure.Message = "WorkBuddy usage limit reached"
		}
		if failure.RetryAfter == 0 {
			if reset := quotaResetFrom(message); reset > 0 {
				failure.RetryAfter = reset
			}
		}

	// The system-prompt requirement is a REQUEST problem, not an account one.
	// Reporting it as invalid_request keeps a healthy account in rotation.
	case code == strconv.Itoa(codeMissingSystemPrompt) || containsAny(message, textMissingSystemPrompt):
		failure.KindHint = "invalid_request"
		if failure.Message == "" {
			failure.Message = "WorkBuddy requires the first message to be a system message"
		}

	// 空消息由请求体引起，换号或冷却账号都不能解决。
	case code == strconv.Itoa(codeEmptyMessage) || containsAny(message, textEmptyMessage):
		failure.KindHint = "invalid_request"
		if failure.Message == "" {
			failure.Message = "WorkBuddy 不接受空消息正文"
		}

	case status == 401 || status == 403:
		failure.KindHint = "auth"

	case status == 429:
		failure.KindHint = "rate_limit"

	case status == 503:
		failure.KindHint = "not_ready"
	}

	if failure.Message == "" {
		if body != "" {
			failure.Message = truncate(strings.TrimSpace(body), 200)
		} else {
			failure.Message = fmt.Sprintf("WorkBuddy returned HTTP %d", status)
		}
	}
	return failure
}

// decodeUpstreamError extracts code, message, type and any explicit kind.
//
// WorkBuddy nests its real payload under error.data, and the numeric code may
// arrive as a number or a string, so both spellings and both types are handled.
func decodeUpstreamError(body string) (code, message, messageType, kind string) {
	text := strings.TrimSpace(body)
	if text == "" || !strings.HasPrefix(text, "{") {
		return "", "", "", ""
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(text), &parsed) != nil {
		return "", "", "", ""
	}

	errObj, _ := parsed["error"].(map[string]any)
	if errObj == nil {
		// Some endpoints return the error shape at the top level.
		errObj = parsed
	}
	message, _ = errObj["message"].(string)
	if message == "" {
		message, _ = errObj["msg"].(string)
	}
	code = stringifyCode(errObj["code"])
	messageType, _ = errObj["type"].(string)
	kind, _ = errObj["kind"].(string)

	if data, ok := errObj["data"].(map[string]any); ok {
		if message == "" {
			message, _ = data["message"].(string)
			if message == "" {
				message, _ = data["msg"].(string)
			}
		}
		if code == "" {
			code = stringifyCode(data["code"])
		}
		if messageType == "" {
			messageType, _ = data["type"].(string)
		}
		if kind == "" {
			kind, _ = data["kind"].(string)
		}
	}
	return code, message, messageType, kind
}

// stringifyCode renders a code that may be a number or a string.
func stringifyCode(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(v, 10)
	case json.Number:
		return v.String()
	}
	return ""
}

// parseRetryAfter reads a retry hint from the header, then the body.
func parseRetryAfter(header, body string) time.Duration {
	now := time.Now()
	if d := parseRetryAfterValue(header, now); d > 0 {
		return clampDuration(d, pool.DefaultMaxRetryAfter)
	}
	for _, key := range []string{"retryAfter", "retry_after", "resetAt", "reset_at"} {
		if d := parseRetryAfterFromJSON(body, key, now); d > 0 {
			return clampDuration(d, pool.DefaultMaxRetryAfter)
		}
	}
	return 0
}

func parseRetryAfterFromJSON(body, key string, now time.Time) time.Duration {
	text := strings.TrimSpace(body)
	if text == "" || !strings.HasPrefix(text, "{") {
		return 0
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(text), &parsed) != nil {
		return 0
	}
	candidates := []any{parsed[key]}
	if errObj, ok := parsed["error"].(map[string]any); ok {
		candidates = append(candidates, errObj[key])
		if data, ok := errObj["data"].(map[string]any); ok {
			candidates = append(candidates, data[key])
		}
	}
	for _, candidate := range candidates {
		switch v := candidate.(type) {
		case string:
			if d := parseRetryAfterValue(v, now); d > 0 {
				return d
			}
		case float64:
			if d := parseRetryAfterValue(strconv.FormatFloat(v, 'f', -1, 64), now); d > 0 {
				return d
			}
		case json.Number:
			if d := parseRetryAfterValue(v.String(), now); d > 0 {
				return d
			}
		}
	}
	return 0
}

// parseRetryAfterValue accepts a duration, a second count, or an absolute
// epoch timestamp in seconds or milliseconds.
func parseRetryAfterValue(raw string, now time.Time) time.Duration {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0
	}
	if d, err := time.ParseDuration(text); err == nil && d > 0 {
		return d
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || value <= 0 {
		return 0
	}
	switch {
	case value >= 1e12 && value < 1e14:
		// Epoch milliseconds: what a JavaScript backend naturally emits.
		return time.Unix(0, int64(value)*int64(time.Millisecond)).Sub(now)
	case value >= 1e9 && value < 1e11:
		// Epoch seconds.
		return time.Unix(int64(value), 0).Sub(now)
	default:
		return time.Duration(value * float64(time.Second))
	}
}

func clampDuration(d, max time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if max > 0 && d > max {
		return max
	}
	return d
}

// quotaResetFrom parses the absolute reset time out of a CN usage-limit message
// such as "... 将于 2026-09-30 12:00:00 (UTC+8) 重置".
//
// WorkBuddy reports the reset in a fixed UTC+8 zone regardless of the client's
// locale, so the parsed time is anchored there rather than in local time.
func quotaResetFrom(message string) time.Duration {
	text := strings.TrimSpace(message)
	if text == "" {
		return 0
	}
	// Look for an ISO-like or space-separated timestamp.
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006/01/02 15:04:05",
		"2006-01-02 15:04",
	} {
		if idx := findTimestamp(text, layout); idx >= 0 {
			candidate := text[idx : idx+len(layout)]
			parsed, err := time.ParseInLocation(layout, candidate, quotaResetLocation)
			if err == nil {
				if d := time.Until(parsed.UTC()); d > 0 {
					return d
				}
			}
		}
	}
	return 0
}

// findTimestamp locates a substring matching the layout's shape.
//
// This is a shape match rather than a strict parse because the timestamp is
// embedded in prose whose exact wording changes.
func findTimestamp(text, layout string) int {
	// Validate the layout against a probe value so the pattern length is the
	// layout's own length.
	probe := "2006-01-02 15:04:05"
	if len(layout) != len(probe) && layout != "2006-01-02 15:04" {
		// Only the shapes above are handled; anything else is a mistake.
		return -1
	}
	for i := 0; i+len(layout) <= len(text); i++ {
		window := text[i : i+len(layout)]
		if _, err := time.ParseInLocation(layout, window, quotaResetLocation); err == nil {
			return i
		}
	}
	return -1
}

// containsAny reports whether haystack contains any needle.
func containsAny(haystack string, needles ...string) bool {
	lower := strings.ToLower(haystack)
	for _, needle := range needles {
		if needle == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(needle)) {
			return true
		}
	}
	return false
}
