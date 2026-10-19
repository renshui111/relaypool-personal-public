package pool

import (
	"strings"
	"testing"
	"time"
)

func testOpts() ClassifyOptions {
	return ClassifyOptions{
		DefaultCooldown:      15 * time.Second,
		MaxRetryAfter:        10 * time.Minute,
		MinRateLimitCooldown: 30 * time.Second,
	}
}

// The two cases below are the most consequential entries in the table: both
// must refuse to fail over, because retrying another account cannot help and
// would only burn that account's allowance too.
func TestClassifyQuotaDoesNotFailover(t *testing.T) {
	c := Classify(429, `{"error":{"message":"insufficient_quota","code":"insufficient_quota"}}`, "", "", testOpts())
	if c.Kind != KindQuota {
		t.Fatalf("kind=%q, want %q", c.Kind, KindQuota)
	}
	if c.Failover {
		t.Errorf("quota must NOT fail over: the whole pool shares the allowance")
	}
	if c.Status != 429 {
		t.Errorf("status=%d, want 429", c.Status)
	}
	// The cooldown must reach the next local midnight, not a fixed interval.
	if c.Cooldown < time.Minute {
		t.Errorf("quota cooldown=%s, want until the next local midnight", c.Cooldown)
	}
}

func TestClassifyInvalidRequestDoesNotFailoverOrCool(t *testing.T) {
	c := Classify(400, `{"error":{"message":"missing required field: model","type":"invalid_request_error"}}`, "", "", testOpts())
	if c.Kind != KindInvalidRequest {
		t.Fatalf("kind=%q, want %q", c.Kind, KindInvalidRequest)
	}
	if c.Failover {
		t.Errorf("invalid_request must NOT fail over: every account rejects it identically")
	}
	if c.Cooldown != 0 {
		t.Errorf("invalid_request cooldown=%s, want 0 (the account is healthy)", c.Cooldown)
	}
}

func TestClassifyModelNotAvailableFailsOverWithoutCooling(t *testing.T) {
	c := Classify(400, `{"error":{"message":"model not found: foo","code":"model_not_found"}}`, "", "", testOpts())
	if c.Kind != KindModelNotAvailable {
		t.Fatalf("kind=%q, want %q", c.Kind, KindModelNotAvailable)
	}
	if !c.Failover {
		t.Errorf("model_not_available SHOULD fail over: another account may carry the model")
	}
	if c.Cooldown != 0 {
		t.Errorf("model_not_available cooldown=%s, want 0 (the account is healthy)", c.Cooldown)
	}
}

func TestClassifyRateLimitFailsOver(t *testing.T) {
	c := Classify(429, `{"error":{"message":"rate limit exceeded, slow down"}}`, "", "", testOpts())
	if c.Kind != KindRateLimit {
		t.Fatalf("kind=%q, want %q", c.Kind, KindRateLimit)
	}
	if !c.Failover {
		t.Errorf("rate_limit should fail over")
	}
	if c.Cooldown < 30*time.Second {
		t.Errorf("cooldown=%s, want at least the 30s floor", c.Cooldown)
	}
}

func TestClassifyAuthFailsOver(t *testing.T) {
	c := Classify(401, `{"error":{"message":"invalid token"}}`, "", "", testOpts())
	if c.Kind != KindAuth {
		t.Fatalf("kind=%q, want %q", c.Kind, KindAuth)
	}
	if !c.Failover {
		t.Errorf("auth should fail over to another account")
	}
	if c.Status != 401 {
		t.Errorf("status=%d, want 401", c.Status)
	}
}

// Upstreams commonly report an exhausted allowance with 403. Treating that as
// an auth failure would cool for 30 seconds and immediately retry into the
// same dead quota, so the quota word must win.
func TestClassify403WithQuotaWordingBecomesQuota(t *testing.T) {
	c := Classify(403, `{"error":{"message":"额度不足，请充值"}}`, "", "", testOpts())
	if c.Kind != KindQuota {
		t.Fatalf("403+quota wording: kind=%q, want %q", c.Kind, KindQuota)
	}
	if c.Failover {
		t.Errorf("403+quota must not fail over")
	}
	if c.Cooldown < time.Minute {
		t.Errorf("cooldown=%s, want until the next local midnight", c.Cooldown)
	}
}

// The same rule applies to 429: a rate-limit status carrying quota wording is
// a quota problem, not a rate problem.
func TestClassify429WithQuotaWordingBecomesQuota(t *testing.T) {
	c := Classify(429, `{"error":{"message":"usage limit reached for this period"}}`, "", "", testOpts())
	if c.Kind != KindQuota {
		t.Fatalf("429+usage-limit wording: kind=%q, want %q", c.Kind, KindQuota)
	}
}

// A prompt-length rejection is a request problem even when it arrives with a
// rate-limit status.
func TestClassifyPromptLimitBecomesInvalidRequest(t *testing.T) {
	c := Classify(429, `{"error":{"message":"prompt is too long: 200000 tokens > 128000 maximum context length"}}`, "", "", testOpts())
	if c.Kind != KindInvalidRequest {
		t.Fatalf("kind=%q, want %q", c.Kind, KindInvalidRequest)
	}
	if c.Failover {
		t.Errorf("a prompt-limit rejection must not fail over")
	}
	if c.Status != 400 {
		t.Errorf("status=%d, want 400", c.Status)
	}
}

func TestClassifyNotReady(t *testing.T) {
	c := Classify(503, `{"error":{"message":"service temporarily unavailable"}}`, "", "", testOpts())
	if c.Kind != KindNotReady {
		t.Fatalf("kind=%q, want %q", c.Kind, KindNotReady)
	}
	if !c.Failover {
		t.Errorf("not_ready should fail over")
	}
	if c.Status != 503 {
		t.Errorf("status=%d, want 503", c.Status)
	}
}

