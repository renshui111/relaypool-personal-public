package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
)

// Options tunes the executor.
type Options struct {
	// MaxAttempts caps how many accounts one client request may try. Without
	// a cap, a request against a fully broken pool would fan out into one
	// upstream call per account.
	MaxAttempts int
	// DefaultCooldown is used when a failure carries no hint.
	DefaultCooldown time.Duration
	// MaxRetryAfter clamps an upstream Retry-After.
	MaxRetryAfter time.Duration
	// MinRateLimitCooldown is the floor for a rate-limit cooldown.
	MinRateLimitCooldown time.Duration
	// BreakerThreshold is how many consecutive same-kind failures trip the
	// backoff ladder.
	BreakerThreshold int
}

// DefaultMaxAttempts bounds the failover fan-out.
const DefaultMaxAttempts = 4

// CredentialSource resolves an account ID to its ready credential.
//
// It is an interface rather than a store handle so the executor does not depend
// on the persistence layer, and so tests can supply credentials directly.
type CredentialSource interface {
	CredentialFor(accountID string) (provider.Credential, error)
}

// Executor runs requests against the pool.
type Executor struct {
	pool        *pool.Pool
	registry    *provider.Registry
	sessions    *SessionAffinity
	credentials CredentialSource
	opts        Options
	// observers receive per-attempt records. They must not block: they are
	// called on the request path.
	observers []AttemptObserver
}

// AttemptObserver records one account attempt. Implementations should be
// non-blocking; the store-backed one writes asynchronously.
type AttemptObserver func(AttemptRecord)

// AttemptRecord describes one attempt at one account.
type AttemptRecord struct {
	RequestID    string
	AttemptIndex int
	AccountID    string
	Provider     string
	Status       string
	Success      bool
	ErrorKind    string
	ErrorCode    string
	ErrorMessage string
	Latency      time.Duration
	StartedAt    time.Time
	FinishedAt   time.Time
}

// New creates an executor.
func New(p *pool.Pool, registry *provider.Registry, sessions *SessionAffinity, credentials CredentialSource, opts Options) *Executor {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	if opts.DefaultCooldown <= 0 {
		opts.DefaultCooldown = 15 * time.Second
	}
	if opts.MaxRetryAfter <= 0 {
		opts.MaxRetryAfter = pool.DefaultMaxRetryAfter
	}
	if opts.MinRateLimitCooldown <= 0 {
		opts.MinRateLimitCooldown = 30 * time.Second
	}
	if opts.BreakerThreshold < 1 {
		opts.BreakerThreshold = 3
	}
	return &Executor{
		pool:        p,
		registry:    registry,
		sessions:    sessions,
		credentials: credentials,
		opts:        opts,
	}
}

// OnAttempt registers an attempt observer.
func (e *Executor) OnAttempt(observer AttemptObserver) {
	if observer == nil {
		return
	}
	e.observers = append(e.observers, observer)
}

func (e *Executor) notify(record AttemptRecord) {
	for _, observer := range e.observers {
		observer(record)
	}
}

// Request is one client completion request.
type Request struct {
	// 适配器已被调用但返回错误时通知入口；凭证读取或选号失败不通知。
	OnUpstreamFailure func(error)
	// ID correlates the request with its attempt records.
	ID string
	// Chat is the provider-neutral request.
	Chat *provider.ChatRequest
	// PreserveSystemPrompt 仅由网关在已注入预设后设置，避免账号清理开关删掉网关预设。
	PreserveSystemPrompt bool
	// SessionKey pins the conversation to one account. Empty disables
	// stickiness for this request.
	SessionKey string
	// PreferAccount pins a specific account, overriding the session binding.
	PreferAccount string
	// OnlyAccount 由管理端设置，严格固定账号；失败时禁止换号。
	OnlyAccount string
	// RecoveryProbe 仅用于管理端发起的非流式短消息检查，允许检查冷却和停用账号。
	RecoveryProbe bool
	// ProviderFilter restricts routing to one channel family.
	ProviderFilter string
	// RegionFilter restricts routing to one region.
	RegionFilter string
	// AllowedProviders, when non-empty, restricts the eligible families.
	AllowedProviders []string
	// AllowedAccounts 为 nil 表示不限；明确空列表禁止所有账号。
	AllowedAccounts []string
	// Stream requests a streaming response.
	Stream bool
}

