// Package scheduler runs the background jobs that keep a pool healthy:
// catalogue refresh, daily check-in, and token keep-alive.
//
// Design note on the timing model: jobs are declared as "run at these local
// hours" rather than as fixed intervals. Check-in is a DAILY action with a
// reset at local midnight, so a periodic timer would drift and eventually fire
// twice in one day or zero times in another. Hour-of-day scheduling against the
// local clock is what makes "once per day" actually mean once per day.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/relaypool/relaypool/internal/config"
	"github.com/relaypool/relaypool/internal/provider"
)

// CredentialSource resolves an account to its credential.
type CredentialSource interface {
	CredentialFor(accountID string) (provider.Credential, error)
}

// AccountRef is the minimum an account needs to expose for scheduling.
type AccountRef struct {
	ID       string
	Name     string
	Provider string
	Region   string
	Enabled  bool
	// AutoCheckin opts an account into the daily check-in job.
	AutoCheckin bool
}

// AccountSource lists the accounts to schedule.
type AccountSource interface {
	// SchedulableAccounts returns every account the scheduler should consider.
	SchedulableAccounts() []AccountRef
	// RecordCheckin persists a check-in outcome.
	RecordCheckin(ctx context.Context, accountID, status, message string) error
	// LastCheckinAt reports when an account last checked in successfully.
	LastCheckinAt(ctx context.Context, accountID string) (time.Time, error)
	// MergeCatalog installs a freshly fetched catalogue for an account.
	MergeCatalog(accountID string, models []provider.ModelInfo)
}

// Options tunes the scheduler.
type Options struct {
	// CatalogRefresh is how often each account's catalogue is refreshed.
	CatalogRefresh time.Duration
	// CheckinEnabled turns the daily check-in job on or off.
	CheckinEnabled bool
	// CheckinHours are the local hours at which to attempt check-in.
	CheckinHours []int
	// KeepaliveEnabled turns the token keep-alive job on or off.
	KeepaliveEnabled bool
	// KeepaliveHours are the local hours at which to keep tokens warm.
	KeepaliveHours []int
	// CooldownGC is how often expired cooldowns are swept.
	CooldownGC time.Duration
	// 日志按配置保留；零关闭清理，不改变用户当天的独立额度。
	RetentionDays int
	// Now is a test seam for the clock.
	Now func() time.Time
}

// FromConfig builds options from the loaded configuration.
func FromConfig(cfg config.Config) Options {
	return Options{
		CatalogRefresh:   cfg.Schedule.CatalogRefresh,
		CheckinEnabled:   cfg.Schedule.CheckinEnabled,
		CheckinHours:     cfg.Schedule.CheckinHours,
		KeepaliveEnabled: cfg.Schedule.KeepaliveEnabled,
		KeepaliveHours:   cfg.Schedule.KeepaliveHours,
		CooldownGC:       cfg.Schedule.CooldownGC,
		RetentionDays:    cfg.Logging.RetentionDays,
	}
}

// Scheduler owns the background jobs.
type Scheduler struct {
	registry *provider.Registry
	accounts AccountSource
	creds    CredentialSource
	opts     Options

	// sweep is the callback that clears expired cooldowns. It is injected so
	// the scheduler does not depend on the pool package directly.
	sweep     func(now time.Time) []string
	pruneLogs func(context.Context, time.Duration) (int64, error)

	// now is the clock, overridable in tests.
	now func() time.Time
	// lastRun tracks the last hour each hour-triggered job fired, so a job
	// does not re-fire on every tick within its hour.
	lastRun map[string]time.Time

	mu        sync.Mutex
	started   bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	checkinMu sync.Mutex
	checking  map[string]bool
}

// New creates a scheduler.
func New(registry *provider.Registry, accounts AccountSource, creds CredentialSource, opts Options) *Scheduler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	// A zero interval would make the ticker config invalid and spin the loop.
	if opts.CatalogRefresh <= 0 {
		opts.CatalogRefresh = 30 * time.Minute
	}
	if opts.CooldownGC <= 0 {
		opts.CooldownGC = 5 * time.Minute
	}
	return &Scheduler{
		registry: registry,
		accounts: accounts,
		creds:    creds,
		opts:     opts,
		now:      opts.Now,
		lastRun:  map[string]time.Time{},
	}
}

// SetSweeper installs the cooldown sweep callback.
func (s *Scheduler) SetSweeper(sweep func(now time.Time) []string) {
	s.sweep = sweep
}

// 清理回调在启动前安装，调度器统一管理其取消与收尾。
func (s *Scheduler) SetLogPruner(prune func(context.Context, time.Duration) (int64, error)) {
	s.pruneLogs = prune
}

