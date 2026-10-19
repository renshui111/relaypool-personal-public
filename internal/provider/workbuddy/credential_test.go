package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

// ---- credential decoding ----

func TestDecodeCredentialCanonicalFlatForm(t *testing.T) {
	raw := []byte(`{
		"access_token":"at_123","refresh_token":"rt_456","expires_at":1790000000,
		"domain":"codebuddy.cn","uid":"u_1","enterprise_id":"e_1","nickname":"me"
	}`)
	cred, err := DecodeCredential(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cred.AccessToken != "at_123" || cred.UID != "u_1" {
		t.Errorf("fields not decoded: %+v", cred)
	}
	if !cred.Ready() {
		t.Errorf("credential should be ready")
	}
	if cred.Region() != provider.RegionCN {
		t.Errorf("region=%q, want cn", cred.Region())
	}
}

func TestDecodeCredentialNestedExportForm(t *testing.T) {
	raw := []byte(`{
		"account":{"uid":"u_9","enterpriseId":"e_9","nickname":"nested"},
		"auth":{"accessToken":"at_nested","refreshToken":"rt_nested","expiresAt":1790000123,"domain":"workbuddy.ai"}
	}`)
	cred, err := DecodeCredential(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cred.AccessToken != "at_nested" {
		t.Errorf("access token=%q", cred.AccessToken)
	}
	if cred.UID != "u_9" || cred.EnterpriseID != "e_9" {
		t.Errorf("account fields not decoded: %+v", cred)
	}
	if !cred.IsGlobal() {
		t.Errorf("workbuddy.ai should be the global region, got %q", cred.Region())
	}
}

func TestDecodeCredentialCamelCaseFlatForm(t *testing.T) {
	raw := []byte(`{"accessToken":"at_camel","refreshToken":"rt_camel","expiresAt":1790000999,"enterpriseId":"e_camel"}`)
	cred, err := DecodeCredential(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cred.AccessToken != "at_camel" || cred.EnterpriseID != "e_camel" {
		t.Errorf("camelCase form not decoded: %+v", cred)
	}
}

func TestDecodeCredentialRejectsEmptyAndTokenless(t *testing.T) {
	if _, err := DecodeCredential(nil); err == nil {
		t.Errorf("empty payload should be rejected")
	}
	if _, err := DecodeCredential([]byte(`{"uid":"u"}`)); err == nil {
		t.Errorf("a credential without an access token should be rejected")
	}
}

// A token without a UID is stored but NOT ready: the upstream rejects chat
// without X-User-Id, so calling it ready would produce confusing failures.
func TestCredentialNeedsUIDToBeReady(t *testing.T) {
	cred := Credential{AccessToken: "at", UID: ""}
	if cred.Ready() {
		t.Errorf("a token-only credential must not be ready")
	}
	cred.UID = "u"
	if !cred.Ready() {
		t.Errorf("token+uid should be ready")
	}
}

// Expiry must be interpreted as seconds. Treating the value as milliseconds
// would place it in 1970 and force a refresh on every request.
func TestCredentialExpiryIsSeconds(t *testing.T) {
	cred := Credential{ExpiresAt: 1790000000}
	expiry := cred.Expiry()
	if expiry.Year() < 2026 {
		t.Fatalf("expiry=%s looks like a millisecond misread", expiry)
	}

	// A stored millisecond value should still be normalized rather than
	// producing a year-50000 timestamp.
	cred.ExpiresAt = 1790000000000
	if got := cred.Expiry().Year(); got > 2100 {
		t.Errorf("millisecond value not normalized: year=%d", got)
	}
}

// Region detection must compare hostnames, not substrings: a lookalike domain
// must not be classified as belonging to the service.
func TestRegionUsesHostnameNotSubstring(t *testing.T) {
	cases := []struct {
		domain string
		want   provider.Region
	}{
		{"codebuddy.cn", provider.RegionCN},
		{"www.codebuddy.cn", provider.RegionCN},
		{"https://copilot.tencent.com", provider.RegionCN},
		{"workbuddy.ai", provider.RegionGlobal},
		{"www.workbuddy.ai", provider.RegionGlobal},
		{"codebuddy.ai", provider.RegionGlobal},
		// A lookalike must not be claimed as global.
		{"evil-workbuddy.ai.attacker.com", provider.RegionCN},
		{"", provider.RegionCN},
	}
	for _, tc := range cases {
		cred := Credential{Domain: tc.domain}
		if got := cred.Region(); got != tc.want {
			t.Errorf("domain %q: region=%q, want %q", tc.domain, got, tc.want)
		}
	}
}

// The catalogue path differs by region, and mixing them fails.
func TestCatalogPathDiffersByRegion(t *testing.T) {
	cn := Credential{Domain: "codebuddy.cn"}
	if got := cn.catalogPath(); got != pathModelsCN {
		t.Errorf("cn catalogue path=%q, want %q", got, pathModelsCN)
	}
	global := Credential{Domain: "workbuddy.ai"}
	if got := global.catalogPath(); got != pathModelsGlobal {
		t.Errorf("global catalogue path=%q, want %q", got, pathModelsGlobal)
	}
	if pathModelsCN == pathModelsGlobal {
		t.Fatalf("the two regions must not share a catalogue path")
	}
}

// Origin must follow the credential's region.
func TestOriginFollowsRegion(t *testing.T) {
	cn := Credential{Domain: "codebuddy.cn"}
	if got := cn.origin(); got != "https://www.codebuddy.cn" {
		t.Errorf("cn origin=%q", got)
	}
	global := Credential{Domain: "workbuddy.ai"}
	if got := global.origin(); got != "https://www.workbuddy.ai" {
		t.Errorf("global origin=%q", got)
	}
}

// ---- payload rewriting ----

// The system-prompt requirement is the single most likely cause of a
// works-once-then-breaks bug, because stripping the caller's system prompt is
// the DEFAULT gateway behaviour.
func TestEnsureLeadingSystemInsertsPlaceholder(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	ensureLeadingSystem(body)

	list, ok := body["messages"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("expected 2 messages, got %#v", body["messages"])
	}
	first, _ := list[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("first role=%v, want system", first["role"])
	}
	if content, ok := first["content"].(string); !ok || strings.TrimSpace(content) == "" {
		t.Errorf("补充的系统消息必须非空：%v", first["content"])
	}
	// The original message must still be present and last.
	second, _ := list[1].(map[string]any)
	if second["role"] != "user" {
		t.Errorf("original message was displaced: %#v", second)
	}
}

func TestEnsureLeadingSystemLeavesExistingSystemAlone(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "you are helpful"},
		},
	}
	ensureLeadingSystem(body)
	list := body["messages"].([]any)
	if len(list) != 1 {
		t.Fatalf("an existing system message should not be duplicated, got %d", len(list))
	}
	first := list[0].(map[string]any)
	if first["content"] != "you are helpful" {
		t.Errorf("existing system content was modified: %v", first["content"])
	}
}

