// Package api wires the HTTP surface: OpenAI-compatible endpoints, an admin
// API, and health probes.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/relaypool/relaypool/internal/armor"
	"github.com/relaypool/relaypool/internal/buildinfo"
	"github.com/relaypool/relaypool/internal/config"
	"github.com/relaypool/relaypool/internal/executor"
	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/compatible"
	"github.com/relaypool/relaypool/internal/provider/native"
	"github.com/relaypool/relaypool/internal/provider/subscriptions"
	"github.com/relaypool/relaypool/internal/provider/workbuddy"
	"github.com/relaypool/relaypool/internal/scheduler"
	"github.com/relaypool/relaypool/internal/store"
)

// Options configures a Server.
type Options struct {
	Config config.Config
	Store  *store.Store
	// APIKey 用于登记初始密钥；与其他密钥统一走数据库验证和撤销。
	APIKey string
}

// Server serves the gateway HTTP API.
type Server struct {
	cfg   config.Config
	store *store.Store
	mux   *http.ServeMux
	// startedAt anchors uptime reporting.
	startedAt time.Time

	// pool, registry, sessions and executor form the request path. They are
	// built by New so the HTTP layer never constructs scheduling state itself.
	pool      *pool.Pool
	registry  *provider.Registry
	executor  *executor.Executor
	sessions  *executor.SessionAffinity
	scheduler *scheduler.Scheduler
	// armor holds the optional system-prompt presets. It is nil when none are
	// configured, and the request path skips the whole branch in that case.
	armor            *armor.Registry
	armorMu          sync.Mutex
	armorCustom      armorCustomization
	armorConfigError string
	researchMu       sync.Mutex
	researchCache    map[string][]armor.ResearchFile

	// loginMu guards logins, the in-progress browser logins. loginCancel stops
	// the sweeper that reaps abandoned ones.
	loginMu     sync.Mutex
	logins      map[string]pendingLogin
	loginCancel context.CancelFunc

	// stateWriter coalesces pool runtime state into asynchronous database writes.
	stateWriter *stateWriter
	// 管理修改和整池重载串行，避免旧数据库快照覆盖刚生效的配置。
	accountMu sync.Mutex
	// 同一服务一次只运行一轮批量测试，避免重复点击消耗上游额度。
	accountTestMu sync.Mutex
	loginStartMu  sync.Mutex
	inspectionMu  sync.Mutex
	quotaCache    map[string]*quotaCacheEntry

	// 流式响应只限制单次阻塞写入，测试可缩短期限复核慢速连接。
	streamWriteTimeout time.Duration
}

// Armor exposes the preset registry.
func (s *Server) Armor() *armor.Registry { return s.armor }

// Scheduler exposes the background job runner.
func (s *Server) Scheduler() *scheduler.Scheduler { return s.scheduler }

