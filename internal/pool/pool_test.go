package pool

import (
	"testing"
	"time"
)

func item(id string, mutate ...func(*Item)) Item {
	it := Item{
		ID:          id,
		Name:        id,
		Provider:    "workbuddy",
		Region:      "cn",
		Enabled:     true,
		Weight:      50,
		MaxInflight: 4,
	}
	for _, m := range mutate {
		m(&it)
	}
	return it
}

// withModel sets a NON-NIL catalog. This matters because nil means "unknown"
// (fail open) while an empty non-nil slice means "known to be empty" (filter
// everything out); the two are deliberately different and these helpers must
// be able to express both.
func withModel(models ...string) func(*Item) {
	return func(i *Item) {
		if models == nil {
			models = []string{}
		}
		i.Models = models
	}
}

func withRegion(region string) func(*Item) {
	return func(i *Item) { i.Region = region }
}

func withWeight(weight int) func(*Item) {
	return func(i *Item) { i.Weight = weight }
}

func withProvider(p string) func(*Item) {
	return func(i *Item) { i.Provider = p }
}

func TestRoundRobinRotatesEvenly(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a"))
	p.Upsert(item("acc_b"))
	p.Upsert(item("acc_c"))

	got := map[string]int{}
	for i := 0; i < 9; i++ {
		picked, ok := p.Pick(RouteQuery{})
		if !ok {
			t.Fatalf("pick %d: expected an account", i)
		}
		got[picked.ID]++
	}
	for _, id := range []string{"acc_a", "acc_b", "acc_c"} {
		if got[id] != 3 {
			t.Errorf("round-robin over 9 picks: %s got %d, want 3", id, got[id])
		}
	}
}

func TestRoundRobinSkipsCoolingAccount(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a"))
	p.Upsert(item("acc_b"))

	p.MarkDown("acc_a", time.Minute, "boom")

	for i := 0; i < 6; i++ {
		picked, ok := p.Pick(RouteQuery{})
		if !ok {
			t.Fatalf("pick %d: expected an account", i)
		}
		if picked.ID != "acc_b" {
			t.Fatalf("pick %d: got %s, want acc_b (acc_a is cooling)", i, picked.ID)
		}
	}
}

// A model-scoped cooldown must not take the account offline for its other
// models. This is the single most important isolation property.
func TestModelCooldownIsIsolated(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", withModel("glm-5.2", "deepseek-v4-pro")))

	p.MarkClassified("acc_a", Classified{
		Kind:     KindRateLimit,
		Cooldown: time.Minute,
		Model:    "glm-5.2",
	}, 3)

	// The failed model is unroutable.
	if _, ok := p.Pick(RouteQuery{PublicModel: "glm-5.2"}); ok {
		t.Fatalf("glm-5.2 should be cooling for acc_a")
	}
	// A different model on the same account must still work.
	picked, ok := p.Pick(RouteQuery{PublicModel: "deepseek-v4-pro"})
	if !ok {
		t.Fatalf("deepseek-v4-pro should still be routable on acc_a")
	}
	if picked.ID != "acc_a" {
		t.Fatalf("got %s, want acc_a", picked.ID)
	}
}

func TestInvalidRequestDoesNotCoolAccount(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a"))

	p.MarkClassified("acc_a", Classified{
		Kind:     KindInvalidRequest,
		Cooldown: 0,
		Model:    "glm-5.2",
	}, 3)

	// The account is healthy, so it stays routable for everything.
	if _, ok := p.Pick(RouteQuery{PublicModel: "glm-5.2"}); !ok {
		t.Fatalf("invalid_request must not make the account unroutable")
	}
	if _, ok := p.Pick(RouteQuery{PublicModel: "other-model"}); !ok {
		t.Fatalf("invalid_request must not make the account unroutable for other models")
	}

	stored, _ := p.ByID("acc_a")
	if !stored.DownUntil.IsZero() {
		t.Errorf("invalid_request set a cooldown, want none")
	}
}

