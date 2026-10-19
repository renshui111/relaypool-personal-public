package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/config"
	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/store"
)

// 并发数和单请求的换号预算是两种独立限制。
func TestReviewAttemptsIndependentOfConcurrency(t *testing.T) {
	cfg := config.Default()
	for _, concurrency := range []int{0, 1, 32} {
		cfg.Pool.MaxInflight = concurrency
		if got := defaultAttempts(cfg); got != 4 {
			t.Errorf("并发配置=%d，默认换号预算=%d，期望=4", concurrency, got)
		}
	}
}

func TestReviewRevokedBootstrapKeyStaysRevokedAfterRestart(t *testing.T) {
	s := newTestServerNoAdapter(t)
	replacement, replacementRecord, err := s.store.CreateAPIKey(t.Context(), "替代管理密钥")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := s.store.ListAPIKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var bootstrapID string
	for _, key := range keys {
		if key.Name == "bootstrap" {
			bootstrapID = key.ID
		}
	}
	if bootstrapID == "" {
		t.Fatal("没有初始密钥记录")
	}
	if err := s.store.DeleteAPIKey(t.Context(), bootstrapID); err != nil {
		t.Fatal(err)
	}
	check := func(server *Server) {
		t.Helper()
		old := httptest.NewRecorder()
		server.Handler().ServeHTTP(old, authRequest(t, http.MethodGet, "/admin/accounts", ""))
		if old.Code != 401 {
			t.Fatalf("撤销的初始密钥仍获准访问：%d", old.Code)
		}
		req := authRequest(t, http.MethodGet, "/admin/accounts", "")
		req.Header.Set("Authorization", "Bearer "+replacement)
		valid := httptest.NewRecorder()
		server.Handler().ServeHTTP(valid, req)
		if valid.Code != 200 {
			t.Fatal("替代密钥不能访问管理接口")
		}
	}
	check(s)
	s.Close()
	restarted := New(Options{Config: testConfig(t), Store: s.store, APIKey: "rp_test"})
	defer restarted.Close()
	check(restarted)
	if err := s.store.DeleteAPIKey(t.Context(), replacementRecord.ID); err == nil {
		t.Fatal("最后一个可用密钥被撤销，导致无法管理号池")
	}
}