// Start launches the background loops.
//
// It is idempotent: a second call is ignored, so a caller cannot accidentally
// start two sets of jobs against the same pool.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go s.catalogLoop(loopCtx)

	s.wg.Add(1)
	go s.cooldownLoop(loopCtx)
	if s.opts.RetentionDays > 0 && s.pruneLogs != nil {
		s.wg.Add(1)
		go s.logPruneLoop(loopCtx)
	}

	if s.opts.CheckinEnabled {
		s.wg.Add(1)
		go s.hourlyLoop(loopCtx, "checkin", s.opts.CheckinHours, s.runCheckin)
	}
	if s.opts.KeepaliveEnabled {
		s.wg.Add(1)
		go s.hourlyLoop(loopCtx, "keepalive", s.opts.KeepaliveHours, s.runKeepalive)
	}

	log.Printf("scheduler: started (catalog %s, checkin %v, keepalive %v)",
		s.opts.CatalogRefresh, s.opts.CheckinHours, s.opts.KeepaliveHours)
}

// 启动后清理一次，随后每小时清理；停止服务时不会遗留写盘任务。
func (s *Scheduler) logPruneLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		jobCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		count, err := s.pruneLogs(jobCtx, time.Duration(s.opts.RetentionDays)*24*time.Hour)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("scheduler: 清理过期日志失败：%v", err)
		}
		if count > 0 {
			log.Printf("scheduler: 已清理 %d 条过期请求日志", count)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Stop halts the background loops and waits for them to finish.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.started = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
	log.Printf("scheduler: stopped")
}

// catalogLoop refreshes account catalogues on a fixed interval.
//
// A fixed interval is correct here (unlike check-in) because a catalogue is not
// tied to a daily reset: it just needs to be reasonably fresh, and a drift of a
// few minutes is harmless.
func (s *Scheduler) catalogLoop(ctx context.Context) {
	defer s.wg.Done()

	// Refresh once shortly after start so a restart does not leave the
	// catalogue unknown for a whole interval.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.refreshCatalogs(ctx)
			timer.Reset(s.opts.CatalogRefresh)
		}
	}
}

// refreshCatalogs fetches every account's catalogue.
func (s *Scheduler) refreshCatalogs(ctx context.Context) {
	for _, account := range s.accounts.SchedulableAccounts() {
		if !account.Enabled {
			continue
		}
		if err := ctx.Err(); err != nil {
			return
		}
		s.refreshOneCatalog(ctx, account)
	}
}

// refreshOneCatalog fetches one account's catalogue.
//
// A failure is logged and swallowed: an unknown catalogue fails open in the
// scheduler, so a catalogue outage must never take a working account out of
// rotation.
func (s *Scheduler) refreshOneCatalog(ctx context.Context, account AccountRef) {
	adapter, err := s.registry.Lookup(account.Provider)
	if err != nil {
		log.Printf("scheduler: catalog: %s: %v", account.ID, err)
		return
	}
	cred, err := s.creds.CredentialFor(account.ID)
	if err != nil {
		// A missing credential is expected for an account that has been added
		// but not yet logged in, so this is not worth a warning on every tick.
		return
	}
	models, err := adapter.FetchCatalog(ctx, cred)
	if err != nil {
		log.Printf("scheduler: catalog for %s (%s): %v", account.ID, account.Provider, err)
		return
	}
	s.accounts.MergeCatalog(account.ID, models)
}

// cooldownLoop sweeps expired cooldowns.
//
// The sweep exists so a long-running process does not accumulate per-model
// cooldown entries forever, and so the console stops showing a deadline that
// has already passed.
func (s *Scheduler) cooldownLoop(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(s.opts.CooldownGC)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.sweep == nil {
				continue
			}
			if resumed := s.sweep(s.now()); len(resumed) > 0 {
				log.Printf("scheduler: %d account(s) resumed after cooldown", len(resumed))
			}
		}
	}
}

// hourlyLoop runs a job at specific local hours.
//
// The dedupe via lastRun is what makes "run at hour 9" mean once per hour rather
// than once per tick. Without it, a fast tick interval would fire the job many
// times within the same hour.
func (s *Scheduler) hourlyLoop(ctx context.Context, name string, hours []int, job func(context.Context)) {
	defer s.wg.Done()

	if len(hours) == 0 {
		return
	}
	allowed := map[int]bool{}
	for _, hour := range hours {
		allowed[hour] = true
	}

	// Tick once a minute: fine enough that a job fires within the right hour,
	// coarse enough to be free.
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := s.now()
			if !allowed[now.Hour()] {
				continue
			}
			if s.alreadyRanThisHour(name, now) {
				continue
			}
			job(ctx)
		}
	}
}

// alreadyRanThisHour reports whether a job has fired in the current hour, and
// records the run when it has not.
func (s *Scheduler) alreadyRanThisHour(name string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if previous, ok := s.lastRun[name]; ok {
		// Same calendar day and same hour means it already ran.
		if previous.Year() == now.Year() && previous.YearDay() == now.YearDay() &&
			previous.Hour() == now.Hour() {
			return true
		}
	}
	s.lastRun[name] = now
	return false
}

// runCheckin performs the daily check-in for every opted-in account.
func (s *Scheduler) runCheckin(ctx context.Context) {
	for _, account := range s.accounts.SchedulableAccounts() {
		if !account.Enabled || !account.AutoCheckin {
			continue
		}
		if err := ctx.Err(); err != nil {
			return
		}
		s.checkinOne(ctx, account)
	}
}

