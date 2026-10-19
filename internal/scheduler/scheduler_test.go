package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

// ---- test doubles ----

// fakeProvider is a scripted adapter.
type fakeProvider struct {
	family string

	mu           sync.Mutex
	catalogCalls int
	checkinCalls int
	refreshCalls int

	catalogErr  error
	checkinErr  error
	refreshErr  error
	checkinResp provider.CheckinResult
	// unsupportedCheckin makes Checkin/Refresh return ErrUnsupported, which is
	// how a channel without those operations behaves.
	unsupportedCheckin bool
	unsupportedRefresh bool
	// catalog is what FetchCatalog returns.
	catalog []provider.ModelInfo
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		family:      "workbuddy",
		checkinResp: provider.CheckinResult{Status: provider.CheckinSuccess, Message: "OK"},
		catalog:     []provider.ModelInfo{{NativeModel: "glm-5.2"}},
	}
}

func (f *fakeProvider) Family() string      { return f.family }
func (f *fakeProvider) Kind() provider.Kind { return provider.KindInProcess }

func (f *fakeProvider) DecodeCredential(raw []byte) (provider.Credential, error) {
	return provider.Credential{Raw: raw}, nil
}

func (f *fakeProvider) FetchCatalog(context.Context, provider.Credential) ([]provider.ModelInfo, error) {
	f.mu.Lock()
	f.catalogCalls++
	err := f.catalogErr
	catalog := f.catalog
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return catalog, nil
}

// ChatNonStream and ChatStream exist only to satisfy the Provider interface.
// The scheduler never calls them, so they report ErrUnsupported rather than
// pretending to have behaviour the tests do not exercise.
func (f *fakeProvider) ChatNonStream(context.Context, provider.Credential, *provider.ChatRequest) (*provider.ChatResult, error) {
	return nil, provider.ErrUnsupported
}

func (f *fakeProvider) ChatStream(context.Context, provider.Credential, *provider.ChatRequest) (*provider.Stream, error) {
	return nil, provider.ErrUnsupported
}

func (f *fakeProvider) Checkin(context.Context, provider.Credential) (provider.CheckinResult, error) {
	f.mu.Lock()
	f.checkinCalls++
	unsupported := f.unsupportedCheckin
	err := f.checkinErr
	resp := f.checkinResp
	f.mu.Unlock()
	if unsupported {
		return provider.CheckinResult{}, provider.ErrUnsupported
	}
	if err != nil {
		return provider.CheckinResult{}, err
	}
	return resp, nil
}

func (f *fakeProvider) Refresh(context.Context, provider.Credential) (provider.Credential, error) {
	f.mu.Lock()
	f.refreshCalls++
	unsupported := f.unsupportedRefresh
	err := f.refreshErr
	f.mu.Unlock()
	if unsupported {
		return provider.Credential{}, provider.ErrUnsupported
	}
	if err != nil {
		return provider.Credential{}, err
	}
	return provider.Credential{
		Provider: f.family,
		Raw:      []byte(`{"access_token":"refreshed"}`),
		Fields:   map[string]string{"access_token": "refreshed"},
	}, nil
}

func (f *fakeProvider) counts() (catalog, checkin, refresh int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.catalogCalls, f.checkinCalls, f.refreshCalls
}

// fakeAccounts is a scripted account source.
type fakeAccounts struct {
	mu       sync.Mutex
	accounts []AccountRef
	// lastCheckin records the most recent successful check-in instant per
	// account, so the daily dedupe can be exercised.
	lastCheckin map[string]time.Time
	records     []checkinRecord
	catalogs    map[string][]provider.ModelInfo

	// now stamps a recorded check-in. It MUST be the harness's injected clock,
	// not time.Now().
	//
	// The scheduler dedupes by comparing the stored instant against its own
	// s.now(), which the harness fakes to a fixed date. Stamping with the real
	// clock made the two disagree on every real day except the faked one, so the
	// dedupe never fired and TestCheckinAlreadyCountsAsSuccess failed with
	// "must suppress further attempts, got 2 calls" whenever the wall clock moved
	// past the hard-coded date. A test whose result depends on today's date is a
	// defect in the test, not a flaky run.
	now func() time.Time
}

type checkinRecord struct {
	accountID string
	status    string
	message   string
}

func newFakeAccounts(accounts ...AccountRef) *fakeAccounts {
	return &fakeAccounts{
		accounts:    accounts,
		lastCheckin: map[string]time.Time{},
		catalogs:    map[string][]provider.ModelInfo{},
		now:         time.Now,
	}
}