func TestModelNotAvailableDoesNotCoolAccount(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", withModel("glm-5.2")))

	p.MarkClassified("acc_a", Classified{
		Kind:     KindModelNotAvailable,
		Cooldown: 0,
		Model:    "nonexistent",
	}, 3)

	stored, _ := p.ByID("acc_a")
	if !stored.DownUntil.IsZero() {
		t.Errorf("model_not_available set an account cooldown, want none")
	}
	if _, ok := p.Pick(RouteQuery{PublicModel: "glm-5.2"}); !ok {
		t.Errorf("account should still serve its real models")
	}
}

func TestQuotaCoolsWholeAccount(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", withModel("glm-5.2", "deepseek-v4-pro")))

	p.MarkClassified("acc_a", Classified{
		Kind:     KindQuota,
		Cooldown: time.Hour,
		Model:    "glm-5.2",
	}, 3)

	// Quota exhaustion is pool-wide: even other models on this account must
	// stop, because the allowance backing them is gone too.
	if _, ok := p.Pick(RouteQuery{PublicModel: "deepseek-v4-pro"}); ok {
		t.Fatalf("quota must cool the whole account, not just the model")
	}
}

func TestWeightedRoundRobinHonoursWeights(t *testing.T) {
	p := New(StrategyWeightedRoundRobin)
	p.Upsert(item("acc_a", withWeight(75)))
	p.Upsert(item("acc_b", withWeight(25)))

	got := map[string]int{}
	const picks = 400
	for i := 0; i < picks; i++ {
		picked, ok := p.Pick(RouteQuery{})
		if !ok {
			t.Fatalf("pick %d: expected an account", i)
		}
		got[picked.ID]++
	}
	// 75:25 over 400 picks is 300:100. Smooth WRR is exact over a window, so
	// allow a small tolerance for the phase at the cut.
	if got["acc_a"] < 285 || got["acc_a"] > 315 {
		t.Errorf("acc_a got %d picks, want ~300 (75%%)", got["acc_a"])
	}
	if got["acc_b"] < 85 || got["acc_b"] > 115 {
		t.Errorf("acc_b got %d picks, want ~100 (25%%)", got["acc_b"])
	}
}

func TestWeightedFallsBackToRoundRobinWhenUniform(t *testing.T) {
	p := New(StrategyWeightedRoundRobin)
	p.Upsert(item("acc_a", withWeight(50)))
	p.Upsert(item("acc_b", withWeight(50)))

	got := map[string]int{}
	for i := 0; i < 10; i++ {
		picked, _ := p.Pick(RouteQuery{})
		got[picked.ID]++
	}
	if got["acc_a"] != 5 || got["acc_b"] != 5 {
		t.Errorf("uniform weights should split evenly, got %v", got)
	}
}

func TestFillFirstAlwaysPrefersHighestWeight(t *testing.T) {
	p := New(StrategyFillFirst)
	p.Upsert(item("acc_a", withWeight(10)))
	p.Upsert(item("acc_b", withWeight(90)))

	for i := 0; i < 5; i++ {
		picked, _ := p.Pick(RouteQuery{})
		if picked.ID != "acc_b" {
			t.Fatalf("fill-first pick %d: got %s, want acc_b", i, picked.ID)
		}
	}
}

func TestSaturatedAccountIsNotPicked(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", func(i *Item) { i.MaxInflight = 1 }))
	p.Upsert(item("acc_b"))

	if !p.Acquire("acc_a") {
		t.Fatalf("first acquire on acc_a should succeed")
	}
	picked, ok := p.Pick(RouteQuery{})
	if !ok {
		t.Fatalf("expected acc_b to be picked")
	}
	if picked.ID != "acc_b" {
		t.Fatalf("got %s, want acc_b (acc_a is saturated)", picked.ID)
	}
}

// When every candidate is merely saturated (no cooldown), PickRoute must say
// so explicitly: the caller should shed load, not wait for a deadline that
// does not exist.
func TestAllSaturatedReportsNoCooldownHint(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", func(i *Item) { i.MaxInflight = 1 }))

	if !p.Acquire("acc_a") {
		t.Fatalf("acquire should succeed")
	}
	res := p.PickRoute(RouteQuery{})
	if res.OK {
		t.Fatalf("expected no routable account while saturated")
	}
	if res.Reason != ReasonAllSaturated {
		t.Errorf("reason=%q, want %q", res.Reason, ReasonAllSaturated)
	}
	if !res.RetryAt.IsZero() {
		t.Errorf("a saturated-only pool must not offer a retry deadline, got %s", res.RetryAt)
	}
}

