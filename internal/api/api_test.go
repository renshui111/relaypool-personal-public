package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/config"
	"github.com/relaypool/relaypool/internal/provider"
)

// These tests exercise the end-to-end request path through the real HTTP
// handlers, a real store and a real pool, with only the upstream adapter
// replaced. They are the evidence that the pieces actually compose.

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.Pool.DefaultCooldown = 10 * time.Millisecond
	cfg.Session.TTL = time.Minute
	cfg.Session.Capacity = 100
	return cfg
}

// newTestServer builds a Server with a fake provider registered and one ready
// account, returning the httptest server and the fake adapter.
func newTestServer(t *testing.T, adapter *stubProvider) (*httptest.Server, *Server) {
	t.Helper()

	srv := newTestServerNoAdapter(t)
	srv.registry.Register(adapter)

	// Seed the account in BOTH the store (so the credential source can resolve
	// it) and the pool (so scheduling can pick it).
	seedCredential(t, srv.store, "acc_test", adapter.family)
	srv.pool.Upsert(poolItem("acc_test", adapter.family))

	srv.executor = rebuildExecutor(srv, adapter)

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	return httpSrv, srv
}

// addAccount registers another usable account on an existing test server.
func addAccount(t *testing.T, srv *Server, accountID, family string) {
	t.Helper()
	seedCredential(t, srv.store, accountID, family)
	srv.pool.Upsert(poolItem(accountID, family))
}

func newTestServerNoAdapter(t *testing.T) *Server {
	t.Helper()
	cfg := testConfig(t)
	st := openTestStore(t)

	srv := New(Options{Config: cfg, Store: st, APIKey: "rp_test"})
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv
}

// stubProvider is a scripted adapter that speaks the upstream SSE shape.
type stubProvider struct {
	family string
	// reply is the text returned on success.
	reply string
	// failuresBeforeSuccess makes the first N calls fail with the given kind.
	failuresBeforeSuccess int
	failKind              string
	// calls counts upstream invocations.
	calls int
	// lastBody records the last request body seen.
	lastBody map[string]any
}

func newStubProvider() *stubProvider {
	return &stubProvider{family: "workbuddy", reply: "hello from the pool"}
}

func (s *stubProvider) Family() string      { return s.family }
func (s *stubProvider) Kind() provider.Kind { return provider.KindInProcess }

func (s *stubProvider) DecodeCredential(raw []byte) (provider.Credential, error) {
	return provider.Credential{
		Provider: s.family,
		Raw:      raw,
		Fields:   map[string]string{"access_token": "at_test"},
	}, nil
}

func (s *stubProvider) FetchCatalog(context.Context, provider.Credential) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{NativeModel: "glm-5.2", PublicModel: "glm-5.2"}}, nil
}

func (s *stubProvider) Checkin(context.Context, provider.Credential) (provider.CheckinResult, error) {
	return provider.CheckinResult{}, provider.ErrUnsupported
}

func (s *stubProvider) Refresh(context.Context, provider.Credential) (provider.Credential, error) {
	return provider.Credential{}, provider.ErrUnsupported
}

