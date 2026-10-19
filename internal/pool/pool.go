package pool

import (
	"container/list"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pool is the in-memory account registry and scheduler.
//
// All mutable state sits behind one mutex. The critical sections only compute
// a choice from already-loaded values, so the lock is held for microseconds;
// per-account I/O happens outside it. A finer-grained design would not pay for
// itself at this scale and would make the rotation cursor much harder to
// reason about.
type Pool struct {
	mu    sync.Mutex
	items []Item
	// indexed maps ID to position in items so lookups stay O(1).
	indexed map[string]int

	strategy       string
	breakerBase    time.Duration
	breakerMax     time.Duration
	preferExpiring bool
	expiringSoon   time.Duration
	epoch          uint64

	// lastPicked and lastRegion key the rotation cursor and region affinity
	// per route, so different models/providers rotate independently. The key
	// is derived by rotationKey.
	lastPicked map[string]string
	lastRegion map[string]string
	// weightCounter holds the smooth-weighted-round-robin running totals.
	weightCounter map[string]map[string]int64
	// 路由状态采用最近使用淘汰，防止任意模型名持续扩大内存占用。
	routeUsage map[string]*list.Element
	routeLRU   list.List

	// observer, when set, is invoked with a copy of an item after a mutation
	// that should be persisted. It runs WITHOUT the lock held.
	observer func(Item)
}

// MaxRouteStates 是同时保留的模型、渠道及地区组合数量。
const MaxRouteStates = 4096

// New creates an empty pool.
func New(strategy string) *Pool {
	return &Pool{
		indexed:       map[string]int{},
		strategy:      NormalizeStrategy(strategy),
		breakerBase:   backoffFloor,
		breakerMax:    backoffCeiling,
		lastPicked:    map[string]string{},
		lastRegion:    map[string]string{},
		weightCounter: map[string]map[string]int64{},
		routeUsage:    map[string]*list.Element{},
	}
}

// SetObserver installs a persistence hook. It is called after the lock is
// released so a slow store cannot stall scheduling.
func (p *Pool) SetObserver(observer func(Item)) {
	p.mu.Lock()
	p.observer = observer
	p.mu.Unlock()
}

func (p *Pool) notify(item Item) {
	p.mu.Lock()
	index, exists := p.indexed[item.ID]
	if !exists || p.items[index].RuntimeEpoch != item.RuntimeEpoch {
		p.mu.Unlock()
		return
	}
	observer := p.observer
	p.mu.Unlock()
	if observer != nil {
		observer(item)
	}
}

// SetStrategy switches the routing strategy.
func (p *Pool) SetStrategy(strategy string) {
	p.mu.Lock()
	p.strategy = NormalizeStrategy(strategy)
	// Drop the weighted counters: they belong to the previous strategy and
	// would otherwise skew the first picks after a switch.
	p.weightCounter = map[string]map[string]int64{}
	p.mu.Unlock()
}

// Strategy reports the active routing strategy.
func (p *Pool) Strategy() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.strategy
}

// Len reports the number of accounts, excluding disabled ones when onlyReady.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.items)
}

// LenRoute reports how many accounts could possibly serve the query, ignoring
// cooldown. It is used to size the failover attempt budget.
func (p *Pool) LenRoute(q RouteQuery) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, item := range p.items {
		if routeMatches(item, q) && itemCouldServeModel(item, q.PublicModel) {
			count++
		}
	}
	return count
}

// Upsert inserts or replaces an account, preserving runtime health across the
// update so a config edit does not erase an active cooldown.
func (p *Pool) Upsert(item Item) {
	p.mu.Lock()
	previous := time.Time{}
	if index, ok := p.indexed[item.ID]; ok {
		previous = p.items[index].GroupDownUntil
	}
	p.upsertLocked(item)
	current := p.items[p.indexed[item.ID]]
	var snapshot *Item
	if !previous.Equal(current.GroupDownUntil) {
		copy := current.Clone()
		snapshot = &copy
	}
	p.mu.Unlock()
	if snapshot != nil {
		p.notify(*snapshot)
	}
}