// When candidates are cooling, PickRoute must report a retry hint naming the
// account that frees up soonest, so the caller can produce a useful
// retry-after instead of a bare failure.
func TestCoolingPoolNamesSoonestRecovery(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a"))
	p.Upsert(item("acc_b"))

	p.MarkDown("acc_a", 10*time.Minute, "slow")
	p.MarkDown("acc_b", 1*time.Minute, "fast")

	res := p.PickRoute(RouteQuery{})
	if res.OK {
		t.Fatalf("expected no routable account")
	}
	if res.Reason != ReasonAllCooling {
		t.Errorf("reason=%q, want %q", res.Reason, ReasonAllCooling)
	}
	if res.RetryAt.IsZero() {
		t.Fatalf("expected a retry hint")
	}
	// The soonest recovery is acc_b at ~1 minute, not acc_a at ~10.
	if remaining := time.Until(res.RetryAt); remaining > 2*time.Minute {
		t.Errorf("retry hint %s is too far out; expected ~1 minute", remaining)
	}
}

func TestProviderAndRegionFilters(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("wb_cn", withProvider("workbuddy"), withRegion("cn")))
	p.Upsert(item("wb_global", withProvider("workbuddy"), withRegion("global")))
	p.Upsert(item("qoder_cn", withProvider("qoder"), withRegion("cn")))

	picked, ok := p.Pick(RouteQuery{ProviderFilter: "qoder"})
	if !ok || picked.ID != "qoder_cn" {
		t.Fatalf("provider filter: got %s ok=%v, want qoder_cn", picked.ID, ok)
	}

	picked, ok = p.Pick(RouteQuery{RegionFilter: "global"})
	if !ok || picked.ID != "wb_global" {
		t.Fatalf("region filter: got %s ok=%v, want wb_global", picked.ID, ok)
	}

	picked, ok = p.Pick(RouteQuery{ProviderFilter: "workbuddy", RegionFilter: "cn"})
	if !ok || picked.ID != "wb_cn" {
		t.Fatalf("combined filter: got %s ok=%v, want wb_cn", picked.ID, ok)
	}
}

func TestAllowedProvidersRestrictsRouting(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("wb", withProvider("workbuddy")))
	p.Upsert(item("qo", withProvider("qoder")))

	picked, ok := p.Pick(RouteQuery{AllowedProviders: []string{"qoder"}})
	if !ok || picked.ID != "qo" {
		t.Fatalf("got %s ok=%v, want qo", picked.ID, ok)
	}
}

func TestExcludedAccountsAreSkipped(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a"))
	p.Upsert(item("acc_b"))

	excluded := map[string]struct{}{"acc_a": {}}
	for i := 0; i < 4; i++ {
		picked, ok := p.Pick(RouteQuery{Excluded: excluded})
		if !ok {
			t.Fatalf("pick %d: expected acc_b", i)
		}
		if picked.ID != "acc_b" {
			t.Fatalf("got %s, want acc_b", picked.ID)
		}
	}
}

func TestPinnedAccountIsPreferred(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a"))
	p.Upsert(item("acc_b"))

	for i := 0; i < 3; i++ {
		picked, ok := p.Pick(RouteQuery{PreferAccount: "acc_b"})
		if !ok || picked.ID != "acc_b" {
			t.Fatalf("pin: got %s ok=%v, want acc_b", picked.ID, ok)
		}
	}
}

// A stale pin must not break the request: fall back to normal scheduling.
func TestPinnedAccountFallsBackWhenCooling(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a"))
	p.Upsert(item("acc_b"))

	p.MarkDown("acc_b", time.Minute, "boom")

	picked, ok := p.Pick(RouteQuery{PreferAccount: "acc_b"})
	if !ok {
		t.Fatalf("expected fallback to acc_a")
	}
	if picked.ID != "acc_a" {
		t.Fatalf("got %s, want acc_a", picked.ID)
	}
}

