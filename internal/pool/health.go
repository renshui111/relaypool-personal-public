package pool

import (
	"strings"
	"time"
)

// MarkOK records a successful request.
//
// Success clears the account-wide ladder AND the ladder for that specific
// model. Clearing only the account-wide one would leave a model permanently
// penalised by an earlier bad streak even after it started working again.
func (p *Pool) MarkOK(id, model string) {
	p.markOK(id, model, 0, false, 0, 0)
}

// MarkOKAtVersion 只在请求发出后的健康状态未变化时清除故障。
// 较早请求迟到的成功仍可证明模型可用，但不能撤销后来观察到的冷却。
func (p *Pool) MarkOKAtVersion(id, model string, version uint64) {
	p.markOK(id, model, version, true, 0, 0)
}

// 请求结果只能改变原来的账号实例，不能影响删除后重建的同名账号。
func (p *Pool) MarkAttemptOK(item Item, model string) {
	p.markOK(item.ID, model, item.StateVersion, true, item.RuntimeEpoch, item.CredentialRevision)
}

func (p *Pool) markOK(id, model string, version uint64, guardVersion bool, epoch, credentialRevision uint64) {
	p.mu.Lock()
	idx, ok := p.indexed[id]
	if !ok {
		p.mu.Unlock()
		return
	}
	item := &p.items[idx]
	if epoch > 0 && item.RuntimeEpoch != epoch {
		p.mu.Unlock()
		return
	}
	if epoch > 0 && item.CredentialRevision != credentialRevision {
		p.mu.Unlock()
		return
	}
	clearHealth := !guardVersion || item.StateVersion == version
	if clearHealth {
		// 成功请求才标记已验证；迟到结果不能覆盖后续故障或新凭证的状态。
		ready := true
		item.Ready = &ready
		item.LastError = ""
		item.LastKind = ""
		item.BackoffLevel = 0
		item.DownUntil = time.Time{}
	}
	item.StateVersion++

	if want := routeModel(model); want != "" {
		if clearHealth && item.ModelDownUntil != nil {
			delete(item.ModelDownUntil, want)
		}
		if clearHealth && item.ModelBackoff != nil {
			delete(item.ModelBackoff, want)
		}
		if clearHealth && item.ModelLastKind != nil {
			delete(item.ModelLastKind, want)
		}
		// Record the model as proven so a later catalog refresh cannot drop
		// an ID this account demonstrably serves.
		p.rememberProvenModel(item, want)
	}

	snapshot := item.Clone()
	p.mu.Unlock()
	p.notify(snapshot)
}

// rememberProvenModel adds a canonical model ID to the proven list. Callers
// must hold the lock.
func (p *Pool) rememberProvenModel(item *Item, canonical string) {
	if item == nil || canonical == "" {
		return
	}
	if itemHasProvenModel(*item, canonical) {
		return
	}
	item.ProvenModels = append(item.ProvenModels, canonical)
}

// MarkClassified applies a classified failure to an account.
//
// The three-way split is the core of the failure policy:
//
//   - Cooldown == 0  → the account is healthy, record nothing durable
//     (invalid_request, model_not_available).
//   - Model != ""    → cool only that model, leaving the account usable for
//     its other models.
//   - otherwise      → cool the account and climb the account-wide ladder.
func (p *Pool) MarkClassified(id string, c Classified, breakerThreshold int) {
	p.markClassified(id, c, breakerThreshold, 0, 0)
}

func (p *Pool) MarkAttemptClassified(item Item, c Classified, breakerThreshold int) {
	p.markClassified(item.ID, c, breakerThreshold, item.RuntimeEpoch, item.CredentialRevision)
}