// Result is the outcome of a successful request.
type Result struct {
	// 流式回复在完整读完后才确认健康，保存原请求的版本避免覆盖较新状态。
	AttemptItem  pool.Item
	Chat         *provider.ChatResult
	Stream       *provider.Stream
	AccountID    string
	Provider     string
	Model        string
	AttemptCount int
	SessionHit   bool
}

// FailureError reports that every attempt failed.
//
// It carries the last classified failure so the HTTP layer can map it onto a
// status code and an OpenAI-shaped error body rather than a generic 500.
type FailureError struct {
	// Status is the HTTP status to report.
	Status int
	// Kind is the classified failure kind.
	Kind string
	// Code and Message are the client-facing error details.
	Code    string
	Message string
	// RetryAfter is set when no account is available but one will recover.
	RetryAfter time.Duration
	// Attempts is how many accounts were tried.
	Attempts int
	// LastAccountID is the final account tried, for diagnostics.
	LastAccountID string
	// LastProvider is the family of the final account tried.
	//
	// It exists so the request log can record WHICH CHANNEL failed: the
	// console offers a provider filter, and without this field a failed
	// request is logged with an empty provider and therefore cannot be found
	// by filtering on the channel that produced it -- which is exactly the
	// query an operator runs while debugging.
	LastProvider string
	// Cause is the underlying error.
	Cause error
}

func (e *FailureError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "all accounts failed"
}