// checkinOne performs one account's check-in.
func (s *Scheduler) checkinOne(ctx context.Context, account AccountRef) {
	_, _ = s.Checkin(ctx, account, false)
}

// Checkin 供定时任务和手动领取共用；同一账号只允许一个领取请求。
func (s *Scheduler) Checkin(ctx context.Context, account AccountRef, manual bool) (provider.CheckinResult, error) {
	capability := provider.CheckinFor(account.Provider, account.Region)
	if !capability.Supported {
		return provider.CheckinResult{}, provider.ErrUnsupported
	}
	s.checkinMu.Lock()
	if s.checking == nil {
		s.checking = map[string]bool{}
	}
	if s.checking[account.ID] {
		s.checkinMu.Unlock()
		return provider.CheckinResult{}, fmt.Errorf("该账号正在签到，请稍后查看结果")
	}
	s.checking[account.ID] = true
	s.checkinMu.Unlock()
	defer func() { s.checkinMu.Lock(); delete(s.checking, account.ID); s.checkinMu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if last, err := s.accounts.LastCheckinAt(ctx, account.ID); err == nil && !last.IsZero() {
		if !manual && provider.CheckinPeriod(last, capability) == provider.CheckinPeriod(s.now(), capability) {
			return provider.CheckinResult{Status: provider.CheckinAlready, Message: "本领取周期已完成"}, nil
		}
	}

	adapter, err := s.registry.Lookup(account.Provider)
	if err != nil {
		return provider.CheckinResult{}, err
	}
	cred, err := s.creds.CredentialFor(account.ID)
	if err != nil {
		return provider.CheckinResult{}, err
	}

	result, err := adapter.Checkin(ctx, cred)
	if err != nil {
		if errorsIs(err, provider.ErrUnsupported) {
			// A channel without a check-in must not be retried every day.
			return provider.CheckinResult{}, err
		}
		log.Printf("scheduler: check-in for %s failed", account.ID)
		_ = s.accounts.RecordCheckin(ctx, account.ID, provider.CheckinError, "签到失败，请检查登录态、资格或设备信息")
		return provider.CheckinResult{}, err
	}
	if err := s.accounts.RecordCheckin(ctx, account.ID, result.Status, result.Message); err != nil {
		log.Printf("scheduler: record check-in for %s: %v", account.ID, err)
		return provider.CheckinResult{}, err
	}
	if result.Status == provider.CheckinSuccess {
		log.Printf("scheduler: check-in succeeded for %s", account.ID)
	}
	return result, nil
}

// runKeepalive refreshes tokens so the account stays warm.
//
// The purpose is to avoid a cold-start failure: an access token that has been
// idle for a long time can be rejected on first use, and refreshing ahead of
// demand keeps the request path fast and predictable.
func (s *Scheduler) runKeepalive(ctx context.Context) {
	for _, account := range s.accounts.SchedulableAccounts() {
		if !account.Enabled {
			continue
		}
		if err := ctx.Err(); err != nil {
			return
		}
		s.keepaliveOne(ctx, account)
	}
}

func (s *Scheduler) keepaliveOne(ctx context.Context, account AccountRef) {
	adapter, err := s.registry.Lookup(account.Provider)
	if err != nil {
		return
	}
	cred, err := s.creds.CredentialFor(account.ID)
	if err != nil {
		return
	}
	// Skip when the token is comfortably valid: refreshing a fresh token
	// churns the credential for no benefit and risks a rotation race with an
	// in-flight request.
	if !cred.ExpiresAt.IsZero() && cred.ExpiresAt.Sub(s.now()) > 6*time.Hour {
		return
	}
	refreshed, err := adapter.Refresh(ctx, cred)
	if err != nil {
		if !errorsIs(err, provider.ErrUnsupported) {
			log.Printf("scheduler: keepalive for %s failed: %v", account.ID, err)
		}
		return
	}
	if saver, ok := s.accounts.(CredentialSaver); ok {
		if err := saver.SaveRefreshedCredential(ctx, account.ID, refreshed); err != nil {
			log.Printf("scheduler: persist refreshed credential for %s: %v", account.ID, err)
		}
	}
}

// CredentialSaver is implemented by a source that can persist a refreshed
// credential. It is an optional interface so a read-only source still works.
type CredentialSaver interface {
	SaveRefreshedCredential(ctx context.Context, accountID string, cred provider.Credential) error
}

// NormalizeHours sorts and deduplicates an hour list.
func NormalizeHours(hours []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(hours))
	for _, hour := range hours {
		if hour < 0 || hour > 23 || seen[hour] {
			continue
		}
		seen[hour] = true
		out = append(out, hour)
	}
	sort.Ints(out)
	return out
}

// sameLocalDay reports whether two instants fall on the same local day.
func sameLocalDay(a, b time.Time) bool {
	ay, am, ad := a.Local().Date()
	by, bm, bd := b.Local().Date()
	return ay == by && am == bm && ad == bd
}

// errorsIs is a local alias for errors.Is so this file's call sites read the
// same way as the rest of the package.
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// errText renders an error for storage without a nil-pointer risk.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