func TestEnsureLeadingSystemHandlesEmptyAndMissingMessages(t *testing.T) {
	empty := map[string]any{"messages": []any{}}
	ensureLeadingSystem(empty)
	if list, ok := empty["messages"].([]any); !ok || len(list) != 1 {
		t.Errorf("an empty message list should gain one placeholder, got %#v", empty["messages"])
	}

	// A body with no messages key at all must not gain one: the upstream
	// rejects a request with only a system message more confusingly than one
	// with no messages.
	none := map[string]any{"model": "x"}
	ensureLeadingSystem(none)
	if _, ok := none["messages"]; ok {
		t.Errorf("a message-less body should be left alone")
	}
}

func TestPrepareBodyForcesStreaming(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"m","stream":false,"messages":[{"role":"system","content":""}]}`))
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["stream"] != true {
		t.Errorf("stream=%v, want true (the upstream only reliably supports streaming)", body["stream"])
	}
}

func TestNormalizeToolChoiceUnwrapsFunctionObject(t *testing.T) {
	out := PrepareBody([]byte(`{
		"model":"m",
		"messages":[{"role":"system","content":""}],
		"tools":[{"type":"function","function":{"name":"get_weather"}}],
		"tool_choice":{"type":"function","function":{"name":"get_weather"}}
	}`))
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := body["tool_choice"].(string); got != "get_weather" {
		t.Errorf("tool_choice=%v, want the plain string \"get_weather\"", body["tool_choice"])
	}
}

func TestNormalizeToolChoiceNoneDropsTools(t *testing.T) {
	out := PrepareBody([]byte(`{
		"model":"m",
		"messages":[{"role":"system","content":""}],
		"tools":[{"type":"function","function":{"name":"x"}}],
		"tool_choice":"none"
	}`))
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body["tool_choice"]; ok {
		t.Errorf("tool_choice should be removed for none")
	}
	if _, ok := body["tools"]; ok {
		t.Errorf("tools should be dropped alongside tool_choice=none")
	}
}

// The upstream errors on tools:[] rather than treating it as "no tools".
func TestDropEmptyTools(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"m","messages":[{"role":"system","content":""}],"tools":[]}`))
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body["tools"]; ok {
		t.Errorf("an empty tools array must be removed")
	}

	nullCase := PrepareBody([]byte(`{"model":"m","messages":[{"role":"system","content":""}],"tools":null}`))
	var body2 map[string]any
	if err := json.Unmarshal(nullCase, &body2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body2["tools"]; ok {
		t.Errorf("a null tools value must be removed")
	}
}