// New builds a Server with every route registered.
//
// The pool is seeded from the store here, so a restart resumes with the
// accounts the operator configured instead of an empty pool.
func New(opts Options) *Server {
	registry := provider.NewRegistry()
	nativeConfig := native.Config{Timeout: opts.Config.Upstream.Timeout, HeaderTimeout: opts.Config.Upstream.HeaderTimeout, IdleTimeout: opts.Config.Upstream.IdleTimeout, ProxyURL: opts.Config.Upstream.ProxyURL}
	registry.Register(native.NewClaude(nativeConfig))
	registry.Register(native.NewGemini(nativeConfig))
	registry.Register(native.NewOpenAI(nativeConfig))
	registry.Register(native.NewCodex(nativeConfig))
	subscriptionConfig := subscriptions.Config{Timeout: opts.Config.Upstream.Timeout, HeaderTimeout: opts.Config.Upstream.HeaderTimeout, IdleTimeout: opts.Config.Upstream.IdleTimeout, ProxyURL: opts.Config.Upstream.ProxyURL}
	registry.Register(subscriptions.NewQoder(subscriptionConfig))
	registry.Register(subscriptions.NewTrae(subscriptionConfig))
	registry.Register(subscriptions.NewTraeIntl(subscriptionConfig))
	compatibleConfig := compatible.Config{Timeout: opts.Config.Upstream.Timeout, HeaderTimeout: opts.Config.Upstream.HeaderTimeout, IdleTimeout: opts.Config.Upstream.IdleTimeout, ProxyURL: opts.Config.Upstream.ProxyURL}
	for _, profile := range compatible.Profiles() {
		registry.Register(compatible.NewFor(profile.ID, profile.DefaultBaseURL, compatibleConfig))
	}
	registry.Register(compatible.New(compatibleConfig))
	registry.Register(workbuddy.New(workbuddy.Config{
		Timeout:       opts.Config.Upstream.Timeout,
		HeaderTimeout: opts.Config.Upstream.HeaderTimeout,
		IdleTimeout:   opts.Config.Upstream.IdleTimeout,
		ProxyURL:      opts.Config.Upstream.ProxyURL,
		UserAgent:     opts.Config.Upstream.UserAgent,
	}))

	accountPool := pool.New(opts.Config.Routing)
	accountPool.SetBreakerCooldown(opts.Config.Pool.BreakerCooldown, opts.Config.Pool.BreakerCooldownMax)
	accountPool.SetExpiryPreference(opts.Config.Pool.PreferExpiring, opts.Config.Pool.ExpiringSoon)
	// 旧记录只迁移一次；此后破甲状态完全由密钥权限控制。
	if opts.Store != nil {
		legacy := readLegacyArmorEnabled(opts.Store, opts.Config.Armor.Enabled)
		if err := opts.Store.InitializeAPIKeyPermissions(context.Background(), legacy); err != nil {
			log.Printf("初始化密钥权限失败：%v", err)
		}
	}
	// 有数据库时初始密钥也走持久化鉴权，撤销后不能从内存旁路通过。
	if opts.Store != nil && strings.TrimSpace(opts.APIKey) != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _, err := opts.Store.EnsureBootstrapAPIKey(ctx, opts.APIKey)
		cancel()
		if err != nil {
			log.Printf("api: 注册初始密钥失败：%v", err)
		}
	}

	sessionTTL := opts.Config.Session.TTL
	sessionCapacity := opts.Config.Session.Capacity
	if !opts.Config.Session.Enabled {
		// A tiny TTL makes every binding expire on the next lookup, which
		// disables stickiness without a second code path in the hot loop.
		sessionTTL = time.Nanosecond
		sessionCapacity = 1
	}
	sessions := executor.NewSessionAffinity(sessionTTL, sessionCapacity)

	s := &Server{
		cfg:       opts.Config,
		store:     opts.Store,
		mux:       http.NewServeMux(),
		startedAt: time.Now(),
		pool:      accountPool,
		registry:  registry,
		sessions:  sessions,
	}

	s.executor = executor.New(accountPool, registry, sessions, credentialSource{
		store:       opts.Store,
		registry:    registry,
		OnRefreshed: s.pool.ResetCredentialHealth,
	}, executor.Options{
		MaxAttempts:          defaultAttempts(opts.Config),
		DefaultCooldown:      opts.Config.Pool.DefaultCooldown,
		MaxRetryAfter:        opts.Config.Pool.MaxRetryAfter,
		MinRateLimitCooldown: opts.Config.Pool.MinRateLimitCooldown,
		BreakerThreshold:     opts.Config.Pool.BreakerThreshold,
	})

	// Persist account health so the console shows live state and a restart does
	// not resurrect a cooldown that already expired.
	//
	// The writer coalesces and writes asynchronously: the observer runs on the
	// request path, and a synchronous SQLite write there would add a database
	// round trip to the latency of every completion.
	s.stateWriter = newStateWriter(opts.Store)
	s.stateWriter.start()
	accountPool.SetObserver(s.stateWriter.observe)

	s.reloadAccounts()
	s.wireScheduler()
	s.loadArmor()
	// Abandoned browser logins must not accumulate forever: an operator who
	// opens the dialog and closes the tab would otherwise leave both a map entry
	// and a disabled account row behind. Expiry is checked here as well as on
	// poll, because polling is exactly what an abandoned login does not do.
	loginCtx, loginCancel := context.WithCancel(context.Background())
	s.loginCancel = loginCancel
	s.startLoginSweeper(loginCtx)
	s.routes()
	return s
}

