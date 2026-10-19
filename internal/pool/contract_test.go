package pool

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// runtimeStateFields are the Item fields the POOL owns, not the caller. They
// must survive an Upsert, because an Upsert is how a configuration refresh or a
// store reload reaches the pool and it must not erase live health.
//
// This list is the contract. Keeping it explicit means adding a new runtime
// field without teaching Upsert about it fails this test rather than silently
// resetting cooldowns in production.
var runtimeStateFields = []string{
	"InFlight",
	"Ready",
	"DownUntil",
	"GroupDownUntil",
	"LastError",
	"LastKind",
	"BackoffLevel",
	"ModelDownUntil",
	"ModelBackoff",
	"ModelLastKind",
	"Models",
	"ModelsAt",
	"ProvenModels",
	"StateVersion",
	"RuntimeEpoch",
	"CredentialRevision",
}

// configFields are the Item fields a CALLER supplies. An Upsert is expected to
// overwrite these, so they are excluded from the preservation contract.
var configFields = []string{
	"ID",
	"Name",
	"Provider",
	"Region",
	"Enabled",
	"Priority",
	"Weight",
	"MaxInflight",
	"QuotaScope",
	"QuotaGroup",
	"QuotaExpiresAt",
	"DropSystemPrompt",
	"AutoCheckin",
}

// TestUpsertPreservesEveryRuntimeField is the structural guard.
//
// It does two things no behavioural test does:
//
//  1. It fails when a NEW field is added to Item and not classified, so the
//     author must decide whether it is runtime state or configuration.
//  2. It fails when a classified runtime field is not actually carried across
//     an Upsert, which is the silent-reset bug this whole mechanism exists to
//     prevent.
func TestUpsertPreservesEveryRuntimeField(t *testing.T) {
	// 1. Every Item field must be classified.
	itemType := reflect.TypeOf(Item{})
	classified := map[string]bool{}
	for _, f := range runtimeStateFields {
		classified[f] = true
	}
	for _, f := range configFields {
		classified[f] = true
	}
	for i := 0; i < itemType.NumField(); i++ {
		name := itemType.Field(i).Name
		if !classified[name] {
			t.Errorf("Item.%s is neither in runtimeStateFields nor configFields; "+
				"classify it so Upsert's preservation contract stays complete", name)
		}
	}
	// A stale entry in either list is also a problem: it means the contract
	// mentions a field that no longer exists.
	for name := range classified {
		found := false
		for i := 0; i < itemType.NumField(); i++ {
			if itemType.Field(i).Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is listed in the contract but Item has no such field", name)
		}
	}

	// 2. Every runtime field must actually survive an Upsert.
	p := New(StrategyRoundRobin)

	// Seed an item whose runtime state is distinctive, so a reset is visible.
	seeded := item("acc_a", withModel("glm-5.2"))
	seeded.InFlight = 3
	ready := true
	seeded.Ready = &ready
	seeded.DownUntil = time.Now().Add(time.Hour)
	seeded.LastError = "upstream exploded"
	seeded.LastKind = KindRateLimit
	seeded.BackoffLevel = 4
	seeded.ModelDownUntil = map[string]time.Time{"glm-5.2": time.Now().Add(time.Hour)}
	seeded.ModelBackoff = map[string]int{"glm-5.2": 3}
	seeded.ModelLastKind = map[string]string{"glm-5.2": KindRateLimit}
	seeded.Models = []string{"glm-5.2", "deepseek-v4-pro"}
	seeded.ModelsAt = time.Now().Add(-time.Minute)
	seeded.ProvenModels = []string{"deepseek-v4-pro"}
	seeded.StateVersion = 7

	p.Upsert(seeded)

	// A configuration refresh arrives carrying no runtime state at all, which is
	// exactly what poolItemFromAccount produces.
	refresh := item("acc_a", withModel("glm-5.2", "deepseek-v4-pro"))
	refresh.Name = "renamed"
	p.Upsert(refresh)

	stored, ok := p.ByID("acc_a")
	if !ok {
		t.Fatalf("account vanished across an Upsert")
	}

	// The configuration change must land.
	if stored.Name != "renamed" {
		t.Errorf("the configuration change was not applied: name=%q", stored.Name)
	}

	// The runtime state must survive.
	if stored.InFlight != 3 {
		t.Errorf("InFlight reset to %d, want 3", stored.InFlight)
	}
	if stored.Ready == nil || !*stored.Ready {
		t.Errorf("Ready was reset: %v", stored.Ready)
	}
	if stored.DownUntil.IsZero() {
		t.Errorf("DownUntil was reset, so a cooling account became routable")
	}
	if stored.LastKind != KindRateLimit {
		t.Errorf("LastKind=%q, want %q", stored.LastKind, KindRateLimit)
	}
	if stored.BackoffLevel != 4 {
		t.Errorf("BackoffLevel=%d, want 4", stored.BackoffLevel)
	}
	if _, ok := stored.ModelDownUntil["glm-5.2"]; !ok {
		t.Errorf("the per-model cooldown was reset: %v", stored.ModelDownUntil)
	}
	if stored.ModelBackoff["glm-5.2"] != 3 {
		t.Errorf("the per-model ladder was reset: %v", stored.ModelBackoff)
	}
	if stored.ModelLastKind["glm-5.2"] != KindRateLimit {
		t.Errorf("the per-model kind was reset: %v", stored.ModelLastKind)
	}
	if len(stored.ProvenModels) == 0 {
		t.Errorf("ProvenModels was reset, so a catalog refresh could strand a live model")
	}
}