func (s *stubProvider) ChatNonStream(ctx context.Context, cred provider.Credential, req *provider.ChatRequest) (*provider.ChatResult, error) {
	stream, err := s.ChatStream(ctx, cred, req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	body, _ := io.ReadAll(stream.Body)

	// Aggregate the frames we produced, mirroring what the adapter does.
	var text strings.Builder
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			text.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	encoded, _ := json.Marshal(text.String())
	return &provider.ChatResult{
		ID:           "cmpl_test",
		Model:        req.Model,
		Created:      time.Now().Unix(),
		FinishReason: "stop",
		Message:      provider.Message{Role: "assistant", Content: encoded},
		Usage:        provider.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
	}, nil
}

func (s *stubProvider) ChatStream(_ context.Context, _ provider.Credential, req *provider.ChatRequest) (*provider.Stream, error) {
	s.calls++
	if req != nil && len(req.Raw) > 0 {
		var body map[string]any
		if json.Unmarshal(req.Raw, &body) == nil {
			s.lastBody = body
		}
	}
	if s.calls <= s.failuresBeforeSuccess {
		kind := s.failKind
		if kind == "" {
			kind = "rate_limit"
		}
		// The status and body must be CONSISTENT with the kind, because the
		// classifier reads all three: an invalid_request carried on a 429 with
		// a rate-limit body would correctly be classified as a rate limit.
		status := 429
		body := `{"error":{"message":"stubbed upstream failure"}}`
		if kind == "invalid_request" {
			status = 400
			body = `{"error":{"message":"stubbed invalid request","type":"invalid_request_error"}}`
		}
		return nil, &provider.Failure{
			Status:   status,
			KindHint: kind,
			Code:     kind,
			Message:  "stubbed upstream failure",
			Body:     body,
		}
	}

	// Emit the upstream shape, placeholders included, then run it through the
	// same sanitizer the real adapter uses. This matters: the transport is
	// supposed to receive an ALREADY SANITIZED stream, so a stub that skipped
	// the sanitizer would let a blank-frame regression pass unnoticed.
	var raw strings.Builder
	frames := []string{
		`{"id":"cmpl_test","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"","refusal":"","tool_calls":[]},"finish_reason":""}]}`,
		`{"choices":[{"index":0,"delta":{"content":"` + s.reply + `"},"finish_reason":""}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	for _, frame := range frames {
		raw.WriteString("data: ")
		raw.WriteString(frame)
		raw.WriteString("\n\n")
	}
	raw.WriteString("data: [DONE]\n\n")

	return &provider.Stream{
		Body:        io.NopCloser(strings.NewReader(sanitizeForTest(raw.String()))),
		ContentType: "text/event-stream",
		Model:       "glm-5.2",
	}, nil
}

// sanitizeForTest reproduces the adapter's stream cleaning so the stub hands
// the transport what a real adapter would.
//
// It is implemented here rather than exported from the adapter because the
// sanitizer is deliberately unexported: the transport must never call it, since
// doing so would double-clean an already-clean stream.
func sanitizeForTest(stream string) string {
	var out strings.Builder
	sentRole := false

	for _, line := range strings.Split(stream, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			out.WriteString("data: [DONE]\n\n")
			continue
		}

		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			out.WriteString("data: " + payload + "\n\n")
			continue
		}

		keep := chunk["usage"] != nil
		choices, _ := chunk["choices"].([]any)
		for _, entry := range choices {
			choice, _ := entry.(map[string]any)
			if choice == nil {
				continue
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			// Drop the placeholder fields.
			for _, key := range []string{"content", "reasoning_content", "refusal"} {
				if text, ok := delta[key].(string); ok && text == "" {
					delete(delta, key)
				}
			}
			if calls, ok := delta["tool_calls"].([]any); ok && len(calls) == 0 {
				delete(delta, "tool_calls")
			}
			// The role is metadata, not payload: a role-only delta is exactly
			// the blank frame this cleaning exists to remove.
			if sentRole {
				delete(delta, "role")
			}
			hasContent := false
			for _, key := range []string{"content", "reasoning_content", "refusal", "tool_calls"} {
				if value, ok := delta[key]; ok && value != nil {
					hasContent = true
				}
			}
			if hasContent {
				keep = true
				if _, ok := delta["role"]; ok {
					sentRole = true
				}
			}
			if finish, ok := choice["finish_reason"].(string); ok && finish != "" {
				keep = true
			}
		}
		if !keep {
			continue
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			continue
		}
		out.WriteString("data: " + string(encoded) + "\n\n")
	}
	return out.String()
}

func authRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer rp_test")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// ---- models ----

func TestModelsEndpointReportsPoolCatalog(t *testing.T) {
	adapter := newStubProvider()
	srv, server := newTestServer(t, adapter)
	server.pool.MergeModels("acc_test", []string{"glm-5.2", "deepseek-v4-pro"})

	req := authRequest(t, http.MethodGet, srv.URL+"/v1/models", "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := map[string]bool{}
	for _, entry := range payload.Data {
		ids[entry.ID] = true
	}
	if !ids["glm-5.2"] {
		t.Errorf("glm-5.2 missing from the catalogue: %+v", payload.Data)
	}
	if !ids["workbuddy/cn"] {
		t.Errorf("provider alias missing from the catalogue: %+v", payload.Data)
	}
	if !ids["workbuddy/glm-5.2"] || !ids["workbuddy/deepseek-v4-pro"] {
		t.Error("模型目录缺少可明确选渠道的型号名称")
	}
}

// ---- chat completions ----

func TestChatCompletionsNonStream(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)

	body := `{"model":"glm-5.2","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`
	req := authRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	// The serving account must be visible without reading the body.
	if got := resp.Header.Get("X-RelayPool-Account"); got != "acc_test" {
		t.Errorf("X-RelayPool-Account=%q, want acc_test", got)
	}

	var payload struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Object != "chat.completion" {
		t.Errorf("object=%q, want chat.completion", payload.Object)
	}
	if len(payload.Choices) != 1 {
		t.Fatalf("choices=%d, want 1", len(payload.Choices))
	}
	if payload.Choices[0].Message.Content != "hello from the pool" {
		t.Errorf("content=%q", payload.Choices[0].Message.Content)
	}
	if payload.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason=%q, want stop", payload.Choices[0].FinishReason)
	}
	if payload.Usage.TotalTokens != 18 {
		t.Errorf("total_tokens=%d, want 18", payload.Usage.TotalTokens)
	}
}

// The gateway's default is to strip the caller's system prompt, and the
// upstream requires messages[0] to be a system message. The adapter must
// therefore reinsert an empty one, or every request fails with code 11128.
func TestChatCompletionsReinsertsSystemMessage(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)

	// No system message from the client at all.
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	req := authRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
}

func TestChatCompletionsStream(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)

	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := authRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type=%q, want text/event-stream", ct)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	text := string(raw)

	if !strings.Contains(text, "[DONE]") {
		t.Errorf("stream missing the [DONE] terminator: %s", text)
	}
	if !strings.Contains(text, "hello from the pool") {
		t.Errorf("stream missing the reply: %s", text)
	}
	// The placeholder frames must have been dropped, which is the whole point
	// of the sanitizer: an empty content field would render as a blank chunk.
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, `"content":""`) {
			t.Errorf("a blank content field survived into the client stream: %s", line)
		}
	}
}