func (e *FailureError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// routeLoop tracks one request's progress through the pool.
type routeLoop struct {
	pool     *pool.Pool
	request  Request
	excluded map[string]struct{}
	index    int
	attempts int

	// observers is copied from the Executor so the loop can emit attempt
	// records without holding a back-reference.
	observers []AttemptObserver

	lastErr       *FailureError
	lastAccountID string
	lastProvider  string
	// regionFilter is pinned once an account is picked, so the retry stays in
	// the region the first attempt established. Letting it float would move
	// the conversation between upstream deployments mid-request.
	regionFilter string
}

// execute runs the failover loop, calling attempt for each candidate account.
//
// The attempt callback returns the classified failure so the loop can decide
// whether to continue. Keeping the loop generic over the attempt body is what
// lets streaming and non-streaming share one failover policy.
//
// # Stream ownership
//
// The attempt callback must not open a stream it then abandons. Its contract is:
//
//   - on success it returns a Result whose Stream is OPEN, and the caller owns it
//   - on failure it returns a nil Result, having closed anything it opened
//
// The loop enforces the second half defensively: a Result returned alongside an
// error is closed before the loop continues, so a callback that leaks a stream
// on its failure path cannot leak it here too.
func (e *Executor) execute(
	ctx context.Context,
	req Request,
	attempt func(ctx context.Context, item pool.Item, p provider.Provider, index int) (*Result, *pool.Classified, error),
) (*Result, error) {
	loop := e.newRouteLoop(req)

	for loop.index < loop.attempts {
		// A client that has gone away must not consume more upstream quota.
		if err := ctx.Err(); err != nil {
			return nil, loop.contextError(err)
		}

		item, err := loop.pickNext()
		if err != nil {
			return nil, loop.pickFailure(err)
		}

		p, err := e.registry.Lookup(item.Provider)
		if err != nil {
			e.pool.ReleaseItem(item)
			// A missing adapter is a configuration problem, not a transient
			// account failure. Record it and move on rather than failing the
			// whole request on one misconfigured row.
			loop.recordFailure(item, loop.index, pool.Classified{
				Kind:     pool.KindUnavailable,
				Message:  fmt.Sprintf("no adapter registered for provider %q", item.Provider),
				Failover: true,
			}, time.Now(), time.Now())
			loop.lastErr = &FailureError{
				Status: 500, Kind: pool.KindUnavailable,
				Code:    "provider_unavailable",
				Message: fmt.Sprintf("no adapter registered for provider %q", item.Provider),
				Cause:   err,
			}
			loop.exclude(item.ID)
			continue
		}

		started := time.Now()
		// Capture the attempt number BEFORE the call: pickNext has already
		// incremented index to point at the next attempt, so index is the
		// 1-based count of the attempt about to run.
		attemptNumber := loop.index
		result, classified, attemptErr := e.runAdmittedAttempt(ctx, item, p, attemptNumber, attempt)

		if attemptErr == nil && result != nil {
			if result.Stream == nil {
				e.pool.MarkAttemptOK(item, modelOf(req))
			}
			result.AttemptItem = item
			if e.sessions != nil && req.SessionKey != "" {
				e.sessions.Bind(req.SessionKey, item.ID)
			}
			result.AccountID = item.ID
			result.Provider = item.Provider
			result.AttemptCount = attemptNumber
			loop.recordSuccess(item, attemptNumber, started, time.Now())
			return result, nil
		}

		// A callback that returned both an error and a Result opened a stream
		// and then failed. Close it here rather than trusting every caller's
		// failure path to be tidy: a leaked stream holds an upstream connection
		// and a sanitizer goroutine until the process exits.
		if result != nil {
			if result.Stream != nil {
				_ = result.Stream.Close()
			}
			if result.Chat != nil {
				// A buffered result holds no resources; nothing to release.
				_ = result.Chat
			}
		}

		finished := time.Now()

		// A cancelled client context is not an account failure: do not cool
		// the account down for it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			loop.recordFailure(item, attemptNumber, pool.Classified{Kind: pool.KindUnavailable, Message: ctxErr.Error()},
				started, finished)
			loop.lastErr = &FailureError{
				Status: 499, Kind: pool.KindUnavailable,
				Code:    "client_closed_request",
				Message: "the client closed the request",
				Cause:   ctxErr,
			}
			loop.lastAccountID = item.ID
			loop.lastProvider = item.Provider
			return nil, loop.lastErr
		}

		if classified == nil {
			// Defensive: an attempt that fails must always classify. Treat an
			// unclassified failure as retryable rather than silently stopping.
			classified = &pool.Classified{
				Kind: pool.KindUnavailable, Failover: true,
				Cooldown: e.opts.DefaultCooldown,
				Message:  errText(attemptErr),
			}
		}

		e.pool.MarkAttemptClassified(item, *classified, e.opts.BreakerThreshold)
		if current, ok := e.pool.ByID(item.ID); ok && current.RuntimeEpoch == item.RuntimeEpoch && current.CredentialRevision == item.CredentialRevision {
			// 错误响应也应报告实际退避时间，包括达到熔断阈值后的延长。
			classified.RetryAfter = e.pool.RetryAfter(current, modelOf(req))
		}
		loop.recordFailure(item, attemptNumber, *classified, started, finished)

		loop.lastErr = loop.failureFrom(item, classified, attemptErr)
		loop.lastAccountID = item.ID
		loop.lastProvider = item.Provider

		// A stale catalog is the usual cause of "model not available", so ask
		// the adapter to drop its cache. This is best-effort: a failure to
		// invalidate only costs one more wasted attempt, and the adapter's own
		// TTL will expire the entry anyway.
		if classified.Kind == pool.KindModelNotAvailable {
			e.invalidateCatalog(item.ID)
		}

		loop.exclude(item.ID)

		if !classified.Failover {
			// The classifier says retrying another account cannot help (an
			// invalid request, or an exhausted shared allowance). Stopping
			// here is the whole point of the distinction: retrying would burn
			// another account's quota for nothing.
			return nil, loop.lastErr
		}

		// Escaping a session binding is worth recording, because a rising
		// escape count is the signal that accounts are unstable.
		if e.sessions != nil && req.SessionKey != "" {
			e.sessions.RecordEscape(classified.Kind)
		}
	}

	if loop.lastErr != nil {
		loop.lastErr.Attempts = loop.index
		loop.lastErr.LastAccountID = loop.lastAccountID
		return nil, loop.lastErr
	}
	return nil, &FailureError{
		Status: 503, Kind: pool.KindUnavailable,
		Code:     "no_available_account",
		Message:  "no usable account is available for this model",
		Attempts: loop.index,
	}
}