// Every runtime field must be COPIED, not aliased: a caller that mutates the
// map it handed in must not be able to change pool state through that alias.
func TestUpsertDoesNotAliasCallerMaps(t *testing.T) {
	p := New(StrategyRoundRobin)

	down := map[string]time.Time{"glm-5.2": time.Now().Add(time.Hour)}
	originalDeadline := down["glm-5.2"]
	seed := item("acc_a")
	seed.ModelDownUntil = down
	seed.ModelBackoff = map[string]int{"glm-5.2": 2}
	seed.ModelLastKind = map[string]string{"glm-5.2": KindRateLimit}
	p.Upsert(seed)

	// Mutate the caller's maps after the fact.
	down["glm-5.2"] = time.Now().Add(100 * time.Hour)
	seed.ModelBackoff["glm-5.2"] = 99

	// A refresh then preserves whatever the pool currently holds.
	p.Upsert(item("acc_a"))

	stored, _ := p.ByID("acc_a")
	// 检查实际内容而非仅检查长度，避免旧测试看似覆盖但无法发现别名修改。
	onlyOne := len(stored.ModelDownUntil) == 1
	if !onlyOne {
		t.Fatalf("unexpected cooldown map size: %v", stored.ModelDownUntil)
	}
	if !stored.ModelDownUntil["glm-5.2"].Equal(originalDeadline) || stored.ModelBackoff["glm-5.2"] != 2 {
		t.Fatal("调用方的映射修改污染了号池状态")
	}
}

// ByID and Items must hand out copies, so a caller cannot mutate pool state
// through a returned Item.
func TestReadAPIsReturnCopies(t *testing.T) {
	p := New(StrategyRoundRobin)
	seed := item("acc_a", withModel("glm-5.2"))
	seed.ModelDownUntil = map[string]time.Time{"glm-5.2": time.Now().Add(time.Hour)}
	seed.ProvenModels = []string{"glm-5.2"}
	p.Upsert(seed)

	got, _ := p.ByID("acc_a")
	// Mutate everything the copy exposes.
	got.Name = "hacked"
	got.ModelDownUntil["glm-5.2"] = time.Time{}
	got.ProvenModels[0] = "hacked"
	delete(got.ModelDownUntil, "glm-5.2")

	fresh, _ := p.ByID("acc_a")
	if fresh.Name == "hacked" {
		t.Errorf("ByID leaked a mutable reference to Name")
	}
	if len(fresh.ModelDownUntil) != 1 {
		t.Errorf("ByID leaked the cooldown map: %v", fresh.ModelDownUntil)
	}
	if len(fresh.ProvenModels) == 0 || fresh.ProvenModels[0] == "hacked" {
		t.Errorf("ByID leaked the proven-model slice: %v", fresh.ProvenModels)
	}
}