func (f *fakeAccounts) SchedulableAccounts() []AccountRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AccountRef(nil), f.accounts...)
}

func (f *fakeAccounts) RecordCheckin(_ context.Context, accountID, status, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, checkinRecord{accountID, status, message})
	if status == provider.CheckinSuccess || status == provider.CheckinAlready {
		stamp := time.Now
		if f.now != nil {
			stamp = f.now
		}
		f.lastCheckin[accountID] = stamp()
	}
	return nil
}

func (f *fakeAccounts) LastCheckinAt(_ context.Context, accountID string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastCheckin[accountID], nil
}

func (f *fakeAccounts) MergeCatalog(accountID string, models []provider.ModelInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.catalogs[accountID] = models
}

func (f *fakeAccounts) checkins() []checkinRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]checkinRecord(nil), f.records...)
}

// fakeCreds is a static credential source.
type fakeCreds struct {
	mu    sync.Mutex
	byID  map[string]provider.Credential
	error map[string]error
}

func newFakeCreds(ids ...string) *fakeCreds {
	c := &fakeCreds{byID: map[string]provider.Credential{}, error: map[string]error{}}
	for _, id := range ids {
		c.byID[id] = provider.Credential{
			AccountID: id, Provider: "workbuddy",
			Raw:    []byte(`{"access_token":"at","uid":"u"}`),
			Fields: map[string]string{"access_token": "at", "uid": "u"},
		}
	}
	return c
}

func (c *fakeCreds) CredentialFor(accountID string) (provider.Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err, ok := c.error[accountID]; ok {
		return provider.Credential{}, err
	}
	cred, ok := c.byID[accountID]
	if !ok {
		return provider.Credential{}, errors.New("no credential")
	}
	return cred, nil
}

// harness bundles the scheduler and its doubles.
type harness struct {
	scheduler *Scheduler
	provider  *fakeProvider
	accounts  *fakeAccounts
	creds     *fakeCreds
}

// clock is a controllable clock for deterministic hour tests.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(t time.Time) *clock { return &clock{now: t} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

func newHarness(t *testing.T, at time.Time, accounts ...AccountRef) *harness {
	t.Helper()

	adapter := newFakeProvider()
	registry := provider.NewRegistry()
	registry.Register(adapter)

	source := newFakeAccounts(accounts...)
	clk := newClock(at)
	// Share the harness clock with the account source so a recorded check-in
	// lands on the SAME faked day the scheduler compares against. Using the real
	// clock here made the daily dedupe depend on today's date, which is a defect
	// in the test rather than a flaky run: it passed only while the wall clock
	// happened to be inside the hard-coded day.
	source.now = clk.Now
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	creds := newFakeCreds(ids...)

	s := New(registry, source, creds, Options{
		CatalogRefresh:   time.Hour,
		CheckinEnabled:   true,
		CheckinHours:     []int{9, 21},
		KeepaliveEnabled: true,
		KeepaliveHours:   []int{22},
		CooldownGC:       time.Hour,
		Now:              clk.Now,
	})

	return &harness{scheduler: s, provider: adapter, accounts: source, creds: creds}
}

func account(id string, mutate ...func(*AccountRef)) AccountRef {
	ref := AccountRef{ID: id, Name: id, Provider: "workbuddy", Region: "cn", Enabled: true, AutoCheckin: true}
	for _, m := range mutate {
		m(&ref)
	}
	return ref
}

func notAutoCheckin(ref *AccountRef) { ref.AutoCheckin = false }
func disabled(ref *AccountRef)       { ref.Enabled = false }

// ---- hour dedupe ----

// A job configured for hour 9 must fire ONCE in that hour, not once per tick.
// Without the dedupe a one-minute tick would run it sixty times.
func TestHourlyJobRunsOncePerHour(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"))

	// Simulate many ticks inside the same hour.
	ran := 0
	for i := 0; i < 20; i++ {
		if !h.scheduler.alreadyRanThisHour("checkin", at) {
			ran++
		}
	}
	if ran != 1 {
		t.Errorf("the hourly guard allowed %d runs, want exactly 1", ran)
	}
}

func TestHourlyGuardAllowsNextHour(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 30, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"))

	if h.scheduler.alreadyRanThisHour("checkin", at) {
		t.Fatalf("the first run should be allowed")
	}
	if !h.scheduler.alreadyRanThisHour("checkin", at) {
		t.Errorf("a repeat in the same hour should be blocked")
	}
	next := at.Add(time.Hour)
	if h.scheduler.alreadyRanThisHour("checkin", next) {
		t.Errorf("the next hour should be allowed")
	}
}

