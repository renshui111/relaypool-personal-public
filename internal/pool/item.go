// Package pool owns account selection: which account serves a given request,
// when an account is unrouteable, and how failures cool an account down.
//
// Design note on the nil-vs-empty slice convention used throughout: a nil
// slice means "unknown" and routing FAILS OPEN (the filter is not applied),
// while a non-nil empty slice means "known to be empty" and routing filters
// everything out. Getting this backwards either kills all routing or lets
// unroutable requests through, so the distinction is load-bearing and every
// such field carries a comment.
package pool

import (
	"strings"
	"time"
)

// Routing strategies.
const (
	StrategyRoundRobin         = "round-robin"
	StrategyWeightedRoundRobin = "weighted-round-robin"
	StrategyFillFirst          = "fill-first"
)

// 每账号最多保留的模型故障记录；超过上限时转为账号冷却，保守控制内存。
const MaxModelFailureStates = 256

// NormalizeStrategy maps an arbitrary string onto a known strategy, defaulting
// to round-robin. An unknown value must not silently disable scheduling.
func NormalizeStrategy(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case StrategyFillFirst:
		return StrategyFillFirst
	case StrategyWeightedRoundRobin:
		return StrategyWeightedRoundRobin
	default:
		return StrategyRoundRobin
	}
}

// NormalizeProvider canonicalizes a provider family name.
func NormalizeProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return "workbuddy"
	}
	return provider
}

// NormalizeRegion canonicalizes a region name.
func NormalizeRegion(region string) string {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		return "cn"
	}
	return region
}

// 模型名替换规则在所有请求之间复用。
var modelIDReplacer = strings.NewReplacer("_", "-", " ", "-")

// CanonicalModelID 统一目录匹配和冷却键使用的大小写及分隔符。
func CanonicalModelID(model string) string {
	key := strings.ToLower(strings.TrimSpace(model))
	// 复用并发安全的替换器，避免每次模型匹配都重新分配匹配表。
	key = modelIDReplacer.Replace(key)
	return key
}

// ModelInfo describes one model advertised by one account.
type ModelInfo struct {
	// NativeModel is the exact ID the upstream expects in a request.
	NativeModel string
	// PublicModel is the ID clients may ask for.
	PublicModel string
	DisplayName string

	ContextWindow int
	MaxOutput     int
	Tools         bool
	Images        bool
	Reasoning     bool
}

// Item is one account's entry in the pool.
//
// Fields split into three groups: configuration mirrored from the store,
// runtime health observed by the executor, and per-model state that isolates
// one model's failures from the rest of the account.
type Item struct {
	ID       string
	Name     string
	Provider string
	Region   string

	// ---- configuration ----
	Enabled  bool
	Priority int
	// Weight biases weighted-round-robin. Equal weights degrade to plain
	// round-robin, which is the intended default for a homogeneous pool.
	Weight      int
	MaxInflight int
	// QuotaScope 显式区分共享、账号独立和模型独立的额度。
	QuotaScope     string
	QuotaGroup     string
	QuotaExpiresAt time.Time
	// DropSystemPrompt strips the caller's system message before relaying.
	DropSystemPrompt bool
	// AutoCheckin opts this account into the daily check-in job. It is mirrored
	// here so the scheduler can observe a runtime change without a store read,
	// which is what makes disabling it take effect immediately.
	AutoCheckin bool

	// ---- runtime health ----
	InFlight int
	// Ready is a tri-state: nil means unknown, which does NOT block routing.
	Ready     *bool
	DownUntil time.Time
	// 额度组冷却独立保存，单账号成功或更换凭证不能解除整组额度耗尽。
	GroupDownUntil time.Time
	LastError      string
	LastKind       string
	// BackoffLevel counts consecutive same-kind account-scoped failures. It
	// climbs the exponential ladder and resets to zero on success.
	BackoffLevel int

	// ---- per-model isolation ----
	// ModelDownUntil is a per-model cooldown. A quota or rate limit on one
	// model must not take the account offline for every other model.
	ModelDownUntil map[string]time.Time
	// ModelBackoff is the per-model backoff ladder.
	ModelBackoff map[string]int
	// ModelLastKind is the per-model previous failure kind. The ladder must
	// only climb when the SAME kind repeats, and that comparison has to be
	// scoped per model: a rate_limit on model A followed by an auth failure
	// on model B must not make a later auth on model A look like a repeat.
	ModelLastKind map[string]string

	// ---- catalog ----
	// Models is the last successful catalog snapshot. nil means unknown
	// (fail open); a non-nil slice, including an empty one, filters routing.
	Models   []string
	ModelsAt time.Time
	// ProvenModels are model IDs this account has actually served. A catalog
	// refresh must never drop one of these: upstream snapshots can omit a
	// live model, and trusting them blindly would strand that route.
	ProvenModels []string

	// StateVersion orders persistence snapshots. Not persisted itself.
	StateVersion uint64
	// RuntimeEpoch 区分同一 ID 删除后重建的账号，运行时分配，不写入数据库。
	RuntimeEpoch uint64
	// CredentialRevision 防止旧凭证请求的迟到结果覆盖新凭证状态。
	CredentialRevision uint64
}