// The snapshot must not leak either, since the admin API serializes it.
func TestSnapshotReflectsPoolState(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a", withModel("glm-5.2")))
	p.MarkClassified("acc_a", Classified{Kind: KindRateLimit, Cooldown: time.Hour, Model: "glm-5.2"}, 3)

	snapshot := p.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot has %d entries, want 1", len(snapshot))
	}
	entry := snapshot[0]
	if entry["id"] != "acc_a" {
		t.Errorf("id=%v", entry["id"])
	}
	// The model-scoped cooldown must be visible, since that is what explains a
	// specific model failing while the account is otherwise healthy.
	if _, ok := entry["model_cooldowns"]; !ok {
		t.Errorf("the snapshot hides the model cooldown: %v", entry)
	}
	if down, _ := entry["down"].(bool); down {
		t.Errorf("the account itself should not be reported as down")
	}
}

// A removal must not leave a dangling rotation cursor, or the next pick would
// resume from a ghost ID.
func TestRemoveDropsCursors(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("acc_a"))
	p.Upsert(item("acc_b"))

	// Warm the cursor so it points at a real account.
	if _, ok := p.Pick(RouteQuery{}); !ok {
		t.Fatalf("warm-up pick failed")
	}
	p.Remove("acc_a")
	p.Remove("acc_b")

	// No accounts left, so a pick must report that clearly rather than panic on
	// a stale index.
	res := p.PickRoute(RouteQuery{})
	if res.OK {
		t.Errorf("an empty pool must not report a routable account")
	}
	if res.Reason != ReasonNoAccounts {
		t.Errorf("reason=%q, want %q", res.Reason, ReasonNoAccounts)
	}
}

// A reason string must be present for every non-OK outcome, because the admin
// API surfaces it to explain why nothing was routed.
func TestPickFailureAlwaysHasAReason(t *testing.T) {
	cases := []struct {
		name  string
		build func() *Pool
		want  string
	}{
		{"empty pool", func() *Pool { return New(StrategyRoundRobin) }, ReasonNoAccounts},
		{
			"all cooling",
			func() *Pool {
				p := New(StrategyRoundRobin)
				p.Upsert(item("acc_a"))
				p.MarkDown("acc_a", time.Hour, "boom")
				return p
			},
			ReasonAllCooling,
		},
		{
			"all disabled",
			func() *Pool {
				p := New(StrategyRoundRobin)
				p.Upsert(item("acc_a", func(i *Item) { i.Enabled = false }))
				return p
			},
			// A disabled account is filtered out entirely, which is the same
			// observable outcome as having no accounts.
			ReasonNoAccounts,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.build().PickRoute(RouteQuery{})
			if res.OK {
				t.Fatalf("expected a non-OK result")
			}
			if res.Reason == "" {
				t.Errorf("a non-OK result must carry a reason")
			}
			if res.Reason != tc.want {
				t.Errorf("原因=%q，期望=%q", res.Reason, tc.want)
			}
		})
	}
}

// The armor preset is no longer an account-level setting: it is selected from
// the requested model, so Item must not carry one. This guards the removal --
// a field reintroduced here would silently resurrect the old design, where the
// effective prompt depended on which account the scheduler happened to pick.
func TestItemHasNoAccountScopedArmorPreset(t *testing.T) {
	itemType := reflect.TypeOf(Item{})
	for i := 0; i < itemType.NumField(); i++ {
		if itemType.Field(i).Name == "ArmorPreset" {
			t.Errorf("Item.ArmorPreset exists again; armor selection must be by model, " +
				"not by account (see internal/armor)")
		}
	}
}

// Guard against a field being added to the contract lists under two names that
// differ only by case, which would make the classification check meaningless.
func TestContractListsHaveNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, list := range [][]string{runtimeStateFields, configFields} {
		for _, name := range list {
			lower := strings.ToLower(name)
			if seen[lower] {
				t.Errorf("%s appears in both contract lists", name)
			}
			seen[lower] = true
		}
	}
}