// 复制调用方数据并保持账号按 ID 排序，把排序开销移出请求热路径。
func (p *Pool) upsertLocked(item Item) {
	item = item.Clone()
	item.Provider = NormalizeProvider(item.Provider)
	item.Region = NormalizeRegion(item.Region)
	if idx, ok := p.indexed[item.ID]; ok {
		existing := p.items[idx]
		if item.Weight != existing.Weight {
			// 改权重后旧累计值不再适用，避免短时间内沿用旧比例。
			p.weightCounter = map[string]map[string]int64{}
		}
		// Health and per-model state live in the pool, not in the caller's
		// payload, so they survive a configuration refresh.
		item.DownUntil = existing.DownUntil
		if item.QuotaGroup == existing.QuotaGroup && item.QuotaScope == existing.QuotaScope && item.Provider == existing.Provider && item.Region == existing.Region {
			item.GroupDownUntil = existing.GroupDownUntil
		} else {
			item.GroupDownUntil = time.Time{}
		}
		item.LastError = existing.LastError
		item.LastKind = existing.LastKind
		item.BackoffLevel = existing.BackoffLevel
		item.ModelDownUntil = existing.ModelDownUntil
		item.ModelBackoff = existing.ModelBackoff
		item.ModelLastKind = existing.ModelLastKind
		item.InFlight = existing.InFlight
		item.Ready = existing.Ready
		// A catalog refresh from the store must not clobber a live snapshot.
		if item.Models == nil {
			item.Models = existing.Models
			item.ModelsAt = existing.ModelsAt
		}
		item.ProvenModels = existing.ProvenModels
		item.StateVersion = existing.StateVersion + 1
		item.RuntimeEpoch = existing.RuntimeEpoch
		item.CredentialRevision = existing.CredentialRevision
		p.items[idx] = item
	} else {
		p.epoch++
		item.RuntimeEpoch = p.epoch
		item.StateVersion = 1
		idx := sort.Search(len(p.items), func(i int) bool { return p.items[i].ID >= item.ID })
		p.items = append(p.items, Item{})
		copy(p.items[idx+1:], p.items[idx:])
		p.items[idx] = item
		for i := idx; i < len(p.items); i++ {
			p.indexed[p.items[i].ID] = i
		}
	}
	// 新加入同一额度组的账号继承尚未到期的组冷却。
	if current, ok := p.indexed[item.ID]; ok {
		for _, peer := range p.items {
			if sameQuotaGroup(p.items[current], peer) && peer.GroupDownUntil.After(p.items[current].GroupDownUntil) {
				p.items[current].GroupDownUntil = peer.GroupDownUntil
			}
		}
	}
}

// Remove deletes an account and its rotation cursors.
func (p *Pool) Remove(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removeLocked(id)
}

func (p *Pool) removeLocked(id string) {
	idx, ok := p.indexed[id]
	if !ok {
		return
	}
	copy(p.items[idx:], p.items[idx+1:])
	p.items[len(p.items)-1] = Item{}
	p.items = p.items[:len(p.items)-1]
	delete(p.indexed, id)
	// Rebuild the index: every entry after idx shifted by one.
	for i := idx; i < len(p.items); i++ {
		p.indexed[p.items[i].ID] = i
	}
	p.dropRotationCursor(id)
	if len(p.items) == 0 {
		p.lastPicked = map[string]string{}
		p.lastRegion = map[string]string{}
		p.weightCounter = map[string]map[string]int64{}
		p.routeUsage = map[string]*list.Element{}
		p.routeLRU.Init()
	}
}

// dropRotationCursor forgets any cursor pointing at a removed account so the
// successor search does not resume from a ghost ID.
func (p *Pool) dropRotationCursor(id string) {
	for cursorKey, picked := range p.lastPicked {
		if picked == id {
			delete(p.lastPicked, cursorKey)
		}
	}
	for cursorKey, counters := range p.weightCounter {
		delete(counters, id)
		_ = cursorKey
	}
}

// ByID returns a copy of one account.
func (p *Pool) ByID(id string) (Item, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, ok := p.indexed[id]
	if !ok {
		return Item{}, false
	}
	return p.items[idx].Clone(), true
}

// Items returns copies of every account.
func (p *Pool) Items() []Item {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Item, 0, len(p.items))
	for _, item := range p.items {
		out = append(out, item.Clone())
	}
	return out
}

