package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

// 并发测试替身记录实际被调用的账号，验证失败不会偷偷换号。
type workspaceProvider struct {
	*stubProvider
	mu    sync.Mutex
	ids   []string
	fail  map[string]bool
	empty bool
	block chan struct{}
}

func (p *workspaceProvider) ChatNonStream(ctx context.Context, c provider.Credential, r *provider.ChatRequest) (*provider.ChatResult, error) {
	p.mu.Lock()
	p.ids = append(p.ids, c.AccountID)
	fail := p.fail[c.AccountID]
	empty := p.empty
	p.mu.Unlock()
	if p.block != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.block:
		}
	}
	if fail {
		return nil, &provider.Failure{Status: 401, KindHint: pool.KindAuth, Message: "凭证错误", Body: `{"message":"认证失败"}`}
	}
	text := `"OK"`
	if empty {
		text = `""`
	}
	return &provider.ChatResult{Model: r.Model, Message: provider.Message{Role: "assistant", Content: json.RawMessage(text)}, FinishReason: "stop"}, nil
}
func (p *workspaceProvider) ChatStream(ctx context.Context, c provider.Credential, r *provider.ChatRequest) (*provider.Stream, error) {
	_, err := p.ChatNonStream(ctx, c, r)
	if err != nil {
		return nil, err
	}
	return &provider.Stream{Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")), Model: r.Model}, nil
}
func workspaceServer(t *testing.T, p *workspaceProvider) (*httptest.Server, *Server) {
	s := newTestServerNoAdapter(t)
	s.registry.Register(p)
	for _, id := range []string{"one", "two"} {
		seedCredential(t, s.store, id, p.family)
		item := poolItem(id, p.family)
		item.Models = []string{"glm-5.2"}
		s.pool.Upsert(item)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(h.Close)
	return h, s
}
func workspacePost(t *testing.T, base, path, body string) *http.Response {
	t.Helper()
	r, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer rp_test")
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
func readWorkspaceResults(t *testing.T, resp *http.Response) []accountTestResult {
	t.Helper()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("状态 %d：%s", resp.StatusCode, body)
	}
	var results []accountTestResult
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type   string            `json:"type"`
			Result accountTestResult `json:"result"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "result" {
			results = append(results, event.Result)
		}
	}
	if !strings.Contains(string(body), `"type":"done"`) {
		t.Fatal("缺少结束事件")
	}
	return results
}
func TestWorkspacePinnedChatNeverSwitchesAccount(t *testing.T) {
	p := &workspaceProvider{stubProvider: newStubProvider(), fail: map[string]bool{"one": true}}
	h, _ := workspaceServer(t, p)
	resp := workspacePost(t, h.URL, "/admin/accounts/one/chat/completions", `{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}],"stream":true}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("状态=%d", resp.StatusCode)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ids) != 1 || p.ids[0] != "one" {
		t.Fatalf("调用了其他账号：%v", p.ids)
	}
}
func TestWorkspaceBatchRecoversButPreservesManualDisable(t *testing.T) {
	p := &workspaceProvider{stubProvider: newStubProvider()}
	h, s := workspaceServer(t, p)
	item, _ := s.pool.ByID("one")
	item.Enabled = false
	item.Ready = new(bool)
	item.DownUntil = time.Now().Add(time.Hour)
	item.LastKind = pool.KindAuth
	s.pool.Upsert(item)
	results := readWorkspaceResults(t, workspacePost(t, h.URL, "/admin/accounts/test", `{}`))
	if len(results) != 2 {
		t.Fatalf("结果数=%d", len(results))
	}
	for _, r := range results {
		if r.Status != "success" || !r.Recovered {
			t.Fatalf("结果=%+v", r)
		}
	}
	current, _ := s.pool.ByID("one")
	if current.Enabled || current.LastKind != "" || current.Ready == nil || !*current.Ready || current.InFlight != 0 {
		t.Fatalf("人工状态或并发数错误：%+v", current)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ids) != 2 {
		t.Fatalf("不是每号一条：%v", p.ids)
	}
}
func TestWorkspaceEmptyResponseDoesNotRecover(t *testing.T) {
	p := &workspaceProvider{stubProvider: newStubProvider(), empty: true}
	h, s := workspaceServer(t, p)
	results := readWorkspaceResults(t, workspacePost(t, h.URL, "/admin/accounts/test", `{"ids":["one"]}`))
	if len(results) != 1 || results[0].Status != "failed" || results[0].Recovered {
		t.Fatalf("空消息被误判：%+v", results)
	}
	item, _ := s.pool.ByID("one")
	if item.Ready != nil && *item.Ready {
		t.Fatal("空消息标记就绪")
	}
}
func TestWorkspaceInferenceKeyCannotTestAccounts(t *testing.T) {
	s := newTestServerNoAdapter(t)
	key, _, err := s.store.CreateAPIKeyWithScope(t.Context(), "调用密钥", store.KeyScopeInference)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/admin/accounts/test", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("调用密钥获得管理功能：%d", w.Code)
	}
}
func TestWorkspaceCancellationReleasesSlotsAndBatchLock(t *testing.T) {
	p := &workspaceProvider{stubProvider: newStubProvider(), block: make(chan struct{})}
	h, s := workspaceServer(t, p)
	ctx, cancel := context.WithCancel(t.Context())
	r, _ := http.NewRequestWithContext(ctx, "POST", h.URL+"/admin/accounts/test", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer rp_test")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		n := len(p.ids)
		p.mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	conflict := workspacePost(t, h.URL, "/admin/accounts/test", `{}`)
	io.Copy(io.Discard, conflict.Body)
	conflict.Body.Close()
	if conflict.StatusCode != 409 {
		t.Fatalf("重复测试未阻止：%d", conflict.StatusCode)
	}
	cancel()
	resp.Body.Close()
	for time.Now().Before(deadline) {
		a, _ := s.pool.ByID("one")
		b, _ := s.pool.ByID("two")
		if a.InFlight == 0 && b.InFlight == 0 && s.accountTestMu.TryLock() {
			s.accountTestMu.Unlock()
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("取消后未释放账号或测试锁")
}