// An unknown catalog must fail open, otherwise a freshly added account is
// unusable until its first catalog refresh lands.
func TestUnknownCatalogFailsOpen(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a")) // Models == nil, meaning unknown

	if _, ok := p.Pick(RouteQuery{PublicModel: "anything-at-all"}); !ok {
		t.Fatalf("nil catalog must fail open")
	}
}

// A known-empty catalog must filter out, since it means "this account truly
// carries nothing".
func TestEmptyCatalogFiltersOut(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", withModel())) // non-nil empty slice

	if _, ok := p.Pick(RouteQuery{PublicModel: "anything-at-all"}); ok {
		t.Fatalf("empty catalog must not route unknown models")
	}
}

func TestCatalogModelMatches(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", withModel("glm-5.2")))

	if _, ok := p.Pick(RouteQuery{PublicModel: "glm-5.2"}); !ok {
		t.Fatalf("exact model should match")
	}
	// Separator and case folding must make these equivalent.
	if _, ok := p.Pick(RouteQuery{PublicModel: "GLM_5.2"}); !ok {
		t.Fatalf("case/separator folding should match glm-5.2")
	}
	if _, ok := p.Pick(RouteQuery{PublicModel: "other"}); ok {
		t.Fatalf("unknown model should not match")
	}
}

// "auto" means "any model", so it must never be filtered by the catalog.
func TestAutoModelMatchesAnything(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", withModel()))

	if _, ok := p.Pick(RouteQuery{PublicModel: "auto"}); !ok {
		t.Fatalf("auto must match regardless of catalog")
	}
}

// Proven models survive a catalog refresh that omits them.
func TestProvenModelSurvivesCatalogRefresh(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a"))

	p.MarkOK("acc_a", "live-model")

	// Upstream catalog now reports something else entirely.
	p.MergeModels("acc_a", []string{"other-model"})

	if _, ok := p.Pick(RouteQuery{PublicModel: "live-model"}); !ok {
		t.Fatalf("a model that succeeded must not be dropped by a catalog refresh")
	}
}

func TestMergeModelsIncludesProven(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a"))

	p.MarkOK("acc_a", "proven-one")
	p.MergeModels("acc_a", []string{"catalog-one"})

	stored, _ := p.ByID("acc_a")
	found := map[string]bool{}
	for _, m := range stored.Models {
		found[CanonicalModelID(m)] = true
	}
	if !found["catalog-one"] {
		t.Errorf("catalog model missing from merged list: %v", stored.Models)
	}
	if !found["proven-one"] {
		t.Errorf("proven model missing from merged list: %v", stored.Models)
	}
}

func TestMarkOKClearsCooldownAndLadder(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", withModel("glm-5.2")))

	p.MarkClassified("acc_a", Classified{Kind: KindRateLimit, Cooldown: time.Minute, Model: "glm-5.2"}, 3)
	p.MarkOK("acc_a", "glm-5.2")

	stored, _ := p.ByID("acc_a")
	if len(stored.ModelDownUntil) != 0 {
		t.Errorf("MarkOK left model cooldowns: %v", stored.ModelDownUntil)
	}
	if stored.BackoffLevel != 0 {
		t.Errorf("MarkOK left backoff level %d, want 0", stored.BackoffLevel)
	}
	if !stored.DownUntil.IsZero() {
		t.Errorf("MarkOK left account cooldown")
	}
}

// Repeated same-kind failures must lengthen the cooldown.
func TestBackoffLadderClimbsOnRepeats(t *testing.T) {
	p := New(StrategyRoundRobin)
	base := 10 * time.Second
	level := 0
	var durations []time.Duration
	for i := 0; i < 4; i++ {
		d, next := p.failureCooldown(Classified{Kind: KindUnavailable, Cooldown: base}, level, 1)
		durations = append(durations, d)
		level = next
	}
	// 10s is below the floor, so the ladder pins at the floor then doubles.
	for i := 1; i < len(durations); i++ {
		if durations[i] < durations[i-1] {
			t.Fatalf("ladder decreased: %v", durations)
		}
	}
	for _, d := range durations {
		if d < backoffFloor {
			t.Errorf("cooldown %s is below the floor %s", d, backoffFloor)
		}
		if d > backoffCeiling {
			t.Errorf("cooldown %s exceeds the ceiling %s", d, backoffCeiling)
		}
	}
}