func (p *Pool) markClassified(id string, c Classified, breakerThreshold int, epoch, credentialRevision uint64) {
	p.mu.Lock()
	idx, ok := p.indexed[id]
	if !ok {
		p.mu.Unlock()
		return
	}
	item := &p.items[idx]
	if epoch > 0 && item.RuntimeEpoch != epoch {
		p.mu.Unlock()
		return
	}
	if epoch > 0 && item.CredentialRevision != credentialRevision {
		p.mu.Unlock()
		return
	}

	// Capture the previous kind BEFORE overwriting it: the ladder must only
	// climb when the same failure kind repeats, so the comparison below needs
	// the old value. Reading it after the assignment would always compare the
	// kind against itself and the ladder would climb on every failure
	// regardless of cause.
	previousKind := item.LastKind

	item.LastError = c.Message
	item.LastKind = c.Kind
	item.StateVersion++

	modelKey := routeModel(c.Model)
	if modelKey != "" && c.Cooldown > 0 {
		if _, tracked := item.ModelBackoff[modelKey]; !tracked && len(item.ModelBackoff) >= MaxModelFailureStates {
			// 不丢弃尚未恢复的冷却；新模型故障改按账号范围处理。
			modelKey = ""
		}
	}

	// A zero cooldown means "healthy account, bad request". Recording a
	// cooldown here would take a working account out of rotation for reasons
	// that another account cannot fix either.
	if c.Cooldown <= 0 {
		// 无效请求不创建模型退避记录，避免任意名称长期占用内存。
		snapshot := item.Clone()
		p.mu.Unlock()
		p.notify(snapshot)
		return
	}

	cooldown := c.Cooldown

	if modelKey != "" {
		// Model-scoped cooldown. Only a REPEAT of the same kind climbs the
		// ladder, and the comparison is per model so unrelated failures on
		// other models cannot inflate it.
		if item.ModelDownUntil == nil {
			item.ModelDownUntil = map[string]time.Time{}
		}
		if item.ModelBackoff == nil {
			item.ModelBackoff = map[string]int{}
		}
		if item.ModelLastKind == nil {
			item.ModelLastKind = map[string]string{}
		}
		previousKind := item.ModelLastKind[modelKey]
		level := item.ModelBackoff[modelKey]
		if previousKind != c.Kind {
			// A new failure kind for this model restarts its ladder, so an
			// unrelated earlier streak cannot inflate this cooldown.
			level = 0
		}
		escalated, failures := p.failureCooldown(c, level, breakerThreshold)
		deadline := time.Now().Add(escalated)
		if deadline.After(item.ModelDownUntil[modelKey]) {
			item.ModelDownUntil[modelKey] = deadline
		}
		item.ModelBackoff[modelKey] = failures
		item.ModelLastKind[modelKey] = c.Kind

		// A model-scoped quota exhaustion is different: the whole account's
		// allowance is typically gone, so cool the account too rather than
		// letting other models hammer a dead account.
		if c.Kind == KindQuota && item.QuotaScope != "model" {
			deadline := time.Now().Add(cooldown)
			if deadline.After(item.DownUntil) {
				item.DownUntil = deadline
			}
		}

		var groupSnapshots []Item
		if c.Kind == KindQuota {
			groupSnapshots = p.coolQuotaGroupLocked(*item, item.DownUntil)
		}
		snapshot := item.Clone()
		p.mu.Unlock()
		p.notify(snapshot)
		for _, peer := range groupSnapshots {
			p.notify(peer)
		}
		return
	}

	// Account-wide cooldown.
	level := item.BackoffLevel
	if previousKind != "" && previousKind != c.Kind {
		// Kind changed: this is a new failure mode, so restart the ladder
		// rather than inheriting the previous streak's length.
		level = 0
	}
	escalated, failures := p.failureCooldown(c, level, breakerThreshold)
	deadline := time.Now().Add(escalated)
	if deadline.After(item.DownUntil) {
		item.DownUntil = deadline
	}
	item.BackoffLevel = failures

	var groupSnapshots []Item
	if c.Kind == KindQuota {
		groupSnapshots = p.coolQuotaGroupLocked(*item, item.DownUntil)
	}
	snapshot := item.Clone()
	p.mu.Unlock()
	p.notify(snapshot)
	for _, peer := range groupSnapshots {
		p.notify(peer)
	}
}

// SetBreakerCooldown 接入配置中的退避基数和上限，不使用跨实例全局变量。
func (p *Pool) SetBreakerCooldown(base, maximum time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if base > 0 {
		p.breakerBase = base
	}
	if maximum > 0 {
		p.breakerMax = maximum
	}
	if p.breakerBase > p.breakerMax {
		p.breakerBase = p.breakerMax
	}
}

// 阈值前只按错误类型冷却；达到阈值后才从配置基数开始翻倍。
// 额度按恢复时间处理，不参与指数退避。乘法前检查上限，防止溢出。
func (p *Pool) failureCooldown(c Classified, previous, threshold int) (time.Duration, int) {
	if threshold < 1 {
		threshold = 1
	}
	failures := previous + 1
	if previous < 0 {
		failures = 1
	}
	if failures > threshold+backoffMaxLevel {
		failures = threshold + backoffMaxLevel
	}
	if c.Kind == KindQuota || failures < threshold {
		return c.Cooldown, failures
	}
	duration := p.breakerBase
	for step := 0; step < failures-threshold; step++ {
		if duration >= p.breakerMax/2 {
			duration = p.breakerMax
			break
		}
		duration *= 2
	}
	if duration > p.breakerMax {
		duration = p.breakerMax
	}
	// 上游要求更长的等待时保留它，分类器已负责钳制不可信提示。
	if duration < c.Cooldown {
		duration = c.Cooldown
	}
	return duration, failures
}