func TestHourlyGuardAllowsNextDay(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"))

	h.scheduler.alreadyRanThisHour("checkin", at)

	// Same clock hour, next calendar day: must be allowed. A naive
	// hour-only comparison would wrongly block the next day's run.
	tomorrow := at.AddDate(0, 0, 1)
	if h.scheduler.alreadyRanThisHour("checkin", tomorrow) {
		t.Errorf("the same hour on the next day must be allowed")
	}
}

func TestHourlyJobsAreIndependent(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"))

	if h.scheduler.alreadyRanThisHour("checkin", at) {
		t.Fatalf("checkin should be allowed")
	}
	// A different job in the same hour must not be blocked by the first.
	if h.scheduler.alreadyRanThisHour("keepalive", at) {
		t.Errorf("keepalive must have its own run record")
	}
}

// ---- check-in ----

func TestCheckinRunsForOptedInAccounts(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"), account("acc_b", notAutoCheckin))

	h.scheduler.runCheckin(context.Background())

	if _, checkins, _ := h.provider.counts(); checkins != 1 {
		t.Errorf("check-in ran %d times, want 1 (only the opted-in account)", checkins)
	}
	records := h.accounts.checkins()
	if len(records) != 1 || records[0].accountID != "acc_a" {
		t.Errorf("records=%+v, want one for acc_a", records)
	}
}

func TestCheckinSkipsDisabledAccounts(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a", disabled))

	h.scheduler.runCheckin(context.Background())

	if _, checkins, _ := h.provider.counts(); checkins != 0 {
		t.Errorf("a disabled account must not be checked in, got %d calls", checkins)
	}
}

// An account that already checked in today must not be called again: the whole
// point of a daily job is that it runs once per day.
func TestCheckinSkipsAlreadyDoneToday(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"))
	h.accounts.lastCheckin["acc_a"] = time.Date(2026, 9, 30, 8, 0, 0, 0, time.Local)

	h.scheduler.runCheckin(context.Background())

	if _, checkins, _ := h.provider.counts(); checkins != 0 {
		t.Errorf("an account already checked in today must be skipped, got %d calls", checkins)
	}
}

// A check-in from the previous day must NOT block today's.
func TestCheckinRunsAgainNextDay(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"))
	h.accounts.lastCheckin["acc_a"] = time.Date(2026, 9, 29, 9, 0, 0, 0, time.Local)

	h.scheduler.runCheckin(context.Background())

	if _, checkins, _ := h.provider.counts(); checkins != 1 {
		t.Errorf("yesterday's check-in must not block today's, got %d calls", checkins)
	}
}

// A channel without a check-in must not be retried, and must not be recorded as
// an error: it simply has no check-in.
func TestCheckinUnsupportedIsNotAnError(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"))
	h.provider.unsupportedCheckin = true

	h.scheduler.runCheckin(context.Background())

	if records := h.accounts.checkins(); len(records) != 0 {
		t.Errorf("an unsupported check-in must not be recorded, got %+v", records)
	}
}

func TestCheckinFailureIsRecorded(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"))
	h.provider.checkinErr = errors.New("upstream exploded")

	h.scheduler.runCheckin(context.Background())

	records := h.accounts.checkins()
	if len(records) != 1 {
		t.Fatalf("records=%+v, want one failure record", records)
	}
	if records[0].status != provider.CheckinError {
		t.Errorf("status=%q, want %q", records[0].status, provider.CheckinError)
	}
	if records[0].message == "" {
		t.Errorf("a failure must carry a message")
	}
}

// "Already checked in" from the upstream is a SUCCESS for scheduling purposes:
// the day's reward is claimed, so treating it as an error would retry all day.
func TestCheckinAlreadyCountsAsSuccess(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"))
	h.provider.checkinResp = provider.CheckinResult{Status: provider.CheckinAlready, Message: "今天已签到"}

	h.scheduler.runCheckin(context.Background())

	records := h.accounts.checkins()
	if len(records) != 1 || records[0].status != provider.CheckinAlready {
		t.Fatalf("records=%+v, want one 'already' record", records)
	}
	// And the next tick in the same day must skip it.
	h.scheduler.runCheckin(context.Background())
	if _, checkins, _ := h.provider.counts(); checkins != 1 {
		t.Errorf("an 'already' result must suppress further attempts, got %d calls", checkins)
	}
}

// ---- catalogue refresh ----