// Unknown fields must survive: clients send vendor extensions this adapter does
// not model, and dropping them silently changes behaviour.
func TestPrepareBodyPreservesUnknownFields(t *testing.T) {
	out := PrepareBody([]byte(`{
		"model":"m",
		"messages":[{"role":"system","content":""}],
		"vendor_extension":{"keep":"me"},
		"seed":1234
	}`))
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body["vendor_extension"]; !ok {
		t.Errorf("an unknown field was dropped")
	}
	if body["seed"] != float64(1234) {
		t.Errorf("seed=%v, want 1234", body["seed"])
	}
}

func TestPrepareBodyPassesThroughUnparseableInput(t *testing.T) {
	raw := []byte(`this is not json`)
	if got := string(PrepareBody(raw)); got != string(raw) {
		t.Errorf("unparseable input should pass through unchanged, got %q", got)
	}
}

// ---- reasoning levels ----

func TestNormalizeReasoningLevelMapsMaxToXhigh(t *testing.T) {
	if got := NormalizeReasoningLevel("max"); got != "xhigh" {
		t.Errorf("max should map to xhigh (OpenAI clients send max), got %q", got)
	}
	if got := NormalizeReasoningLevel("HIGH"); got != "high" {
		t.Errorf("levels should fold case, got %q", got)
	}
	if got := NormalizeReasoningLevel("bogus"); got != "" {
		t.Errorf("an unknown level should be empty, got %q", got)
	}
}

// A model that cannot disable thinking must never be sent "none".
func TestResolveReasoningNeverSendsNoneToMandatoryThinkingModel(t *testing.T) {
	got := resolveReasoningLevel("none", []string{"low", "medium", "high"}, "medium", false)
	if got == "none" {
		t.Fatalf("resolved to none for a model that cannot disable thinking")
	}
	if got != "medium" {
		t.Errorf("got %q, want the model default medium", got)
	}
}

func TestResolveReasoningHonoursSupportedOptions(t *testing.T) {
	// "high" is not in this model's list, so it must fall back to the default.
	got := resolveReasoningLevel("high", []string{"low", "medium"}, "medium", true)
	if got != "medium" {
		t.Errorf("got %q, want the supported default medium", got)
	}
	got = resolveReasoningLevel("low", []string{"low", "medium"}, "medium", true)
	if got != "low" {
		t.Errorf("got %q, want low", got)
	}
}

func TestApplyChatReasoningSetsUpstreamShape(t *testing.T) {
	body := map[string]any{}
	applyChatReasoning(body, "high", []string{"none", "low", "medium", "high"}, "medium", true)

	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning object missing: %#v", body)
	}
	if reasoning["effort"] != "high" {
		t.Errorf("effort=%v, want high", reasoning["effort"])
	}
	if reasoning["summary"] != "auto" {
		t.Errorf("summary=%v, want auto", reasoning["summary"])
	}
}

func TestApplyChatReasoningDropsForNone(t *testing.T) {
	body := map[string]any{"reasoning": map[string]any{"effort": "stale"}}
	applyChatReasoning(body, "none", []string{"none", "low"}, "low", true)
	if _, ok := body["reasoning"]; ok {
		t.Errorf("reasoning should be removed when thinking is disabled")
	}
}

// ---- error classification ----