func TestReviewPatchUpdatesSchedulingPreservingCooldown(t *testing.T) {
	s := newTestServerNoAdapter(t)
	a, err := s.store.CreateAccount(t.Context(), accountFixture("config"))
	if err != nil {
		t.Fatal(err)
	}
	s.pool.Upsert(s.accountPoolItem(a))
	s.pool.MarkClassified(a.ID, pool.Classified{Kind: pool.KindRateLimit, Model: "m", Cooldown: time.Hour}, 3)
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, authRequest(t, http.MethodPatch, "/admin/accounts/config", `{"max_inflight":1,"priority":100,"weight":75,"quota_scope":"model"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("配置修改失败：%s", response.Body.String())
	}
	live, _ := s.pool.ByID(a.ID)
	if live.MaxInflight != 1 || live.Priority != 100 || live.Weight != 75 || live.QuotaScope != "model" || live.ModelDownUntil["m"].IsZero() {
		t.Fatalf("配置或冷却不一致：%+v", live)
	}
	stored, err := s.store.GetAccount(t.Context(), a.ID)
	if err != nil || stored.MaxInflight != 1 || stored.QuotaScope != "model" {
		t.Fatal("配置未持久化")
	}
	for _, body := range []string{`{"max_inflight":-1}`, `{"weight":0}`, `{"quota_scope":"invalid"}`} {
		invalid := httptest.NewRecorder()
		s.Handler().ServeHTTP(invalid, authRequest(t, http.MethodPatch, "/admin/accounts/config", body))
		if invalid.Code != http.StatusBadRequest {
			t.Fatalf("无效配置被接受：%s", body)
		}
	}
}

func TestReviewCredentialRegionMismatchLeavesStoreUnchanged(t *testing.T) {
	s := newTestServerNoAdapter(t)
	a, err := s.store.CreateAccount(t.Context(), accountFixture("region"))
	if err != nil {
		t.Fatal(err)
	}
	s.pool.Upsert(s.accountPoolItem(a))
	if err := s.store.UpsertCredential(t.Context(), store.Credential{AccountID: a.ID, Format: "test", Payload: []byte(`{"access_token":"test-old","domain":"codebuddy.cn"}`)}); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, authRequest(t, http.MethodPut, "/admin/accounts/region/credential", `{"payload":{"access_token":"test-global","domain":"www.workbuddy.ai"}}`))
	if response.Code != http.StatusBadRequest {
		t.Fatal("地区不匹配凭证被接受")
	}
	stored, _ := s.store.GetCredential(t.Context(), a.ID)
	if string(stored.Payload) != `{"access_token":"test-old","domain":"codebuddy.cn"}` {
		t.Fatal("错误导入覆盖了原凭证")
	}
}

func TestReviewBadCreationCredentialDoesNotLeaveAccount(t *testing.T) {
	s := newTestServerNoAdapter(t)
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, authRequest(t, http.MethodPost, "/admin/accounts", `{"credential":"not-json"}`))
	if response.Code != 400 {
		t.Fatalf("无效导入状态码=%d", response.Code)
	}
	accounts, err := s.store.ListAccounts(t.Context())
	if err != nil || len(accounts) != 0 || s.pool.Len() != 0 {
		t.Fatal("导入失败留下账号")
	}
}

func TestReviewStateWriterRetriesAndOrdersRecreatedAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retry.db")
	st, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAccount(t.Context(), accountFixture("retry")); err != nil {
		t.Fatal(err)
	}
	w := newStateWriter(st)
	w.observe(pool.Item{ID: "retry", RuntimeEpoch: 1, StateVersion: 20, LastError: "旧实例"})
	st.Close()
	w.flush(t.Context())
	if len(w.pending) != 1 {
		t.Fatal("临时写入失败丢弃了健康快照")
	}
	st, err = store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w.store = st
	w.observe(pool.Item{ID: "retry", RuntimeEpoch: 2, StateVersion: 1, LastError: "新实例"})
	w.observe(pool.Item{ID: "retry", RuntimeEpoch: 1, StateVersion: 30, LastError: "迟到旧实例"})
	w.flush(t.Context())
	a, err := st.GetAccount(t.Context(), "retry")
	if err != nil || a.LastError != "新实例" {
		t.Fatal("新实例状态未保存或被旧实例覆盖")
	}
	w.forget("retry")
	if len(w.pending) != 0 || len(w.versions) != 0 || len(w.epochs) != 0 {
		t.Fatal("删除后残留状态索引")
	}
}

func TestReviewHTTPIndependentQuotaFailoverAndRestart(t *testing.T) {
	adapter := newStubProvider()
	adapter.failuresBeforeSuccess = 1
	adapter.failKind = pool.KindQuota
	httpServer, s := newTestServer(t, adapter)
	addAccount(t, s, "acc_second", adapter.family)
	for _, id := range []string{"acc_test", "acc_second"} {
		item, _ := s.pool.ByID(id)
		item.QuotaScope = "account"
		s.pool.Upsert(item)
	}
	response, err := http.DefaultClient.Do(authRequest(t, http.MethodPost, httpServer.URL+"/v1/chat/completions", `{"model":"glm-5.2","messages":[{"role":"user","content":"验证号池"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || payload["object"] != "chat.completion" {
		t.Fatalf("完整 HTTP 链路失败：%+v", payload)
	}
	if adapter.calls != 2 {
		t.Fatal("额度耗尽未切换到第二个账号")
	}
	s.Close()
	restarted := New(Options{Config: testConfig(t), Store: s.store})
	defer restarted.Close()
	served := response.Header.Get("X-RelayPool-Account")
	a, ok := restarted.pool.ByID(served)
	if !ok || len(a.ProvenModels) == 0 {
		t.Fatal("成功验证的模型未跨重启保存")
	}
}

// 未设置账号覆盖值时，实际名额应使用全局配置。
func TestReviewAccountInheritsConcurrency(t *testing.T) {
	st := openTestStore(t)
	defer st.Close()
	a, err := st.CreateAccount(t.Context(), store.Account{ID: "inherit", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Pool.MaxInflight = 1
	s := New(Options{Config: cfg, Store: st})
	defer s.Close()
	if !s.pool.Acquire(a.ID) {
		t.Fatal("首次占位失败")
	}
	defer s.pool.Release(a.ID)
	if s.pool.Acquire(a.ID) {
		s.pool.Release(a.ID)
		t.Fatal("默认并发配置未生效，第二次占位应被拒绝")
	}
}

// 验证真实数据库和服务重建，而不仅是内存快照。
func TestReviewModelHealthSurvivesRestart(t *testing.T) {
	st := openTestStore(t)
	defer st.Close()
	if _, err := st.CreateAccount(t.Context(), accountFixture("restart")); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Config: testConfig(t), Store: st})
	s.pool.MarkClassified("restart", pool.Classified{Kind: pool.KindRateLimit, Model: "m", Cooldown: time.Hour}, 3)
	s.Close()
	restarted := New(Options{Config: testConfig(t), Store: st})
	defer restarted.Close()
	if restarted.pool.PickRoute(pool.RouteQuery{PublicModel: "m"}).OK {
		t.Fatal("重启后模型冷却丢失，过早重试上游")
	}
	if !restarted.pool.PickRoute(pool.RouteQuery{PublicModel: "other"}).OK {
		t.Fatal("模型冷却错误地阻塞了其他模型")
	}
}