func TestChatCompletionsFailsOver(t *testing.T) {
	adapter := newStubProvider()
	// Two accounts, both handled by the same stub; the first two upstream
	// calls fail, so the request must reach the second account.
	srv, server := newTestServer(t, adapter)
	addAccount(t, server, "acc_second", adapter.family)
	adapter.failuresBeforeSuccess = 1

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	req := authRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected failover to succeed: status=%d body=%s", resp.StatusCode, raw)
	}
	attempts := resp.Header.Get("X-RelayPool-Attempts")
	if attempts != "2" {
		t.Errorf("X-RelayPool-Attempts=%q, want 2", attempts)
	}
}

// A non-retryable failure must surface as a client error, not be retried.
func TestChatCompletionsInvalidRequestIsNotRetried(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)
	// Make the adapter return a request-shaped rejection.
	adapter.failuresBeforeSuccess = 100
	adapter.failKind = "invalid_request"

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	req := authRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", resp.StatusCode)
	}
	if adapter.calls != 1 {
		t.Errorf("upstream was called %d times; an invalid_request must not be retried", adapter.calls)
	}
}

// ---- validation ----

func TestChatCompletionsValidation(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)

	cases := []struct {
		name string
		body string
	}{
		{"missing model", `{"messages":[{"role":"user","content":"hi"}]}`},
		{"empty messages", `{"model":"glm-5.2","messages":[]}`},
		{"message without role", `{"model":"glm-5.2","messages":[{"content":"hi"}]}`},
		{"not json", `{oops`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := authRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", tc.body)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status=%d, want 400", resp.StatusCode)
			}
			if adapter.calls != 0 {
				t.Errorf("a locally invalid request reached upstream %d times", adapter.calls)
			}
		})
	}
}

func TestChatCompletionsRequiresAuth(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
}

// ---- anthropic ----