// Code 6004 is a usage limit carrying an absolute reset time. It must classify
// as quota, NOT as a rate limit: cooling for the generic fallback would retry
// seconds later and burn more of an already-spent allowance.
func TestClassifyHTTPUsageLimitCodeBecomesQuota(t *testing.T) {
	body := `{"error":{"code":6004,"message":"usage limit reached"}}`
	failure := classifyHTTP(429, "", body)
	if failure.KindHint != "quota" {
		t.Fatalf("kind=%q, want quota", failure.KindHint)
	}
	if failure.Code != "6004" {
		t.Errorf("code=%q, want 6004", failure.Code)
	}
}

func TestClassifyHTTPDeadSessionBecomesAuth(t *testing.T) {
	body := `{"error":{"code":12153,"message":"Offline user session not found"}}`
	failure := classifyHTTP(400, "", body)
	if failure.KindHint != "auth" {
		t.Errorf("kind=%q, want auth", failure.KindHint)
	}
}

// A dead session identified only by its message must still classify, because
// the upstream sometimes omits the code.
func TestClassifyHTTPDeadSessionByMessageOnly(t *testing.T) {
	body := `{"error":{"message":"Offline user session not found"}}`
	failure := classifyHTTP(400, "", body)
	if failure.KindHint != "auth" {
		t.Errorf("kind=%q, want auth from the message alone", failure.KindHint)
	}
}

// Code 11128 is the system-prompt requirement: a REQUEST problem. Reporting it
// as anything else would take a healthy account out of rotation.
func TestClassifyHTTPMissingSystemPromptBecomesInvalidRequest(t *testing.T) {
	body := `{"error":{"code":11128,"message":"first message is not system prompt"}}`
	failure := classifyHTTP(400, "", body)
	if failure.KindHint != "invalid_request" {
		t.Errorf("kind=%q, want invalid_request", failure.KindHint)
	}
}

func TestClassifyHTTPStatusFallbacks(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{401, "auth"},
		{403, "auth"},
		{429, "rate_limit"},
		{503, "not_ready"},
	}
	for _, tc := range cases {
		failure := classifyHTTP(tc.status, "", `{}`)
		if failure.KindHint != tc.want {
			t.Errorf("status %d: kind=%q, want %q", tc.status, failure.KindHint, tc.want)
		}
	}
}

// The nested error.data envelope is what the upstream actually returns.
func TestDecodeUpstreamErrorNestedData(t *testing.T) {
	body := `{"error":{"code":6004,"data":{"message":"额度不足","code":6004,"type":"limit"}}}`
	code, message, messageType, _ := decodeUpstreamError(body)
	if code != "6004" {
		t.Errorf("code=%q, want 6004", code)
	}
	if message != "额度不足" {
		t.Errorf("message=%q, want it read from error.data", message)
	}
	if messageType != "limit" {
		t.Errorf("type=%q, want limit", messageType)
	}
}

func TestClassifyHTTPAlwaysProducesAMessage(t *testing.T) {
	failure := classifyHTTP(500, "", "")
	if strings.TrimSpace(failure.Message) == "" {
		t.Errorf("a failure must always carry a message")
	}
}

// A Retry-After header must be honoured.
func TestClassifyHTTPReadsRetryAfterHeader(t *testing.T) {
	failure := classifyHTTP(429, "45s", `{}`)
	if failure.RetryAfter != 45*time.Second {
		t.Errorf("retry after=%s, want 45s", failure.RetryAfter)
	}
}

// ---- catalog ----