func TestBackoffLadderIsCapped(t *testing.T) {
	p := New(StrategyRoundRobin)
	level := backoffMaxLevel + 5
	d, next := p.failureCooldown(Classified{Kind: KindUnavailable, Cooldown: time.Minute}, level, 1)
	if d != p.breakerBase*(1<<backoffMaxLevel) && d != p.breakerMax {
		t.Errorf("退避未按最大级数限制：%s", d)
	}
	if next != 1+backoffMaxLevel {
		t.Errorf("失败计数未封顶：%d", next)
	}
}

// A different failure kind must restart the ladder rather than inherit the
// previous streak's length.
func TestKindChangeResetsLadder(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a"))

	// Trip the ladder with repeats of one kind.
	for i := 0; i < 3; i++ {
		p.MarkClassified("acc_a", Classified{Kind: KindRateLimit, Cooldown: time.Minute}, 1)
	}
	afterRepeats, _ := p.ByID("acc_a")
	if afterRepeats.BackoffLevel < 3 {
		t.Fatalf("expected the ladder to climb past 3, got %d", afterRepeats.BackoffLevel)
	}
	repeatCooldown := time.Until(afterRepeats.DownUntil)

	// A different kind is a new problem, so the ladder restarts at step 1
	// and the resulting cooldown must be shorter than the accumulated one.
	p.MarkClassified("acc_a", Classified{Kind: KindAuth, Cooldown: time.Minute}, 1)
	afterChange, _ := p.ByID("acc_a")

	if afterChange.BackoffLevel != 1 {
		t.Errorf("kind change should restart the ladder at 1, got %d", afterChange.BackoffLevel)
	}
	changeCooldown := time.Until(afterChange.DownUntil)
	if changeCooldown >= repeatCooldown {
		t.Errorf("kind change should shorten the cooldown: repeat=%s change=%s",
			repeatCooldown, changeCooldown)
	}
}

func TestSweepExpiredClearsCooldowns(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", withModel("glm-5.2")))

	p.MarkClassified("acc_a", Classified{Kind: KindRateLimit, Cooldown: time.Minute, Model: "glm-5.2"}, 3)
	p.MarkDown("acc_b_missing", time.Minute, "x")

	// Sweep in the future so every cooldown has elapsed.
	resumed := p.SweepExpired(time.Now().Add(2 * time.Hour))

	stored, _ := p.ByID("acc_a")
	if !stored.DownUntil.IsZero() {
		t.Errorf("account cooldown not cleared")
	}
	if len(stored.ModelDownUntil) != 0 {
		t.Errorf("model cooldowns not cleared: %v", stored.ModelDownUntil)
	}
	if len(resumed) == 0 {
		t.Errorf("expected at least one resumed account")
	}
}

// 冷却结束只代表允许重试，成功之前仍需保留同类连续失败的计数。
func TestSweepExpiredPreservesModelLadder(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", withModel("glm-5.2")))

	p.MarkClassified("acc_a", Classified{Kind: KindRateLimit, Cooldown: time.Second, Model: "glm-5.2"}, 3)
	p.SweepExpired(time.Now().Add(time.Hour))

	stored, _ := p.ByID("acc_a")
	if stored.ModelBackoff["glm-5.2"] != 1 {
		t.Errorf("冷却清扫错误清除了失败计数：%v", stored.ModelBackoff)
	}
}