// PickResult is the outcome of one scheduling decision.
//
// The two return values of a naive (Item, bool) API are ambiguous: a caller
// receiving false plus a zero Item cannot tell "no accounts at all" from "all
// accounts cooling, retry at T". Making the hint a separate field removes that
// ambiguity and stops a caller from treating a hint as a routable account.
type PickResult struct {
	// Item is the account to use. It is only valid when OK is true.
	Item Item
	// OK reports that Item is routable right now.
	OK bool
	// RetryAt is when a cooling account becomes routable again. It is only
	// meaningful when OK is false and RetryAt is non-zero.
	RetryAt time.Time
	// Reason explains a non-OK outcome: "no_accounts", "all_cooling" or
	// "all_saturated". Callers use it to choose between retrying and failing
	// fast, which is the whole point of distinguishing the two.
	Reason string
}

// Pick reasons.
const (
	ReasonNoAccounts   = "no_accounts"
	ReasonAllCooling   = "all_cooling"
	ReasonAllSaturated = "all_saturated"
)

// PickRoute chooses one account for the query.
//
// See PickResult for why this does not return a bare (Item, bool).
func (p *Pool) PickRoute(q RouteQuery) PickResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pickRouteLocked(q)
}

// AcquireRoute 在同一把锁内完成选择和占位，成功后必须调用一次 Release。
func (p *Pool) AcquireRoute(q RouteQuery) PickResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := p.pickRouteLocked(q)
	if result.OK {
		p.items[p.indexed[result.Item.ID]].InFlight++
		result.Item.InFlight++
	}
	return result
}

func (p *Pool) pickRouteLocked(q RouteQuery) PickResult {

	if len(p.items) == 0 {
		return PickResult{Reason: ReasonNoAccounts}
	}
	now := time.Now()
	regionKey := rotationKey(q)

	// A pinned account wins outright when it is healthy. When it is not, fall
	// through to normal scheduling rather than failing: a stale pin from a
	// client must not break the request.
	if q.PreferAccount != "" {
		if idx, ok := p.indexed[q.PreferAccount]; ok {
			pinned := p.items[idx]
			if routeMatches(pinned, q) && itemCouldServeModel(pinned, q.PublicModel) &&
				!itemDown(pinned, now) && !itemModelDown(pinned, q, now) && !itemSaturated(pinned) {
				p.touchRoute(regionKey)
				p.lastPicked[regionKey] = pinned.ID
				return PickResult{Item: pinned.Clone(), OK: true}
			}
		}
	}

	// Collect candidates that can serve right now.
	// 只暂存指针，避免复制每个账号的完整状态；锁内指针不会失效。
	available := make([]*Item, 0, len(p.items))
	var highestPriority int
	for i := range p.items {
		item := &p.items[i]
		if !routeMatches(*item, q) || !itemCouldServeModel(*item, q.PublicModel) {
			continue
		}
		if itemDown(*item, now) || itemModelDown(*item, q, now) || itemSaturated(*item) {
			continue
		}
		// 数值越大优先级越高；主账号不可用时自然降到备用层。
		if len(available) == 0 || item.Priority > highestPriority {
			available = available[:0]
			highestPriority = item.Priority
		}
		if item.Priority < highestPriority {
			continue
		}
		available = append(available, item)
	}

	if len(available) == 0 {
		return p.pickUnavailable(q, now)
	}

	// 账号注册时已排序，筛选不会改变顺序，无需每个请求重新排序。

	// regionKey scopes region affinity and the rotation cursor per route, so
	// different models rotate independently and a pinned-account request does
	// not disturb the cursor unpinned requests rely on.
	p.touchRoute(regionKey)

	// Region affinity. When the caller did not pin a region, keep serving the
	// region this route used last so a single conversation does not flip
	// between CN and Global upstreams as rotation advances. On a cold route
	// there is no history: choose the region with the most candidates rather
	// than letting the alphabetically-first account capture the route.
	if q.RegionFilter == "" {
		if previous, ok := p.lastRegion[regionKey]; ok {
			if subset := inRegion(available, previous); len(subset) > 0 {
				available = subset
			}
		} else if preferred := largestRegion(available); preferred != "" {
			p.lastRegion[regionKey] = preferred
			if subset := inRegion(available, preferred); len(subset) > 0 {
				available = subset
			}
		}
	}

	available = p.expiringCandidates(available, now)
	picked := p.selectItem(available, regionKey)
	p.lastPicked[regionKey] = picked.ID
	p.lastRegion[regionKey] = itemRegion(picked)
	return PickResult{Item: picked.Clone(), OK: true}
}