// loadArmor loads the system-prompt presets from the configured directory.
//
// A missing directory or a load failure is not fatal: armor is an optional
// feature, and refusing to start the gateway because a preset file is
// unreadable would take the whole pool offline for a cosmetic problem.
func (s *Server) loadArmor() {
	dir := strings.TrimSpace(s.cfg.Armor.Dir)
	if dir == "" {
		s.armor = armor.NewRegistry()
		s.loadArmorCustomization()
		return
	}
	registry := armor.NewRegistry()
	loaded, err := registry.LoadDir(dir)
	if err != nil {
		log.Printf("api: load armor presets from %s: %v", dir, err)
	}
	if loaded == 0 && dir == "prompts" {
		loaded, err = registry.LoadBundledPresets()
		if err != nil {
			log.Printf("api: 内置预设加载失败：%v", err)
		}
	}
	if loaded == 0 {
		// An empty registry means the request path short-circuits, so a
		// deployment without presets costs nothing.
		log.Printf("api: no armor presets loaded (dir=%s)", dir)
	} else {
		if def := strings.TrimSpace(s.cfg.Armor.Default); def != "" {
			if err := registry.SetDefault(def); err != nil {
				log.Printf("api: armor default %q: %v", def, err)
			}
		}
		log.Printf("api: armor loaded %d preset(s): %s (default=%s)",
			loaded, strings.Join(registry.Names(), ", "), registry.Default())
	}
	s.armor = registry
	s.loadArmorCustomization()
}

// wireScheduler builds the background job runner.
//
// The scheduler is wired through callbacks rather than given the pool directly,
// so the dependency runs one way: api depends on scheduler, and scheduler knows
// nothing about the pool or the HTTP layer.
func (s *Server) wireScheduler() {
	accounts := scheduler.StoreAccounts{
		OnCredentialRefresh: s.pool.ResetCredentialHealth,
		Store:               s.store,
		// The pool is the live view of enablement, so a disabled account stops
		// being scheduled immediately.
		States: func() []scheduler.AccountState {
			items := s.pool.Items()
			out := make([]scheduler.AccountState, 0, len(items))
			for _, item := range items {
				out = append(out, scheduler.AccountState{
					ID:          item.ID,
					Enabled:     item.Enabled,
					AutoCheckin: item.AutoCheckin,
				})
			}
			return out
		},
		// A fetched catalogue goes straight into the pool, which merges it with
		// the proven-model list so a snapshot omission cannot strand a model.
		OnCatalog: func(accountID string, models []provider.ModelInfo) {
			names := make([]string, 0, len(models))
			for _, model := range models {
				names = append(names, model.NativeModel)
			}
			s.pool.MergeModels(accountID, names)
		},
	}

	s.scheduler = scheduler.New(s.registry, accounts, credentialSource{
		store:    s.store,
		registry: s.registry,
	}, scheduler.FromConfig(s.cfg))

	// The sweep callback is how the scheduler clears expired cooldowns without
	// importing the pool package.
	s.scheduler.SetSweeper(func(now time.Time) []string {
		return s.pool.SweepExpired(now)
	})
	if s.store != nil {
		s.scheduler.SetLogPruner(s.store.PruneRequests)
	}
}

// 单请求换号预算与每个账号的并发容量分别配置。
func defaultAttempts(cfg config.Config) int {
	attempts := cfg.Pool.MaxAttempts
	if attempts < 1 {
		attempts = executor.DefaultMaxAttempts
	}
	if attempts > 8 {
		attempts = 8
	}
	return attempts
}

