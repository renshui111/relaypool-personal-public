package pool

import "time"

// 更换凭证后仅清除认证类故障，保留真实额度或限流，避免误清冷却。
func (p *Pool) ResetCredentialHealth(id string) {
	p.mu.Lock()
	index, ok := p.indexed[id]
	if !ok {
		p.mu.Unlock()
		return
	}
	item := &p.items[index]
	item.Ready = nil
	item.CredentialRevision++
	if item.LastKind == KindAuth || item.LastKind == KindNotReady {
		item.DownUntil = time.Time{}
		item.LastError = ""
		item.LastKind = ""
		item.BackoffLevel = 0
	}
	item.StateVersion++
	snapshot := item.Clone()
	p.mu.Unlock()
	p.notify(snapshot)
}