// MarkDown applies a raw cooldown without classification. It exists for
// transport-level failures where no HTTP response is available at all.
func (p *Pool) MarkDown(id string, d time.Duration, errMsg string) {
	p.mu.Lock()
	idx, ok := p.indexed[id]
	if !ok {
		p.mu.Unlock()
		return
	}
	item := &p.items[idx]
	item.LastError = errMsg
	item.LastKind = KindUnavailable
	if d > 0 {
		item.DownUntil = time.Now().Add(d)
	}
	item.StateVersion++
	snapshot := item.Clone()
	p.mu.Unlock()
	p.notify(snapshot)
}

// CooldownScope reports the map key a cooldown occupies for this account and
// model: the model ID when the cooldown is model-scoped, else "" for the
// account-wide scope. Tests use it to assert isolation.
func (p *Pool) CooldownScope(item Item, publicModel string) string {
	now := time.Now()
	want := routeModel(publicModel)
	if want == "" || item.ModelDownUntil == nil {
		return ""
	}
	until, ok := item.ModelDownUntil[want]
	if !ok || !until.After(now) {
		return ""
	}
	if itemDown(item, now) {
		// The account is cooling too, so the effective scope is the account.
		return ""
	}
	return want
}

// RetryAfter reports how long until this account is routable for this model.
func (p *Pool) RetryAfter(item Item, publicModel string) time.Duration {
	now := time.Now()
	resume := resumeAt(item, RouteQuery{PublicModel: publicModel})
	if resume.IsZero() || !resume.After(now) {
		return 0
	}
	return resume.Sub(now)
}

// MergeHealth folds executor-reported runtime state into an account without
// touching configuration or cooldown state.
func (p *Pool) MergeHealth(id string, ready *bool, inFlight int, lastError string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, ok := p.indexed[id]
	if !ok {
		return
	}
	item := &p.items[idx]
	if ready != nil {
		value := *ready
		item.Ready = &value
	}
	item.InFlight = inFlight
	if lastError != "" {
		item.LastError = lastError
	}
	item.StateVersion++
}

// Acquire increments the in-flight counter, reporting whether the account had
// capacity. It is the admission control that keeps a single account from being
// stampeded by concurrent requests.
//
// Release must be called exactly once per successful Acquire.
func (p *Pool) Acquire(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, ok := p.indexed[id]
	if !ok {
		return false
	}
	item := &p.items[idx]
	if !item.Enabled || (item.Ready != nil && !*item.Ready) || itemDown(*item, time.Now()) || itemSaturated(*item) {
		return false
	}
	item.InFlight++
	return true
}

// AcquireTarget 原子获取指定账号和其健康版本，避免人工检查被调度到其他账号。
// 检查可越过旧故障，但仍遵守并发上限，也不会修改人工启停状态。
func (p *Pool) AcquireTarget(id, model string, probe bool) (Item, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, ok := p.indexed[id]
	if !ok {
		return Item{}, false
	}
	item := &p.items[idx]
	if itemSaturated(*item) {
		return Item{}, false
	}
	if !probe {
		if !item.Enabled || (item.Ready != nil && !*item.Ready) || itemDown(*item, time.Now()) {
			return Item{}, false
		}
		if until := item.ModelDownUntil[routeModel(model)]; until.After(time.Now()) {
			return Item{}, false
		}
	}
	item.InFlight++
	return item.Clone(), true
}

// Release decrements the in-flight counter.
func (p *Pool) Release(id string) {
	p.release(id, 0)
}

// ReleaseItem 防止旧请求归还名额时误减新建账号的并发数。
func (p *Pool) ReleaseItem(item Item) {
	p.release(item.ID, item.RuntimeEpoch)
}

func (p *Pool) release(id string, epoch uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, ok := p.indexed[id]
	if !ok {
		return
	}
	if p.items[idx].InFlight > 0 {
		if epoch > 0 && p.items[idx].RuntimeEpoch != epoch {
			return
		}
		p.items[idx].InFlight--
	}
}