// The REAL envelope, confirmed against the live upstream. This shape is the
// regression test for a parser that rejected a 200 OK because the model list is
// nested under "data" rather than at the top level.
func TestParseCatalogRealEnvelope(t *testing.T) {
	payload := `{
		"code": 0,
		"msg": "OK",
		"requestId": "ced6be4d",
		"data": {
			"endpoint": "https://copilot.tencent.com",
			"models": [
				{"id":"auto","name":"Auto","maxInputTokens":256000,"maxOutputTokens":32000,
				 "isDefault":true,"onlyReasoning":true,"supportsReasoning":true,
				 "supportsImages":true,"supportsToolCall":true,
				 "reasoning":{"effort":"high","summary":"auto"}},
				{"id":"glm-5.3","name":"GLM-5.3","maxInputTokens":200000,"maxOutputTokens":32000,
				 "supportsReasoning":true,"supportsImages":false,"supportsToolCall":true,
				 "reasoning":{"effort":"medium","summary":"auto"}}
			],
			"agents": [{"name":"cli","models":["auto","hy4-preview","glm-5.3"]}]
		}
	}`
	models, err := parseCatalog(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("parse the real envelope: %v", err)
	}
	byID := map[string]provider.ModelInfo{}
	for _, m := range models {
		byID[m.NativeModel] = m
	}

	auto, ok := byID["auto"]
	if !ok {
		t.Fatalf("auto missing from %d parsed models", len(models))
	}
	if auto.ContextWindow != 256000 {
		t.Errorf("auto context window=%d, want 256000", auto.ContextWindow)
	}
	if auto.MaxOutput != 32000 {
		t.Errorf("auto max output=%d, want 32000", auto.MaxOutput)
	}
	if !auto.Images || !auto.Tools {
		t.Errorf("auto capabilities wrong: images=%v tools=%v", auto.Images, auto.Tools)
	}
	// onlyReasoning means thinking cannot be turned off.
	if auto.CanDisableThinking {
		t.Errorf("auto is onlyReasoning, so thinking must not be disableable")
	}
	if auto.ReasoningDefault != "high" {
		t.Errorf("auto reasoning default=%q, want high", auto.ReasoningDefault)
	}

	glm, ok := byID["glm-5.3"]
	if !ok {
		t.Fatalf("glm-5.3 missing")
	}
	if glm.ReasoningDefault != "medium" {
		t.Errorf("glm-5.3 reasoning default=%q, want medium", glm.ReasoningDefault)
	}
	if !glm.CanDisableThinking {
		t.Errorf("glm-5.3 should allow disabling thinking")
	}
	// "none" must be offered when thinking is optional.
	if !containsLevel(glm.ReasoningOptions, "none") {
		t.Errorf("glm-5.3 options should include none: %v", glm.ReasoningOptions)
	}

	// Agent-only models are merged in, because they are still callable.
	if _, ok := byID["hy4-preview"]; !ok {
		t.Errorf("an agent-only model must be merged in; got %d models", len(models))
	}
}

// A non-zero code on HTTP 200 is an application-level failure. Treating it as
// success would yield an empty catalogue that looks like "no models".
func TestParseCatalogRejectsNonZeroCode(t *testing.T) {
	payload := `{"code":40001,"msg":"token expired","data":null}`
	if _, err := parseCatalog(strings.NewReader(payload)); err == nil {
		t.Fatalf("a non-zero code must be an error")
	}
}