// newRouteLoop builds the loop state for a request.
func (e *Executor) newRouteLoop(req Request) *routeLoop {
	attempts := e.opts.MaxAttempts
	if req.OnlyAccount != "" {
		attempts = 1
	}
	if e.pool != nil {
		// Size the budget to the pool but never exceed the configured cap: a
		// single account should not be retried MaxAttempts times.
		route := pool.RouteQuery{
			PublicModel:      modelOf(req),
			ProviderFilter:   req.ProviderFilter,
			RegionFilter:     req.RegionFilter,
			AllowedProviders: req.AllowedProviders,
			AllowedAccounts:  req.AllowedAccounts,
		}
		if size := e.pool.LenRoute(route); size > 0 && size < attempts {
			attempts = size
		}
	}
	if attempts < 1 {
		attempts = 1
	}

	loop := &routeLoop{
		pool:         e.pool,
		request:      req,
		excluded:     map[string]struct{}{},
		attempts:     attempts,
		regionFilter: req.RegionFilter,
		observers:    e.observers,
	}

	// Resolve the session binding up front so the first pick is already
	// sticky. A pinned account from the client wins over the binding.
	if req.PreferAccount == "" && req.SessionKey != "" && e.sessions != nil {
		if accountID, ok := e.sessions.Get(req.SessionKey); ok {
			loop.request.PreferAccount = accountID
		}
	}
	return loop
}

// pickNext selects the next account.
func (l *routeLoop) pickNext() (pool.Item, error) {
	if l.request.OnlyAccount != "" {
		if l.request.AllowedAccounts != nil {
			allowed := false
			for _, id := range l.request.AllowedAccounts {
				if id == l.request.OnlyAccount {
					allowed = true
					break
				}
			}
			if !allowed {
				return pool.Item{}, &noAccountError{reason: pool.ReasonNoAccounts}
			}
		}
		item, ok := l.pool.AcquireTarget(l.request.OnlyAccount, modelOf(l.request), l.request.RecoveryProbe)
		if !ok {
			return pool.Item{}, &noAccountError{reason: pool.ReasonAllSaturated}
		}
		l.index++
		return item, nil
	}
	// Excluded accounts are passed to the pool rather than filtered here, so
	// the pool's rotation cursor advances over the real candidate set.
	result := l.pool.AcquireRoute(pool.RouteQuery{
		PublicModel:      modelOf(l.request),
		PreferAccount:    l.request.PreferAccount,
		ProviderFilter:   l.request.ProviderFilter,
		RegionFilter:     l.regionFilter,
		AllowedProviders: l.request.AllowedProviders,
		AllowedAccounts:  l.request.AllowedAccounts,
		Excluded:         l.excluded,
	})
	if result.OK {
		// Pin the region from the first successful pick so a retry cannot move
		// the conversation to the other upstream deployment.
		if l.regionFilter == "" {
			l.regionFilter = result.Item.Region
		}
		// Only the first attempt honours the pin; later attempts must be free
		// to pick any account.
		l.request.PreferAccount = ""
		l.index++
		return result.Item, nil
	}

	return pool.Item{}, &noAccountError{
		reason:  result.Reason,
		retryAt: result.RetryAt,
	}
}

// noAccountError reports that the pool had nothing usable.
type noAccountError struct {
	reason  string
	retryAt time.Time
}

func (e *noAccountError) Error() string {
	switch e.reason {
	case pool.ReasonAllSaturated:
		return "all accounts are at their concurrency limit"
	case pool.ReasonAllCooling:
		return "all accounts are cooling down"
	default:
		return "no accounts are configured for this model"
	}
}

// pickFailure converts a pick failure into a client-facing error.
//
// The saturated case is deliberately distinguished from the cooling case: a
// saturated pool needs load shedding, while a cooling pool needs a retry-after.
// Reporting one as the other tells the caller to do the wrong thing.
func (l *routeLoop) pickFailure(err error) error {
	// A real attempt already failed, so that error is more informative than
	// "nothing left to try".
	if l.lastErr != nil {
		l.lastErr.Attempts = l.index
		l.lastErr.LastAccountID = l.lastAccountID
		l.lastErr.LastProvider = l.lastProvider
		return l.lastErr
	}

	var noAccount *noAccountError
	if errors.As(err, &noAccount) {
		switch noAccount.reason {
		case pool.ReasonAllSaturated:
			return &FailureError{
				Status: 429, Kind: pool.KindRateLimit,
				Code:    "pool_saturated",
				Message: "every account is at its concurrency limit; retry shortly",
				Cause:   err,
			}
		case pool.ReasonAllCooling:
			failure := &FailureError{
				Status: 429, Kind: pool.KindRateLimit,
				Code:    "all_accounts_cooling",
				Message: "every account for this model is cooling down",
				Cause:   err,
			}
			if !noAccount.retryAt.IsZero() {
				failure.RetryAfter = time.Until(noAccount.retryAt)
			}
			return failure
		}
	}
	return &FailureError{
		Status: 503, Kind: pool.KindUnavailable,
		Code:    "no_available_account",
		Message: err.Error(),
		Cause:   err,
	}
}