func TestCatalogRefreshPopulatesThePool(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"))

	h.scheduler.refreshCatalogs(context.Background())

	if catalog, _, _ := h.provider.counts(); catalog != 1 {
		t.Errorf("catalogue was fetched %d times, want 1", catalog)
	}
	models, ok := h.accounts.catalogs["acc_a"]
	if !ok {
		t.Fatalf("the catalogue was not merged into the pool")
	}
	if len(models) != 1 || models[0].NativeModel != "glm-5.2" {
		t.Errorf("models=%+v, want glm-5.2", models)
	}
}

// A catalogue failure must not stop the other accounts, and must not be fatal.
func TestCatalogFailureDoesNotBlockOtherAccounts(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"), account("acc_b"))
	h.provider.catalogErr = errors.New("catalogue unavailable")

	h.scheduler.refreshCatalogs(context.Background())

	// Both were attempted even though the first failed.
	if catalog, _, _ := h.provider.counts(); catalog != 2 {
		t.Errorf("attempts=%d, want 2 (a failure must not abort the sweep)", catalog)
	}
}

func TestCatalogSkipsAccountsWithoutCredentials(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"), account("acc_b"))
	// acc_b has no credential: it was added but never logged in.
	h.creds.error["acc_b"] = errors.New("no credential stored")

	h.scheduler.refreshCatalogs(context.Background())

	if catalog, _, _ := h.provider.counts(); catalog != 1 {
		t.Errorf("attempts=%d, want 1 (an account without a credential is skipped)", catalog)
	}
}

func TestCatalogSkipsDisabledAccounts(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a", disabled))

	h.scheduler.refreshCatalogs(context.Background())

	if catalog, _, _ := h.provider.counts(); catalog != 0 {
		t.Errorf("a disabled account must not be refreshed, got %d calls", catalog)
	}
}

// ---- keep-alive ----

// A token that is comfortably valid must not be refreshed: doing so churns the
// credential and can race an in-flight request.
func TestKeepaliveSkipsFreshTokens(t *testing.T) {
	at := time.Date(2026, 9, 30, 22, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"))
	cred := h.creds.byID["acc_a"]
	cred.ExpiresAt = at.Add(48 * time.Hour)
	h.creds.byID["acc_a"] = cred

	h.scheduler.runKeepalive(context.Background())

	if _, _, refresh := h.provider.counts(); refresh != 0 {
		t.Errorf("a fresh token must not be refreshed, got %d calls", refresh)
	}
}

// A token close to expiry must be refreshed.
func TestKeepaliveRefreshesExpiringTokens(t *testing.T) {
	at := time.Date(2026, 9, 30, 22, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"))
	cred := h.creds.byID["acc_a"]
	cred.ExpiresAt = at.Add(time.Hour)
	h.creds.byID["acc_a"] = cred

	h.scheduler.runKeepalive(context.Background())

	if _, _, refresh := h.provider.counts(); refresh != 1 {
		t.Errorf("an expiring token should be refreshed, got %d calls", refresh)
	}
}

// An unknown expiry means "try it and see", so the refresh is attempted.
func TestKeepaliveRefreshesWhenExpiryUnknown(t *testing.T) {
	at := time.Date(2026, 9, 30, 22, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"))
	cred := h.creds.byID["acc_a"]
	cred.ExpiresAt = time.Time{}
	h.creds.byID["acc_a"] = cred

	h.scheduler.runKeepalive(context.Background())

	if _, _, refresh := h.provider.counts(); refresh != 1 {
		t.Errorf("an unknown expiry should trigger a refresh, got %d calls", refresh)
	}
}

// ---- lifecycle ----

func TestStartIsIdempotent(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)
	h := newHarness(t, at, account("acc_a"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.scheduler.Start(ctx)
	h.scheduler.Start(ctx) // a second call must be ignored
	h.scheduler.Stop()
	h.scheduler.Stop() // stopping twice must not panic
}

// ---- helpers ----

func TestNormalizeHours(t *testing.T) {
	got := NormalizeHours([]int{21, 9, 9, 25, -1, 9})
	want := []int{9, 21}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestSameLocalDay(t *testing.T) {
	a := time.Date(2026, 9, 30, 0, 1, 0, 0, time.Local)
	b := time.Date(2026, 9, 30, 23, 59, 0, 0, time.Local)
	if !sameLocalDay(a, b) {
		t.Errorf("instants on the same local day should match")
	}
	c := time.Date(2026, 10, 1, 0, 1, 0, 0, time.Local)
	if sameLocalDay(a, c) {
		t.Errorf("instants on different local days must not match")
	}
}
