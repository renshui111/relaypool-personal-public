package pool

import "time"

// 同一供应商、地区、显式额度组才共享额度，未填写组名时保留旧行为。
func sameQuotaGroup(a, b Item) bool {
	return a.QuotaScope == "shared" && b.QuotaScope == "shared" && a.QuotaGroup != "" &&
		a.QuotaGroup == b.QuotaGroup && a.Provider == b.Provider && a.Region == b.Region
}

// 必须在号池锁内调用；独立的组期限避免单账号成功解除组冷却。
func (p *Pool) coolQuotaGroupLocked(source Item, deadline time.Time) []Item {
	var snapshots []Item
	for i := range p.items {
		peer := &p.items[i]
		if sameQuotaGroup(source, *peer) && deadline.After(peer.GroupDownUntil) {
			peer.GroupDownUntil = deadline
			peer.StateVersion++
			snapshots = append(snapshots, peer.Clone())
		}
	}
	return snapshots
}

// SetExpiryPreference 启用真实额度到期优先，不读取访问令牌的过期时间。
func (p *Pool) SetExpiryPreference(enabled bool, window time.Duration) {
	p.mu.Lock()
	p.preferExpiring, p.expiringSoon = enabled, window
	p.mu.Unlock()
}

func (p *Pool) expiringCandidates(items []*Item, now time.Time) []*Item {
	if !p.preferExpiring || p.expiringSoon <= 0 {
		return items
	}
	var earliest time.Time
	for _, item := range items {
		expiry := item.QuotaExpiresAt
		if expiry.After(now) && !expiry.After(now.Add(p.expiringSoon)) && (earliest.IsZero() || expiry.Before(earliest)) {
			earliest = expiry
		}
	}
	if earliest.IsZero() {
		return items
	}
	selected := items[:0]
	for _, item := range items {
		if item.QuotaExpiresAt.Equal(earliest) {
			selected = append(selected, item)
		}
	}
	return selected
}