// contextError reports a client cancellation.
func (l *routeLoop) contextError(err error) error {
	if l.lastErr != nil {
		l.lastErr.Attempts = l.index
		l.lastErr.LastAccountID = l.lastAccountID
		l.lastErr.LastProvider = l.lastProvider
		return l.lastErr
	}
	return &FailureError{
		Status: 499, Kind: pool.KindUnavailable,
		Code:    "client_closed_request",
		Message: "the client closed the request",
		Cause:   err,
	}
}

// failureFrom builds a FailureError from a classified failure.
func (l *routeLoop) failureFrom(item pool.Item, classified *pool.Classified, cause error) *FailureError {
	status := classified.Status
	if status < 400 {
		status = 502
	}
	message := classified.Message
	if strings.TrimSpace(message) == "" {
		message = errText(cause)
	}
	if strings.TrimSpace(message) == "" {
		message = "the upstream request failed"
	}
	return &FailureError{
		Status:       status,
		Kind:         classified.Kind,
		Code:         classified.Code,
		Message:      message,
		LastProvider: item.Provider,
		RetryAfter:   classified.RetryAfter,
		Cause:        cause,
	}
}

// exclude adds an account to this request's exclusion set.
func (l *routeLoop) exclude(accountID string) {
	l.excluded[accountID] = struct{}{}
}

// recordSuccess notifies observers of a successful attempt.
func (l *routeLoop) recordSuccess(item pool.Item, attemptNumber int, started, finished time.Time) {
	l.emit(AttemptRecord{
		RequestID:    l.request.ID,
		AttemptIndex: attemptNumber - 1,
		AccountID:    item.ID,
		Provider:     item.Provider,
		Status:       "ok",
		Success:      true,
		Latency:      finished.Sub(started),
		StartedAt:    started,
		FinishedAt:   finished,
	})
}

// recordFailure notifies observers of a failed attempt.
func (l *routeLoop) recordFailure(item pool.Item, attemptNumber int, classified pool.Classified, started, finished time.Time) {
	l.emit(AttemptRecord{
		RequestID:    l.request.ID,
		AttemptIndex: attemptNumber - 1,
		AccountID:    item.ID,
		Provider:     item.Provider,
		Status:       "error",
		ErrorKind:    classified.Kind,
		ErrorCode:    classified.Code,
		ErrorMessage: classified.Message,
		Latency:      finished.Sub(started),
		StartedAt:    started,
		FinishedAt:   finished,
	})
}

// emit delivers an attempt record to the loop's observers.
//
// The observer list lives on the Executor, so the loop carries a reference
// rather than reaching back through a pointer that may be nil in tests.
func (l *routeLoop) emit(record AttemptRecord) {
	if l.observers == nil {
		return
	}
	for _, observer := range l.observers {
		observer(record)
	}
}

// catalogInvalidator is implemented by adapters that cache a catalogue and can
// drop it. It is an optional interface so adapters without a cache need not
// implement it.
type catalogInvalidator interface {
	InvalidateCatalog(accountID string)
}

// invalidateCatalog asks the account's adapter to drop a cached catalogue.
//
// It is a no-op for adapters that do not cache, which is why the interface is
// optional rather than part of the main Provider contract.
func (e *Executor) invalidateCatalog(accountID string) {
	if e.pool == nil {
		return
	}
	item, ok := e.pool.ByID(accountID)
	if !ok {
		return
	}
	p, err := e.registry.Lookup(item.Provider)
	if err != nil {
		return
	}
	if inv, ok := p.(catalogInvalidator); ok {
		inv.InvalidateCatalog(accountID)
	}
}

// modelOf returns the request's model.
func modelOf(req Request) string {
	if req.Chat == nil {
		return ""
	}
	return req.Chat.Model
}

// errText renders an error for a log or message without a nil-pointer risk.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