// pickUnavailable handles the "nothing routable" case, which has two distinct
// causes that must not be conflated: every candidate cooling, versus every
// candidate merely saturated. The first deserves a retry hint; the second
// means the caller should shed load instead of waiting for a deadline that
// does not exist.
func (p *Pool) pickUnavailable(q RouteQuery, now time.Time) PickResult {
	var best Item
	found := false
	saturated := false
	for _, item := range p.items {
		if !routeHintMatches(item, q) {
			continue
		}
		// Only cooling accounts qualify for the hint. A merely saturated
		// account is a concurrency problem, not a cooldown problem.
		if !itemDown(item, now) && !itemModelDown(item, q, now) {
			if _, excluded := q.Excluded[item.ID]; !excluded && itemSaturated(item) {
				saturated = true
			}
			continue
		}
		if !found || resumeAt(item, q).Before(resumeAt(best, q)) {
			best = item
			found = true
		}
	}
	// 有满载账号时可能随时释放名额，不能误报一个固定恢复时间。
	if saturated {
		return PickResult{Reason: ReasonAllSaturated}
	}
	if !found {
		return PickResult{Reason: ReasonNoAccounts}
	}
	return PickResult{OK: false, RetryAt: resumeAt(best, q), Reason: ReasonAllCooling}
}

// Pick is the compatibility shim returning the account plus a routability
// flag. Prefer PickRoute where the retry hint matters.
func (p *Pool) Pick(q RouteQuery) (Item, bool) {
	// PickRoute takes the lock itself, so this must not hold it.
	res := p.PickRoute(q)
	if res.OK {
		return res.Item, true
	}
	// Surface the soonest-recovering account so a legacy caller can still
	// build a retry-after, matching the documented behaviour.
	if res.Reason == ReasonAllCooling {
		return p.soonestCooling(q), false
	}
	return Item{}, false
}

// soonestCooling returns a copy of the cooling account that recovers first.
func (p *Pool) soonestCooling(q RouteQuery) Item {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var best Item
	found := false
	for _, item := range p.items {
		if !routeHintMatches(item, q) {
			continue
		}
		if !itemDown(item, now) && !itemModelDown(item, q, now) {
			continue
		}
		if !found || resumeAt(item, q).Before(resumeAt(best, q)) {
			best = item
			found = true
		}
	}
	if !found {
		return Item{}
	}
	return best.Clone()
}

// selectItem applies the configured strategy to the candidate set.
func (p *Pool) selectItem(available []*Item, key string) Item {
	switch p.strategy {
	case StrategyFillFirst:
		return pickFillFirst(available)
	case StrategyWeightedRoundRobin:
		if picked, ok := p.pickWeighted(available, key); ok {
			return picked
		}
		// Uniform weights make weighted identical to plain round-robin, and
		// the ID cursor is more stable across a changing candidate set.
		return *available[successorIndex(available, p.lastPicked[key])]
	default:
		return *available[successorIndex(available, p.lastPicked[key])]
	}
}

// pickFillFirst always prefers the highest weight, breaking ties by ID.
func pickFillFirst(available []*Item) Item {
	picked := available[0]
	for _, item := range available[1:] {
		if itemWeight(*item) > itemWeight(*picked) ||
			(itemWeight(*item) == itemWeight(*picked) && item.ID < picked.ID) {
			picked = item
		}
	}
	return *picked
}

// pickWeighted runs smooth weighted round-robin.
//
// It reports false when every candidate shares one weight, leaving plain
// round-robin in charge: with a uniform pool the two are equivalent, and the
// ID cursor is the more stable choice.
//
// Smooth WRR (as opposed to weighted random) is what keeps a high-weight
// account from being burst: each candidate accumulates its weight, the largest
// wins, and the winner pays back the total.
func (p *Pool) pickWeighted(available []*Item, key string) (Item, bool) {
	if len(available) == 0 {
		return Item{}, false
	}
	total := int64(0)
	uniform := true
	first := itemWeight(*available[0])
	for _, item := range available {
		w := int64(itemWeight(*item))
		total += w
		if w != int64(first) {
			uniform = false
		}
	}
	if uniform || total <= 0 {
		return Item{}, false
	}
	if p.weightCounter[key] == nil {
		p.weightCounter[key] = map[string]int64{}
	}
	counters := p.weightCounter[key]

	best := -1
	var bestCurrent int64
	for i := range available {
		current := counters[available[i].ID] + int64(itemWeight(*available[i]))
		counters[available[i].ID] = current
		if best < 0 || current > bestCurrent {
			best = i
			bestCurrent = current
		}
	}
	if best < 0 {
		return Item{}, false
	}
	counters[available[best].ID] -= total
	return *available[best], true
}