// reloadAccounts seeds the pool from the store.
func (s *Server) reloadAccounts() {
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	if s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		log.Printf("api: load accounts into the pool: %v", err)
		return
	}
	items := make([]pool.Item, 0, len(accounts))
	for _, account := range accounts {
		items = append(items, s.accountPoolItem(account))
	}
	s.pool.SyncFromStore(items)
	log.Printf("api: pool loaded with %d accounts", len(items))
}

// persistAccountState writes runtime health back to the store synchronously.
//
// It is retained for the shutdown path and tests; the request path uses the
// coalescing stateWriter instead.
func (s *Server) persistAccountState(item pool.Item) {
	if s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.store.UpdatePoolRuntime(ctx, item.ID, runtimeFromItem(item)); err != nil {
		log.Printf("api: persist runtime state for %s: %v", item.ID, err)
	}
}

// Close flushes and stops the background writers.
//
// It exists so a clean shutdown persists the final cooldown state: a restart
// immediately after would otherwise let a cooling account be retried.
func (s *Server) Close() {
	// Stop the login sweeper first: it touches the store and the pool, so it
	// must not run while those are being torn down.
	if s.loginCancel != nil {
		s.loginCancel()
	}
	if s.scheduler != nil {
		s.scheduler.Stop()
	}
	if s.stateWriter != nil {
		s.stateWriter.stop()
	}
}

// poolItemFromAccount converts a stored account into a pool entry.
func poolItemFromAccount(account store.Account) pool.Item {
	return pool.Item{
		ID:               account.ID,
		Name:             account.Name,
		Provider:         account.Provider,
		Region:           account.Region,
		Enabled:          account.Enabled,
		Priority:         account.Priority,
		Weight:           account.Weight,
		MaxInflight:      account.MaxInflight,
		DropSystemPrompt: account.DropSystemPrompt,
		AutoCheckin:      account.AutoCheckin,
		DownUntil:        account.CooldownUntil,
		LastError:        account.LastError,
		LastKind:         account.LastErrorKind,
		QuotaScope:       account.QuotaScope,
		QuotaGroup:       account.QuotaGroup,
		QuotaExpiresAt:   account.QuotaExpiresAt,
		GroupDownUntil:   account.Runtime.GroupDownUntil,
		BackoffLevel:     account.Runtime.BackoffLevel,
		ModelDownUntil:   account.Runtime.ModelDownUntil,
		ModelBackoff:     account.Runtime.ModelBackoff,
		ModelLastKind:    account.Runtime.ModelLastKind,
		ProvenModels:     account.Runtime.ProvenModels,
	}
}

// 账号零值继承全局上限；存储值保持零，之后调整全局配置仍能生效。
func (s *Server) accountPoolItem(account store.Account) pool.Item {
	item := poolItemFromAccount(account)
	if item.MaxInflight == 0 {
		item.MaxInflight = s.cfg.Pool.MaxInflight
	}
	// 主动填写的型号应立即可供客户端选择，不必等待下一次远端目录刷新。
	if s.store != nil && account.Provider != "workbuddy" {
		ctx, cancel := context.WithTimeout(context.Background(), credentialLoadTimeout)
		defer cancel()
		if record, err := s.store.GetCredential(ctx, account.ID); err == nil {
			if adapter, err := s.registry.Lookup(account.Provider); err == nil {
				if cred, err := adapter.DecodeCredential(record.Payload); err == nil {
					var models []string
					if json.Unmarshal([]byte(cred.Field("models")), &models) == nil && len(models) > 0 {
						item.Models = models
						item.ModelsAt = time.Now()
					}
				}
			}
		}
	}
	return item
}

// Pool exposes the scheduler for the admin API and tests.
func (s *Server) Pool() *pool.Pool { return s.pool }

// Registry exposes the provider registry.
func (s *Server) Registry() *provider.Registry { return s.registry }

// Sessions exposes the session affinity table.
func (s *Server) Sessions() *executor.SessionAffinity { return s.sessions }

// Handler returns the fully wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	// Order matters: recover outermost so a panic anywhere still returns a
	// well-formed error, then request id, then logging, then auth inside the
	// individual routes (health must stay unauthenticated).
	return s.securityHeadersMiddleware(s.recoverMiddleware(s.requestIDMiddleware(s.logMiddleware(s.mux))))
}