// SweepExpired clears cooldowns that have elapsed.
//
// 清除已过期的期限并通知持久化；模型失败计数另有固定容量，成功后清除。
func (p *Pool) SweepExpired(now time.Time) []string {
	p.mu.Lock()

	var resumed []string
	var snapshots []Item
	for i := range p.items {
		item := &p.items[i]
		changed := false
		if !item.GroupDownUntil.IsZero() && !item.GroupDownUntil.After(now) {
			item.GroupDownUntil = time.Time{}
			changed = true
		}

		if !item.DownUntil.IsZero() && !item.DownUntil.After(now) {
			item.DownUntil = time.Time{}
			changed = true
		}
		for model, until := range item.ModelDownUntil {
			if !until.After(now) {
				delete(item.ModelDownUntil, model)
				// 等待结束不代表请求成功；保留失败计数，让下一次同类失败能触发阈值。
				changed = true
			}
		}
		if changed {
			item.StateVersion++
			snapshots = append(snapshots, item.Clone())
			if !itemDown(*item, now) && !p.modelHasCooldown(item, now) {
				resumed = append(resumed, item.ID)
			}
		}
	}
	p.mu.Unlock()
	// 冷却清扫也要更新持久化状态；回调必须在锁外执行。
	for _, snapshot := range snapshots {
		p.notify(snapshot)
	}
	return resumed
}

func (p *Pool) modelHasCooldown(item *Item, now time.Time) bool {
	for _, until := range item.ModelDownUntil {
		if until.After(now) {
			return true
		}
	}
	return false
}

// MergeModels installs a fresh catalog snapshot.
//
// Proven models are unioned in: an upstream catalog snapshot can omit a model
// that is demonstrably being served, and dropping it would strand that route.
func (p *Pool) MergeModels(id string, models []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, ok := p.indexed[id]
	if !ok {
		return
	}
	item := &p.items[idx]
	merged := make([]string, 0, len(models)+len(item.ProvenModels))
	seen := map[string]struct{}{}
	for _, m := range models {
		key := CanonicalModelID(m)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, m)
	}
	for _, m := range item.ProvenModels {
		key := CanonicalModelID(m)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, m)
	}
	item.Models = merged
	item.ModelsAt = time.Now()
	item.StateVersion++
}

// SyncFromStore reconciles the pool with the persisted account list.
//
// Accounts missing from the store are removed. Existing entries keep their
// runtime health (cooldowns, ladders, proven models) because Upsert preserves
// those fields; without that, every config refresh would silently clear an
// active cooldown and re-hammer a dead account.
func (p *Pool) SyncFromStore(items []Item) {
	want := make(map[string]struct{}, len(items))
	for _, item := range items {
		want[item.ID] = struct{}{}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// 一次同步只持锁一次，避免调度器看到账号删除和更新之间的半成品。
	for index := len(p.items) - 1; index >= 0; index-- {
		existing := p.items[index]
		if _, ok := want[existing.ID]; !ok {
			p.removeLocked(existing.ID)
		}
	}
	for _, item := range items {
		p.upsertLocked(item)
	}
}

// Snapshot renders the pool for the admin API.
func (p *Pool) Snapshot() []map[string]any {
	items := p.Items()
	now := time.Now()
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entry := map[string]any{
			"id":               item.ID,
			"name":             item.Name,
			"provider":         item.Provider,
			"region":           item.Region,
			"enabled":          item.Enabled,
			"weight":           item.Weight,
			"priority":         item.Priority,
			"in_flight":        item.InFlight,
			"max_inflight":     item.MaxInflight,
			"backoff_level":    item.BackoffLevel,
			"last_error":       item.LastError,
			"last_error_kind":  item.LastKind,
			"models":           item.Models,
			"models_at":        item.ModelsAt,
			"proven_models":    item.ProvenModels,
			"down":             itemDown(item, now),
			"quota_group":      item.QuotaGroup,
			"quota_expires_at": item.QuotaExpiresAt,
		}
		if item.Ready != nil {
			entry["ready"] = *item.Ready
		}
		if deadline := item.AccountDownUntil(); deadline.After(now) {
			entry["cooldown_until"] = deadline
			entry["retry_after_seconds"] = int(deadline.Sub(now).Seconds())
		}
		// Surface only models still cooling, which is what an operator
		// actually needs to explain "why is this model failing".
		cooling := map[string]string{}
		for model, until := range item.ModelDownUntil {
			if until.After(now) {
				cooling[model] = until.UTC().Format(time.RFC3339)
			}
		}
		if len(cooling) > 0 {
			entry["model_cooldowns"] = cooling
		}
		out = append(out, entry)
	}
	return out
}

// ParseModelList splits a comma or whitespace separated model list, which is
// how operator-provided catalogs are usually expressed.
func ParseModelList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if trimmed := strings.TrimSpace(f); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