// successorIndex returns the first position ordered after lastID, wrapping to
// the head. Candidates are sorted by ID, so this resumes rotation at the
// account following the previous pick even when candidates were filtered out
// in between. An empty or unknown lastID starts at the head.
func successorIndex(available []*Item, lastID string) int {
	if lastID == "" {
		return 0
	}
	index := sort.Search(len(available), func(i int) bool { return available[i].ID > lastID })
	if index >= len(available) {
		return 0
	}
	return index
}

func inRegion(items []*Item, region string) []*Item {
	// 原地筛选候选指针，不再分配第二份账号列表。
	out := items[:0]
	for _, item := range items {
		if itemRegion(*item) == region {
			out = append(out, item)
		}
	}
	return out
}

// largestRegion reports the region with the most candidates, breaking ties by
// region name so the choice is deterministic.
func largestRegion(items []*Item) string {
	counts := map[string]int{}
	for _, item := range items {
		counts[itemRegion(*item)]++
	}
	best := ""
	bestCount := 0
	for region, count := range counts {
		if count > bestCount || (count == bestCount && region < best) {
			best, bestCount = region, count
		}
	}
	return best
}

// rotationKey scopes the rotation cursor and region affinity.
//
// It deliberately keys on the provider/region filters and the model rather than
// on the whole query, so a pinned-account request does not disturb the cursor
// that unpinned requests rely on.
func rotationKey(q RouteQuery) string {
	parts := []string{
		strings.ToLower(strings.TrimSpace(q.ProviderFilter)),
		strings.ToLower(strings.TrimSpace(q.RegionFilter)),
		routeModel(q.PublicModel),
	}
	allowed := make([]string, 0, len(q.AllowedProviders))
	for _, provider := range q.AllowedProviders {
		allowed = append(allowed, NormalizeProvider(provider))
	}
	sort.Strings(allowed)
	for i, provider := range allowed {
		if i == 0 || provider != allowed[i-1] {
			parts = append(parts, provider)
		}
	}
	if q.AllowedAccounts != nil {
		parts = append(parts, "account-policy")
		ids := append([]string{}, q.AllowedAccounts...)
		sort.Strings(ids)
		parts = append(parts, ids...)
	}
	// 引号编码防止模型名或渠道名里的分隔符造成键冲突。
	var key strings.Builder
	for _, part := range parts {
		key.WriteString(strconv.Quote(part))
	}
	return key.String()
}

// 调用方持锁；所有关联游标一起淘汰，避免只清理其中一张表。
func (p *Pool) touchRoute(key string) {
	if entry, ok := p.routeUsage[key]; ok {
		p.routeLRU.MoveToFront(entry)
		return
	}
	if len(p.routeUsage) >= MaxRouteStates {
		oldest := p.routeLRU.Back()
		oldKey := oldest.Value.(string)
		delete(p.lastPicked, oldKey)
		delete(p.lastRegion, oldKey)
		delete(p.weightCounter, oldKey)
		delete(p.routeUsage, oldKey)
		p.routeLRU.Remove(oldest)
	}
	p.routeUsage[key] = p.routeLRU.PushFront(key)
}

// SetMaxInflight updates one account's concurrency cap.
func (p *Pool) SetMaxInflight(id string, max int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, ok := p.indexed[id]
	if !ok {
		return
	}
	p.items[idx].MaxInflight = max
	p.items[idx].StateVersion++
}

// SetWeight updates one account's scheduling weight.
func (p *Pool) SetWeight(id string, weight int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, ok := p.indexed[id]
	if !ok {
		return
	}
	if weight < 0 {
		weight = 0
	}
	p.items[idx].Weight = weight
	p.items[idx].StateVersion++
}