func (s *Server) routes() {
	s.mux.Handle("GET /admin/providers", s.auth(http.HandlerFunc(s.handlePlatforms)))
	// Probes: deliberately unauthenticated so a container healthcheck and a
	// reverse proxy can reach them without holding a credential.
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /readyz", s.handleReady)
	s.mux.HandleFunc("GET /version", s.handleVersion)

	// OpenAI-compatible surface. Registered as a prefix so nested paths
	// (e.g. /v1/chat/completions) all land here.
	s.mux.Handle("GET /v1/key/permissions", s.auth(http.HandlerFunc(s.handleCurrentKeyPermissions)))
	s.mux.Handle("PUT /v1/key/armor", s.auth(http.HandlerFunc(s.handleKeyArmor)))
	s.mux.Handle("GET /v1/models", s.auth(http.HandlerFunc(s.handleModels)))
	s.mux.Handle("POST /v1/chat/completions", s.auth(http.HandlerFunc(s.handleChatCompletions)))
	s.mux.Handle("POST /v1/messages", s.auth(http.HandlerFunc(s.handleMessages)))
	s.mux.Handle("POST /v1/responses", s.auth(http.HandlerFunc(s.handleResponses)))

	// Admin surface.
	s.mux.Handle("GET /admin/accounts", s.auth(http.HandlerFunc(s.handleListAccounts)))
	s.mux.Handle("POST /admin/accounts/test", s.auth(http.HandlerFunc(s.handleTestAccounts)))
	s.mux.Handle("POST /admin/accounts/{id}/chat/completions", s.auth(http.HandlerFunc(s.handleAccountChat)))
	s.mux.Handle("POST /admin/accounts", s.auth(http.HandlerFunc(s.handleCreateAccount)))
	s.mux.Handle("GET /admin/accounts/{id}", s.auth(http.HandlerFunc(s.handleGetAccount)))
	s.mux.Handle("GET /admin/accounts/{id}/inspect/quota", s.auth(http.HandlerFunc(s.handleAccountQuota)))
	s.mux.Handle("GET /admin/accounts/{id}/inspect/catalog", s.auth(http.HandlerFunc(s.handleAccountCatalog)))
	s.mux.Handle("PATCH /admin/accounts/{id}", s.auth(http.HandlerFunc(s.handlePatchAccount)))
	s.mux.Handle("DELETE /admin/accounts/{id}", s.auth(http.HandlerFunc(s.handleDeleteAccount)))
	s.mux.Handle("POST /admin/accounts/{id}/enabled", s.auth(http.HandlerFunc(s.handleSetAccountEnabled)))
	s.mux.Handle("PUT /admin/accounts/{id}/credential", s.auth(http.HandlerFunc(s.handlePutCredential)))

	// Adding an account by signing in through a browser, rather than pasting a
	// token copied out of developer tools.
	//
	// The start route creates the account row (disabled) and returns a URL to
	// open; the status route is polled until the credential arrives, at which
	// point the account is enabled and becomes routable.
	s.mux.Handle("POST /admin/accounts/login/start", s.auth(http.HandlerFunc(s.handleStartAccountLogin)))
	s.mux.Handle("GET /admin/accounts/login/{id}", s.auth(http.HandlerFunc(s.handleAccountLoginStatus)))
	s.mux.Handle("POST /admin/accounts/login/{id}/cancel", s.auth(http.HandlerFunc(s.handleCancelAccountLogin)))
	s.mux.Handle("POST /admin/accounts/login/{id}/complete", s.auth(http.HandlerFunc(s.handleCompleteAccountLogin)))
	s.mux.Handle("POST /admin/accounts/{id}/checkin", s.auth(http.HandlerFunc(s.handleAccountCheckin)))
	s.mux.Handle("GET /admin/checkin/schedule", s.auth(http.HandlerFunc(s.handleCheckinSchedule)))
	s.mux.HandleFunc("GET /auth/callback", s.handleBrowserCallback)
	s.mux.HandleFunc("GET /callback", s.handleBrowserCallback)
	s.mux.HandleFunc("GET /authorize", s.handleBrowserCallback)
	s.mux.HandleFunc("OPTIONS /authorize", s.handleBrowserCallbackPreflight)

	// Armor presets: optional system-prompt substitutions.
	s.mux.Handle("GET /admin/armor", s.auth(http.HandlerFunc(s.handleListArmorPresets)))
	s.mux.Handle("GET /admin/armor/sources", s.auth(http.HandlerFunc(s.handleResearchSources)))
	s.mux.Handle("GET /admin/armor/sources/{source}", s.auth(http.HandlerFunc(s.handleResearchFiles)))
	s.mux.Handle("GET /admin/armor/sources/{source}/preview", s.auth(http.HandlerFunc(s.handleResearchPreview)))
	s.mux.Handle("PUT /admin/armor/bindings", s.auth(http.HandlerFunc(s.handleArmorBinding)))
	s.mux.Handle("GET /admin/armor/pack", s.auth(http.HandlerFunc(s.handleExportArmorPack)))
	s.mux.Handle("POST /admin/armor/pack/preview", s.auth(http.HandlerFunc(s.handleArmorPack)))
	s.mux.Handle("PUT /admin/armor/pack", s.auth(http.HandlerFunc(s.handleArmorPack)))
	s.mux.Handle("PUT /admin/armor/{name}", s.auth(http.HandlerFunc(s.handleImportArmor)))
	s.mux.Handle("DELETE /admin/armor/{name}", s.auth(http.HandlerFunc(s.handleDeleteArmor)))
	s.mux.Handle("GET /admin/armor/resolve", s.auth(http.HandlerFunc(s.handleArmorResolve)))
	// 保留旧路径的明确拒绝，避免 switch 被动态预设路由误当成预设名称。
	s.mux.Handle("GET /admin/armor/switch", s.auth(http.HandlerFunc(s.handleNotFound)))
	s.mux.Handle("PUT /admin/armor/switch", s.auth(http.HandlerFunc(s.handleNotFound)))
	s.mux.Handle("GET /admin/armor/{name}", s.auth(http.HandlerFunc(s.handleGetArmorPreset)))

	// Exposure information, so a client can tell which caution level applies.
	s.mux.Handle("GET /admin/access", s.auth(http.HandlerFunc(s.handleAccessInfo)))

	s.mux.Handle("GET /admin/keys/options", s.auth(http.HandlerFunc(s.handleKeyOptions)))
	s.mux.Handle("PATCH /admin/keys/{id}", s.auth(http.HandlerFunc(s.handlePatchKey)))
	s.mux.Handle("PUT /admin/keys/{id}/armor", s.auth(http.HandlerFunc(s.handleKeyArmor)))
	s.mux.Handle("GET /admin/keys", s.auth(http.HandlerFunc(s.handleListKeys)))
	s.mux.Handle("POST /admin/keys", s.auth(http.HandlerFunc(s.handleCreateKey)))
	s.mux.Handle("DELETE /admin/keys/{id}", s.auth(http.HandlerFunc(s.handleDeleteKey)))

	s.mux.Handle("GET /admin/requests", s.auth(http.HandlerFunc(s.handleListRequests)))
	s.mux.Handle("GET /admin/requests/{id}/attempts", s.auth(http.HandlerFunc(s.handleListAttempts)))

	// Aggregated overview statistics: requests, success rate, latency
	// percentiles, token usage, per-model and per-provider breakdowns.
	s.mux.Handle("GET /admin/stats", s.auth(http.HandlerFunc(s.handleStats)))

	// The bundled console. Unauthenticated on purpose: it is a static shell
	// that collects the key in-browser; every admin call it makes is still
	// authenticated. See console.go.
	s.mountConsole()

	// Anything else is a JSON 404 rather than net/http's HTML page, because
	// every caller here is a program parsing JSON.
	s.mux.HandleFunc("/", s.handleNotFound)
}