func TestClassifyUnavailableIsTheCatchAll(t *testing.T) {
	c := Classify(500, `{"error":{"message":"internal server error"}}`, "", "", testOpts())
	if c.Kind != KindUnavailable {
		t.Fatalf("kind=%q, want %q", c.Kind, KindUnavailable)
	}
	if !c.Failover {
		t.Errorf("5xx should fail over")
	}
}

// A kindHint from the adapter must override the text-based guess: the adapter
// knows the protocol's error codes better than a keyword match does.
func TestClassifyKindHintWins(t *testing.T) {
	c := Classify(400, `{"error":{"message":"something odd"}}`, "", KindQuota, testOpts())
	if c.Kind != KindQuota {
		t.Fatalf("kindHint should win: kind=%q, want %q", c.Kind, KindQuota)
	}
}

// A 401 with an empty body must still classify as auth.
func TestClassify401EmptyBody(t *testing.T) {
	c := Classify(401, "", "", "", testOpts())
	if c.Kind != KindAuth {
		t.Fatalf("kind=%q, want %q", c.Kind, KindAuth)
	}
	if !c.Failover {
		t.Errorf("401 should fail over")
	}
}

// The nested {error:{data:{...}}} envelope is what Tencent services return, so
// the message must be found inside it.
func TestExtractErrorNestedDataEnvelope(t *testing.T) {
	body := `{"error":{"code":6004,"data":{"message":"usage limit reached","code":6004}}}`
	msg, code, _, _ := extractError(body)
	if !strings.Contains(strings.ToLower(msg), "usage limit") {
		t.Errorf("message=%q, want it extracted from error.data", msg)
	}
	if code != "6004" {
		t.Errorf("code=%q, want 6004", code)
	}
}

func TestExtractErrorNumericCode(t *testing.T) {
	_, code, _, _ := extractError(`{"error":{"code":11128,"message":"first message is not system prompt"}}`)
	if code != "11128" {
		t.Errorf("code=%q, want 11128", code)
	}
}

// Retry-After parsing must accept a duration, a second count, and an absolute
// epoch-millisecond timestamp, because upstreams emit all three.
func TestParseRetryAfterEncodings(t *testing.T) {
	max := 10 * time.Minute
	now := time.Now()

	if got := ParseRetryAfter("45s", time.Second, max); got != 45*time.Second {
		t.Errorf("duration form: got %s, want 45s", got)
	}
	if got := ParseRetryAfter("120", time.Second, max); got != 2*time.Minute {
		t.Errorf("seconds form: got %s, want 2m", got)
	}
	futureMs := now.Add(90 * time.Second).UnixMilli()
	got := ParseRetryAfter(strings.TrimSpace(itoa(futureMs)), time.Second, max)
	if got < 80*time.Second || got > 91*time.Second {
		t.Errorf("epoch-ms form: got %s, want ~90s", got)
	}
}

// A hostile or buggy Retry-After must not freeze an account for days.
func TestParseRetryAfterIsClamped(t *testing.T) {
	max := 10 * time.Minute
	if got := ParseRetryAfter("720h", time.Second, max); got != max {
		t.Errorf("got %s, want it clamped to %s", got, max)
	}
}

// The body is the second source checked, after the header.
func TestParseRetryAfterFromBody(t *testing.T) {
	body := `{"error":{"message":"slow down","reset_at":9999999999}}`
	got := ParseRetryAfterHint(body, "", time.Second, time.Hour)
	if got <= 0 {
		t.Errorf("expected a positive hint parsed from the body, got %s", got)
	}
}

// A negative or zero hint must never produce a hot retry loop.
func TestParseRetryAfterNeverNegative(t *testing.T) {
	pastMs := time.Now().Add(-time.Hour).UnixMilli()
	got := ParseRetryAfter(itoa(pastMs), time.Second, time.Minute)
	if got < 0 {
		t.Errorf("got %s, want a non-negative duration", got)
	}
}

// The rate-limit floor exists so a "0 seconds" report does not cause an
// immediate retry.
func TestRateLimitFloorApplied(t *testing.T) {
	opts := testOpts()
	opts.MinRateLimitCooldown = 30 * time.Second
	c := Classify(429, `{"error":{"message":"rate limit"}}`, "0s", "", opts)
	if c.Cooldown < 30*time.Second {
		t.Errorf("cooldown=%s, want at least the 30s floor", c.Cooldown)
	}
}

func TestNextLocalMidnightCooldownIsWithinADay(t *testing.T) {
	d := NextLocalMidnightCooldown()
	if d <= 0 || d > 24*time.Hour {
		t.Errorf("got %s, want a positive duration no longer than 24h", d)
	}
}

func TestIsInvalidRequestText(t *testing.T) {
	if !IsInvalidRequestText("messages: at least one message is required") {
		t.Errorf("expected an invalid-request match")
	}
	if IsInvalidRequestText("internal server error") {
		t.Errorf("a server error is not an invalid request")
	}
}

// A non-JSON body (an HTML gateway error page) should still yield a short
// diagnostic excerpt rather than nothing.
func TestExtractErrorNonJSONBody(t *testing.T) {
	msg, _, _, _ := extractError("<html><body>502 Bad Gateway</body></html>")
	if msg == "" {
		t.Errorf("expected a short excerpt from a non-JSON body")
	}
}

func TestClassifyStatusFallsBackTo502(t *testing.T) {
	c := Classify(0, "", "", "", testOpts())
	if c.Status != 502 {
		t.Errorf("status=%d, want 502 for a transport-level failure", c.Status)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