func TestAcquireReleaseRespectsCap(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", func(i *Item) { i.MaxInflight = 2 }))

	if !p.Acquire("acc_a") {
		t.Fatalf("acquire 1 failed")
	}
	if !p.Acquire("acc_a") {
		t.Fatalf("acquire 2 failed")
	}
	if p.Acquire("acc_a") {
		t.Fatalf("acquire 3 should fail at the cap")
	}
	p.Release("acc_a")
	if !p.Acquire("acc_a") {
		t.Fatalf("acquire after release should succeed")
	}
}

func TestUpsertPreservesRuntimeHealth(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", withModel("glm-5.2")))

	p.MarkClassified("acc_a", Classified{Kind: KindRateLimit, Cooldown: time.Hour, Model: "glm-5.2"}, 3)
	p.MarkOK("acc_a", "deepseek")

	// A configuration refresh arrives.
	p.Upsert(item("acc_a", withModel("glm-5.2"), func(i *Item) { i.Name = "renamed" }))

	stored, _ := p.ByID("acc_a")
	if stored.Name != "renamed" {
		t.Errorf("config change not applied: name=%s", stored.Name)
	}
	if _, cooling := stored.ModelDownUntil["glm-5.2"]; !cooling {
		t.Errorf("upsert cleared an active model cooldown")
	}
	if !itemHasProvenModel(stored, "deepseek") {
		t.Errorf("upsert cleared proven models: %v", stored.ProvenModels)
	}
}

func TestDisabledAccountIsNotRoutable(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", func(i *Item) { i.Enabled = false }))

	if _, ok := p.Pick(RouteQuery{}); ok {
		t.Fatalf("disabled account must not be routable")
	}
}

func TestRemoveCleansIndexAndCursor(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a"))
	p.Upsert(item("acc_b"))
	p.Upsert(item("acc_c"))

	if _, ok := p.Pick(RouteQuery{}); !ok {
		t.Fatalf("warm-up pick failed")
	}
	p.Remove("acc_b")

	if _, ok := p.ByID("acc_b"); ok {
		t.Errorf("removed account still resolvable")
	}
	if p.Len() != 2 {
		t.Errorf("Len()=%d, want 2", p.Len())
	}
	// Remaining accounts must still rotate correctly after the index rebuild.
	got := map[string]int{}
	for i := 0; i < 4; i++ {
		picked, ok := p.Pick(RouteQuery{})
		if !ok {
			t.Fatalf("pick after remove failed")
		}
		got[picked.ID]++
	}
	if got["acc_a"] != 2 || got["acc_c"] != 2 {
		t.Errorf("rotation broken after remove: %v", got)
	}
}

func TestSyncFromStoreRemovesMissingAccounts(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a"))
	p.Upsert(item("acc_b"))

	p.SyncFromStore([]Item{item("acc_b")})

	if _, ok := p.ByID("acc_a"); ok {
		t.Errorf("acc_a should have been removed")
	}
	if _, ok := p.ByID("acc_b"); !ok {
		t.Errorf("acc_b should remain")
	}
}

// Region affinity: an unpinned route must not flip regions as rotation
// advances, otherwise one conversation's context jumps between upstreams.
func TestRegionAffinityKeepsRouteStable(t *testing.T) {
	p := New(StrategyRoundRobin)
	// Two CN accounts and one Global: the majority region is CN.
	p.Upsert(item("cn_a", withRegion("cn")))
	p.Upsert(item("cn_b", withRegion("cn")))
	p.Upsert(item("gl_a", withRegion("global")))

	regions := map[string]int{}
	for i := 0; i < 12; i++ {
		picked, ok := p.Pick(RouteQuery{PublicModel: "glm-5.2"})
		if !ok {
			t.Fatalf("pick %d failed", i)
		}
		regions[picked.Region]++
	}
	if regions["global"] != 0 {
		t.Errorf("route flipped to a minority region: %v", regions)
	}
}

func TestCanonicalModelID(t *testing.T) {
	cases := map[string]string{
		"GLM-5.2":   "glm-5.2",
		"glm_5.2":   "glm-5.2",
		" glm 5.2 ": "glm-5.2",
		"":          "",
	}
	for in, want := range cases {
		if got := CanonicalModelID(in); got != want {
			t.Errorf("CanonicalModelID(%q)=%q, want %q", in, got, want)
		}
	}
}