// Clone returns a deep copy safe to hand outside the lock. The maps and slices
// are copied because callers must not observe concurrent mutation.
func (i Item) Clone() Item {
	out := i
	out.Models = cloneStrings(i.Models)
	out.ProvenModels = cloneStrings(i.ProvenModels)
	out.ModelDownUntil = cloneTimeMap(i.ModelDownUntil)
	out.ModelBackoff = cloneIntMap(i.ModelBackoff)
	out.ModelLastKind = cloneStringMap(i.ModelLastKind)
	if i.Ready != nil {
		ready := *i.Ready
		out.Ready = &ready
	}
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneTimeMap(in map[string]time.Time) map[string]time.Time {
	if in == nil {
		return nil
	}
	out := make(map[string]time.Time, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneIntMap(in map[string]int) map[string]int {
	if in == nil {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// RouteQuery selects candidates for one request. Empty fields are not
// filtered; Excluded is honoured before anything else.
type RouteQuery struct {
	// PublicModel is the model the client asked for. Empty or "auto" matches
	// any account regardless of catalog.
	PublicModel string
	// PreferAccount pins a specific account. When it is unavailable the pool
	// falls back to normal scheduling rather than failing the request.
	PreferAccount string
	// ProviderFilter restricts to one provider family.
	ProviderFilter string
	// RegionFilter restricts to one region.
	RegionFilter string
	// AllowedProviders, when non-empty, restricts to this set. It exists so
	// a request-scoped policy can narrow routing without changing the filter
	// semantics of ProviderFilter.
	AllowedProviders []string
	// AllowedAccounts 为 nil 表示不限，空集合禁止所有账号。
	AllowedAccounts []string
	// Excluded holds account IDs already tried during this request.
	Excluded map[string]struct{}
}

// routeModel returns the canonical model to match, or "" when the query
// accepts any model.
func routeModel(model string) string {
	id := CanonicalModelID(model)
	if id == "" || id == "auto" {
		return ""
	}
	return id
}

func itemHasCatalogModel(item Item, want string) bool {
	if want == "" || item.Models == nil {
		return false
	}
	for _, m := range item.Models {
		if CanonicalModelID(m) == want {
			return true
		}
	}
	return false
}

func itemHasProvenModel(item Item, want string) bool {
	if want == "" {
		return false
	}
	for _, m := range item.ProvenModels {
		if CanonicalModelID(m) == want {
			return true
		}
	}
	return false
}

// itemCouldServeModel reports whether an account is even a candidate for a
// model. Unknown catalogs fail open so a freshly added account is usable
// before its first catalog refresh completes.
func itemCouldServeModel(item Item, publicModel string) bool {
	want := routeModel(publicModel)
	if want == "" {
		return true
	}
	if item.Models == nil {
		return true
	}
	return itemHasCatalogModel(item, want) || itemHasProvenModel(item, want)
}

// itemDown reports whether the account is in an account-wide cooldown.
func itemDown(item Item, now time.Time) bool {
	return item.AccountDownUntil().After(now)
}

// AccountDownUntil 返回账号自身与共享额度组中较晚的恢复时间。
func (i Item) AccountDownUntil() time.Time {
	if i.GroupDownUntil.After(i.DownUntil) {
		return i.GroupDownUntil
	}
	return i.DownUntil
}

// itemModelDown reports whether this model is cooling down for this account.
func itemModelDown(item Item, q RouteQuery, now time.Time) bool {
	want := routeModel(q.PublicModel)
	if want == "" || item.ModelDownUntil == nil {
		return false
	}
	until, ok := item.ModelDownUntil[want]
	if !ok {
		return false
	}
	return until.After(now)
}

// itemSaturated reports whether the account has hit its concurrency cap. A
// non-positive MaxInflight means unlimited.
func itemSaturated(item Item) bool {
	if item.MaxInflight <= 0 {
		return false
	}
	return item.InFlight >= item.MaxInflight
}

// resumeAt is when the account becomes routable again, considering both the
// account-wide and the model-specific cooldown. Used to pick the "frees up
// soonest" account when nothing is available.
func resumeAt(item Item, q RouteQuery) time.Time {
	resume := time.Time{}
	if until := item.AccountDownUntil(); !until.IsZero() {
		resume = until
	}
	if want := routeModel(q.PublicModel); want != "" && item.ModelDownUntil != nil {
		if until, ok := item.ModelDownUntil[want]; ok {
			if resume.IsZero() || until.After(resume) {
				resume = until
			}
		}
	}
	return resume
}

// routeMatches applies the provider/region/allowed filters. A query with an
// empty filter does not constrain that dimension.
func routeMatches(item Item, q RouteQuery) bool {
	if q.AllowedAccounts != nil {
		found := false
		for _, id := range q.AllowedAccounts {
			if id == item.ID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if q.ProviderFilter != "" && NormalizeProvider(item.Provider) != NormalizeProvider(q.ProviderFilter) {
		return false
	}
	if q.RegionFilter != "" && NormalizeRegion(item.Region) != NormalizeRegion(q.RegionFilter) {
		return false
	}
	if len(q.AllowedProviders) > 0 {
		want := NormalizeProvider(item.Provider)
		ok := false
		for _, p := range q.AllowedProviders {
			if NormalizeProvider(p) == want {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if !item.Enabled {
		return false
	}
	if item.Ready != nil && !*item.Ready {
		return false
	}
	if _, skip := q.Excluded[item.ID]; skip {
		return false
	}
	return true
}

// 重试提示可以参考刚尝试过的账号，但禁用或未就绪的账号不会自动恢复。
func routeHintMatches(item Item, q RouteQuery) bool {
	q.Excluded = nil
	return routeMatches(item, q) && itemCouldServeModel(item, q.PublicModel)
}

func itemWeight(item Item) int {
	if item.Weight <= 0 {
		return 1
	}
	if item.Weight > 1000000 {
		return 1000000
	}
	return item.Weight
}

func itemRegion(item Item) string { return NormalizeRegion(item.Region) }