// ---- middleware ----

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
)

func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 外部编号不能作为日志主键，避免重复编号覆盖历史记录和日志注入。
		id := newRequestID()
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

// statusRecorder captures the response code for the access log without
// buffering the body, so streaming responses stay streaming.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

// 将连接控制操作传给真实响应体，批量检查的写入期限才会生效。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying writer when it supports flushing, which is
// required for SSE. Without this, wrapping the ResponseWriter would silently
// break streaming.
func (r *statusRecorder) Flush() {
	_ = r.FlushError()
}

// 包装器必须继续传递刷新失败，否则小响应的阻塞写入会被误记为成功。
func (r *statusRecorder) FlushError() error {
	return http.NewResponseController(r.ResponseWriter).Flush()
}

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health probes fire every few seconds; logging them would bury
		// everything useful.
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		log.Printf("%s %q -> %d (%d bytes, %s) id=%s",
			r.Method, r.URL.Path, rec.status, rec.bytes,
			time.Since(started).Round(time.Millisecond), requestIDFrom(r.Context()))
	})
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// 不记录异常的原始值，其中可能包含账号凭证或请求正文。
				log.Printf("panic serving %s %q (%T)", r.Method, r.URL.Path, rec)
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// auth enforces API key authentication.
//
// 所有密钥通过数据库中的有效哈希验证，初始密钥没有额外通道。
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		// 所有访问方式统一使用请求头，密钥不得进入浏览器历史或代理日志。
		if key == "" || len(key) > 1024 {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "Missing/invalid API key")
			return
		}
		if s.store == nil {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "鉴权存储不可用")
			return
		}
		record, err := s.store.AuthenticateAPIKey(r.Context(), key)
		if err != nil {
			log.Printf("api key verification failed: %v", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "api key verification failed")
			return
		}
		if record == nil {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "Missing/invalid API key")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/admin/") && record.Scope != store.KeyScopeAdmin {
			writeError(w, http.StatusForbidden, "insufficient_scope", "调用密钥不能访问管理接口，请使用管理密钥")
			return
		}
		ctx := r.Context()
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, keyPermissionsContextKey{}, *record)))
	})
}