func TestMessagesEndpoint(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)

	body := `{"model":"glm-5.2","max_tokens":64,"system":"be helpful","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	req := authRequest(t, http.MethodPost, srv.URL+"/v1/messages", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var payload struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Type != "message" {
		t.Errorf("type=%q, want message", payload.Type)
	}
	if len(payload.Content) == 0 || payload.Content[0].Text != "hello from the pool" {
		t.Errorf("content=%+v", payload.Content)
	}
	// The Anthropic vocabulary uses end_turn, not stop.
	if payload.StopReason != "end_turn" {
		t.Errorf("stop_reason=%q, want end_turn", payload.StopReason)
	}
	if payload.Usage.InputTokens != 11 || payload.Usage.OutputTokens != 7 {
		t.Errorf("usage=%+v, want 11/7", payload.Usage)
	}
}

func TestMessagesStream(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)

	body := `{"model":"glm-5.2","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := authRequest(t, http.MethodPost, srv.URL+"/v1/messages", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)

	// The Anthropic stream uses named events and explicit block bracketing.
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("stream missing %q\n%s", want, text)
		}
	}
	if !strings.Contains(text, "hello from the pool") {
		t.Errorf("stream missing the reply text:\n%s", text)
	}
}

// ---- responses ----

func TestResponsesEndpoint(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)

	body := `{"model":"glm-5.2","instructions":"be terse","input":"hi"}`
	req := authRequest(t, http.MethodPost, srv.URL+"/v1/responses", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var payload struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Object != "response" {
		t.Errorf("object=%q, want response", payload.Object)
	}
	if payload.Status != "completed" {
		t.Errorf("status=%q, want completed", payload.Status)
	}
	if len(payload.Output) == 0 || len(payload.Output[0].Content) == 0 {
		t.Fatalf("output is empty: %+v", payload.Output)
	}
	if payload.Output[0].Content[0].Text != "hello from the pool" {
		t.Errorf("text=%q", payload.Output[0].Content[0].Text)
	}
}

// ---- model prefix ----

func TestModelPrefixPinsProvider(t *testing.T) {
	model, filter := resolveModel("workbuddy/glm-5.2")
	if model != "glm-5.2" {
		t.Errorf("model=%q, want glm-5.2", model)
	}
	if filter != "workbuddy" {
		t.Errorf("filter=%q, want workbuddy", filter)
	}

	model, filter = resolveModel("glm-5.2")
	if model != "glm-5.2" || filter != "" {
		t.Errorf("an unprefixed model must not pin a provider: model=%q filter=%q", model, filter)
	}

	// A slash that is not a known family is part of the model name.
	model, filter = resolveModel("vendor/model-x")
	if model != "vendor/model-x" || filter != "" {
		t.Errorf("an unknown prefix must stay in the model name: model=%q filter=%q", model, filter)
	}
}

// ---- session stickiness ----

func TestSessionStickyAcrossRequests(t *testing.T) {
	adapter := newStubProvider()
	srv, server := newTestServer(t, adapter)
	addAccount(t, server, "acc_second", adapter.family)

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"sticky conversation"}]}`
	var firstAccount string
	for i := 0; i < 4; i++ {
		req := authRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions", body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		account := resp.Header.Get("X-RelayPool-Account")
		if i == 0 {
			firstAccount = account
			continue
		}
		if account != firstAccount {
			t.Fatalf("request %d served by %q, want the sticky %q", i, account, firstAccount)
		}
	}
}

// ---- admin surfaces ----

func TestHealthAndReadinessRemainPublic(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)

	for _, path := range []string{"/healthz", "/readyz", "/version"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status=%d, want 200 (probes must not require auth)", path, resp.StatusCode)
		}
	}
}

func TestReadyDoesNotExposePoolState(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["ok"] != true {
		t.Errorf("ok=false")
	}
	if _, present := payload["accounts_total"]; present {
		t.Error("就绪接口公开账号数量")
	}
	if _, present := payload["accounts_enabled"]; present {
		t.Error("就绪接口公开号池状态")
	}
}

func TestUnknownRouteReturnsJSON(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)

	resp, err := http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type=%q, want JSON (every caller here parses JSON)", ct)
	}
}