// An empty catalogue is valid and must yield a non-nil empty slice, so the
// scheduler can tell "known to be empty" from "unknown".
func TestCatalogEmptyIsNonNil(t *testing.T) {
	models, err := parseCatalog(strings.NewReader(`{"code":0,"msg":"OK","data":{"models":[]}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if models == nil {
		t.Fatalf("an empty catalogue must be a non-nil slice")
	}
	if len(models) != 0 {
		t.Errorf("expected no models, got %d", len(models))
	}
}

// Duplicate IDs across data.models and agent lists must collapse.
func TestCatalogDeduplicatesModels(t *testing.T) {
	payload := `{"code":0,"data":{"models":[{"id":"glm-5.3"}],"agents":[{"name":"a","models":["glm-5.3","extra"]}]}}`
	models, err := parseCatalog(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	counts := map[string]int{}
	for _, m := range models {
		counts[m.NativeModel]++
	}
	if counts["glm-5.3"] != 1 {
		t.Errorf("glm-5.3 appeared %d times, want 1", counts["glm-5.3"])
	}
	if counts["extra"] != 1 {
		t.Errorf("extra should be merged in once, got %d", counts["extra"])
	}
}

// A flat top-level list is still accepted, so a shape change on a different
// endpoint does not hard-fail.
func TestParseCatalogAcceptsFlatFallback(t *testing.T) {
	models, err := parseCatalog(strings.NewReader(`{"models":[{"id":"flat-model"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(models) != 1 || models[0].NativeModel != "flat-model" {
		t.Errorf("got %+v, want flat-model", models)
	}
}

// Entries without an id are skipped rather than producing an unusable model.
func TestCatalogSkipsEntriesWithoutID(t *testing.T) {
	payload := `{"code":0,"data":{"models":[{"id":"good"},{"id":""},{"name":"no-id"}]}}`
	models, err := parseCatalog(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(models) != 1 || models[0].NativeModel != "good" {
		t.Errorf("got %+v, want only 'good'", models)
	}
}

// A model that is onlyReasoning must not advertise "none" as an option.
func TestCatalogOnlyReasoningCannotDisableThinking(t *testing.T) {
	// Fields are assigned directly rather than via a struct literal so adding a
	// field to catalogModel does not break this test.
	var entry catalogModel
	entry.ID = "thinker"
	entry.OnlyReasoning = true
	entry.Reasoning.SupportedEfforts = []string{"low", "high"}
	entry.Reasoning.DefaultEffort = "low"

	info := catalogEntryToModel(entry)
	if info.CanDisableThinking {
		t.Errorf("an onlyReasoning model must not allow disabling thinking")
	}
	for _, option := range info.ReasoningOptions {
		if option == "none" {
			t.Errorf("none must not be offered: %v", info.ReasoningOptions)
		}
	}
}

// A normal reasoning model should offer "none" so clients can turn thinking off.
func TestCatalogDisableableAddsNoneOption(t *testing.T) {
	entry := catalogModel{
		ID:                "normal",
		SupportsReasoning: true,
	}
	entry.Reasoning.SupportedEfforts = []string{"low", "high"}
	entry.Reasoning.DefaultEffort = "low"

	info := catalogEntryToModel(entry)
	if !info.CanDisableThinking {
		t.Fatalf("a normal model should allow disabling thinking")
	}
	if !containsLevel(info.ReasoningOptions, "none") {
		t.Errorf("none should be offered: %v", info.ReasoningOptions)
	}
}

// The default level must always appear in the option list, or a request asking
// for the default would fail the option check.
func TestCatalogDefaultIsAlwaysInOptions(t *testing.T) {
	entry := catalogModel{ID: "m", SupportsReasoning: true}
	entry.Reasoning.SupportedEfforts = []string{"low", "high"}
	entry.Reasoning.DefaultEffort = "medium" // deliberately absent from the list

	info := catalogEntryToModel(entry)
	if !containsLevel(info.ReasoningOptions, "medium") {
		t.Errorf("the default must be inserted into the options: %v", info.ReasoningOptions)
	}
}

func TestAlreadyCheckedInDetection(t *testing.T) {
	if !alreadyCheckedIn(`{"error":{"message":"今天已签到，请明天再来"}}`) {
		t.Errorf("a Chinese already-checked-in message should be detected")
	}
	if !alreadyCheckedIn(`{"error":{"message":"already checked in today"}}`) {
		t.Errorf("an English already-checked-in message should be detected")
	}
	if alreadyCheckedIn(`{"error":{"message":"internal server error"}}`) {
		t.Errorf("a genuine error must not be reported as already-checked-in")
	}
}

// A check-in rejection must never be confused with a real failure: treating it
// as an error would make the scheduler retry all day.
func TestClassifyHTTPDoesNotTreatGeneric400AsAlreadyCheckedIn(t *testing.T) {
	failure := classifyHTTP(400, "", `{"error":{"message":"bad request"}}`)
	if failure.KindHint == "already" {
		t.Errorf("a generic 400 must not be classified as already-checked-in")
	}
}

// ---- helpers ----

func TestCanonicalModelIDMatchesSchedulerConvention(t *testing.T) {
	cases := map[string]string{
		"GLM-5.2":  "glm-5.2",
		"glm_5.2":  "glm-5.2",
		"glm 5.2 ": "glm-5.2",
	}
	for in, want := range cases {
		if got := canonicalModelID(in); got != want {
			t.Errorf("canonicalModelID(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestQuotaResetParsingFromCNMessage(t *testing.T) {
	message := "本次额度已用完，将于 2099-01-01 12:00:00 (UTC+8) 重置"
	got := quotaResetFrom(message)
	if got <= 0 {
		t.Fatalf("expected a positive reset duration, got %s", got)
	}
}

func TestQuotaResetIgnoresMessagesWithoutTimestamp(t *testing.T) {
	if got := quotaResetFrom("usage limit reached"); got != 0 {
		t.Errorf("got %s, want 0 when no timestamp is present", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short strings should pass through, got %q", got)
	}
	if got := truncate("hello world", 5); !strings.HasPrefix(got, "hello") || !strings.HasSuffix(got, "...") {
		t.Errorf("truncate should cut and mark, got %q", got)
	}
}