func bearerToken(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	if raw == "" {
		// Anthropic clients use x-api-key instead of a bearer token.
		return strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	const prefix = "Bearer "
	if len(raw) > len(prefix) && strings.EqualFold(raw[:len(prefix)], prefix) {
		return strings.TrimSpace(raw[len(prefix):])
	}
	return raw
}

// ---- health ----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "relaypool",
		"version": buildinfo.Version,
		"commit":  buildinfo.Commit,
		"uptime":  time.Since(s.startedAt).Round(time.Second).String(),
	})
}

// handleReady gates on the database being reachable, which is the one hard
// dependency of this service. Accounts are deliberately NOT part of readiness:
// a gateway with zero usable accounts should still be reportable as up so the
// console can be used to fix it.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	err := s.store.Ping(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": "store unavailable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "relaypool",
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":  buildinfo.Version,
		"commit":   buildinfo.Commit,
		"built_at": buildinfo.BuiltAt,
		"go":       buildinfo.GoVersion(),
	})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no route for %s %s", r.Method, r.URL.Path))
}

// ---- helpers ----

func requestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

func newRequestID() string {
	if id, err := store.NewRequestID(); err == nil {
		return id
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	// Keep the payload readable during manual testing.
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

// errorBody matches the OpenAI error envelope so existing SDKs parse it
// without special casing.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{
		Message: message,
		Type:    "api_error",
		Code:    code,
	}})
}

// decodeJSONBody reads a JSON request body with a size cap.
//
// The cap matters: this endpoint is reachable by any local process holding the
// key, and an unbounded body would let one bad request exhaust memory.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = 32 << 20 // 32 MiB
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// 拒绝尾随内容，避免校验和处理对同一请求产生不同解释。
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("请求只能包含一个完整 JSON 值")
	}
	return nil
}

// clientIP reports the caller address, used only for logging.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
