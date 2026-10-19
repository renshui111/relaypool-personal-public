package pool

import (
	"testing"
	"time"
)

func TestCompleteQuotaGroupIsolation(t *testing.T) {
	p := New(StrategyRoundRobin)
	for _, account := range []Item{
		{ID: "a", Enabled: true, Provider: "workbuddy", Region: "cn", QuotaScope: "shared", QuotaGroup: "项目一"},
		{ID: "b", Enabled: true, Provider: "workbuddy", Region: "cn", QuotaScope: "shared", QuotaGroup: "项目一"},
		{ID: "c", Enabled: true, Provider: "workbuddy", Region: "global", QuotaScope: "shared", QuotaGroup: "项目一"},
		{ID: "d", Enabled: true, Provider: "workbuddy", Region: "cn", QuotaScope: "account", QuotaGroup: "项目一"},
	} {
		p.Upsert(account)
	}
	p.MarkClassified("a", Classified{Kind: KindQuota, Cooldown: time.Minute}, 3)
	a, _ := p.ByID("a")
	b, _ := p.ByID("b")
	c, _ := p.ByID("c")
	d, _ := p.ByID("d")
	if !itemDown(a, time.Now()) || !itemDown(b, time.Now()) || itemDown(c, time.Now()) || itemDown(d, time.Now()) {
		t.Fatal("额度组没有按渠道、地区、范围隔离")
	}
	p.MarkOK("b", "m")
	b, _ = p.ByID("b")
	if !itemDown(b, time.Now()) {
		t.Fatal("旧成功结果解除组冷却")
	}
	p.Upsert(Item{ID: "new", Enabled: true, Provider: "workbuddy", Region: "cn", QuotaScope: "shared", QuotaGroup: "项目一"})
	added, _ := p.ByID("new")
	if !itemDown(added, time.Now()) {
		t.Fatal("新账号绕过组冷却")
	}
	b.QuotaGroup = "项目二"
	p.Upsert(b)
	b, _ = p.ByID("b")
	if itemDown(b, time.Now()) {
		t.Fatal("修改额度组未清除旧组期限")
	}
	p.SweepExpired(time.Now().Add(2 * time.Minute))
	added, _ = p.ByID("new")
	if itemDown(added, time.Now()) {
		t.Fatal("组冷却未清扫")
	}
}

func TestCompleteQuotaExpiryPreference(t *testing.T) {
	p := New(StrategyRoundRobin)
	now := time.Now()
	p.SetExpiryPreference(true, 24*time.Hour)
	p.Upsert(Item{ID: "a", Enabled: true, QuotaExpiresAt: now.Add(12 * time.Hour)})
	p.Upsert(Item{ID: "b", Enabled: true, QuotaExpiresAt: now.Add(time.Hour)})
	p.Upsert(Item{ID: "c", Enabled: true, QuotaExpiresAt: now.Add(-time.Hour)})
	if selected := p.PickRoute(RouteQuery{}); !selected.OK || selected.Item.ID != "b" {
		t.Fatal("未优先使用真实即将到期额度")
	}
	b, _ := p.ByID("b")
	b.Priority = -1
	p.Upsert(b)
	if selected := p.PickRoute(RouteQuery{}); selected.Item.ID != "a" {
		t.Fatal("到期优先覆盖了账号主备优先级")
	}
	p.SetExpiryPreference(false, 24*time.Hour)
	seenC := false
	for i := 0; i < 3; i++ {
		if p.PickRoute(RouteQuery{}).Item.ID == "c" {
			seenC = true
		}
	}
	if !seenC {
		t.Fatal("关闭到期优先后未恢复原调度")
	}
}
