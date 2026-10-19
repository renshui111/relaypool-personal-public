package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// 外部配置的切片、映射和指针不能继续修改号池内部状态。
func TestOptimizationUpsertOwnsInput(t *testing.T) {
	p := New(StrategyRoundRobin)
	ready := true
	until := time.Now().Add(time.Hour)
	seed := item("acc_a", withModel("model-a"))
	seed.Ready = &ready
	seed.ProvenModels = []string{"model-b"}
	seed.ModelDownUntil = map[string]time.Time{"model-a": until}
	seed.ModelBackoff = map[string]int{"model-a": 2}
	seed.ModelLastKind = map[string]string{"model-a": KindRateLimit}
	p.Upsert(seed)
	seed.Models[0] = "changed"
	seed.ProvenModels[0] = "changed"
	seed.ModelDownUntil["model-a"] = time.Time{}
	seed.ModelBackoff["model-a"] = 99
	seed.ModelLastKind["model-a"] = KindAuth
	ready = false
	stored, _ := p.ByID("acc_a")
	if stored.Models[0] != "model-a" || stored.ProvenModels[0] != "model-b" ||
		!stored.ModelDownUntil["model-a"].Equal(until) || stored.ModelBackoff["model-a"] != 2 ||
		stored.ModelLastKind["model-a"] != KindRateLimit || !*stored.Ready {
		t.Fatal("调用方修改了已提交的数据，号池内部状态也随之改变")
	}
	refresh := item("acc_a", withModel("model-c"))
	p.Upsert(refresh)
	refresh.Models[0] = "changed"
	stored, _ = p.ByID("acc_a")
	if stored.Models[0] != "model-c" {
		t.Fatal("更新账号时没有复制调用方的模型目录")
	}
}

// 无账号、冷却和满载是不同情况，不能让客户端错误地继续重试。
func TestOptimizationUnavailableReasons(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  Item
		query RouteQuery
		want  string
	}{
		{"全部禁用", item("a", func(i *Item) { i.Enabled = false; i.DownUntil = time.Now().Add(time.Hour) }), RouteQuery{}, ReasonNoAccounts},
		{"模型不匹配", item("a", withModel("model-a"), func(i *Item) { i.DownUntil = time.Now().Add(time.Hour) }), RouteQuery{PublicModel: "model-b"}, ReasonNoAccounts},
		{"渠道不匹配", item("a"), RouteQuery{ProviderFilter: "another"}, ReasonNoAccounts},
		{"明确未就绪", item("a", func(i *Item) { ready := false; i.Ready = &ready }), RouteQuery{}, ReasonNoAccounts},
		{"全部排除", item("a"), RouteQuery{Excluded: map[string]struct{}{"a": {}}}, ReasonNoAccounts},
		{"真实满载", item("a", func(i *Item) { i.InFlight = i.MaxInflight }), RouteQuery{}, ReasonAllSaturated},
		{"真实冷却", item("a", func(i *Item) { i.DownUntil = time.Now().Add(time.Hour) }), RouteQuery{}, ReasonAllCooling},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(StrategyRoundRobin)
			p.Upsert(tc.seed)
			got := p.PickRoute(tc.query)
			if got.OK || got.Reason != tc.want {
				t.Fatalf("选择结果=%+v，期望原因=%s", got, tc.want)
			}
			if tc.want != ReasonAllCooling && !got.RetryAt.IsZero() {
				t.Fatal("无可恢复账号时不应返回冷却时间")
			}
		})
	}
}

func TestOptimizationRouteStateIsolation(t *testing.T) {
	base := RouteQuery{PublicModel: "model-a"}
	for _, other := range []RouteQuery{
		{PublicModel: "model-a", ProviderFilter: "workbuddy"},
		{PublicModel: "model-a", RegionFilter: "cn"},
		{PublicModel: "model-a", AllowedProviders: []string{"workbuddy"}},
		{PublicModel: "model-a", AllowedProviders: []string{"official"}},
	} {
		if rotationKey(base) == rotationKey(other) {
			t.Fatalf("不同的路由策略共享了游标：%+v", other)
		}
	}
	a := RouteQuery{PublicModel: "MODEL_A", AllowedProviders: []string{" Official ", "workbuddy", "official"}}
	b := RouteQuery{PublicModel: "model-a", AllowedProviders: []string{"workbuddy", "official"}}
	if rotationKey(a) != rotationKey(b) {
		t.Fatal("相同允许渠道集合应共享路由状态，不受排列、大小写或重复项影响")
	}
}

func TestOptimizationPriorityFallsBack(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("a_backup", func(i *Item) { i.Priority = 10 }))
	p.Upsert(item("z_primary", func(i *Item) { i.Priority = 100; i.MaxInflight = 1 }))
	if got := p.PickRoute(RouteQuery{}); !got.OK || got.Item.ID != "z_primary" {
		t.Fatalf("未优先选择主账号：%+v", got)
	}
	p.Acquire("z_primary")
	if got := p.PickRoute(RouteQuery{}); !got.OK || got.Item.ID != "a_backup" {
		t.Fatalf("主账号满载后未选择备用账号：%+v", got)
	}
}

func TestOptimizationAuthenticationCoolsAllModels(t *testing.T) {
	c := Classify(401, `{"error":{"message":"invalid credential"}}`, "", KindAuth, ClassifyOptions{Model: "model-a"})
	if c.Model != "" || c.Cooldown <= 0 {
		t.Fatalf("凭证失效必须影响整个账号，实际分类=%+v", c)
	}
}

// 所有请求同时到达时，选择和占位不能争抢同一个最后名额。
func TestOptimizationAcquireRouteIsAtomic(t *testing.T) {
	p := New(StrategyRoundRobin)
	const accounts = 32
	for index := 0; index < accounts; index++ {
		p.Upsert(item(fmt.Sprintf("acc_%02d", index), func(i *Item) { i.MaxInflight = 1 }))
	}
	start := make(chan struct{})
	results := make(chan PickResult, accounts)
	var workers sync.WaitGroup
	for index := 0; index < accounts; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- p.AcquireRoute(RouteQuery{})
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	seen := map[string]bool{}
	for result := range results {
		if !result.OK || seen[result.Item.ID] || result.Item.InFlight != 1 {
			t.Fatalf("有剩余名额却选取失败或重复占用：%+v", result)
		}
		seen[result.Item.ID] = true
	}
	if result := p.AcquireRoute(RouteQuery{}); result.OK || result.Reason != ReasonAllSaturated {
		t.Fatalf("全部名额占满后未准确拒绝：%+v", result)
	}
	for id := range seen {
		p.Release(id)
	}
	if !p.AcquireRoute(RouteQuery{}).OK {
		t.Fatal("释放后没有恢复可用名额")
	}
}

func TestOptimizationRouteCacheIsBounded(t *testing.T) {
	p := New(StrategyWeightedRoundRobin)
	p.Upsert(item("a", withWeight(10)))
	p.Upsert(item("b", withWeight(20)))
	hot := RouteQuery{PublicModel: "hot-model"}
	old := RouteQuery{PublicModel: "old-model"}
	p.PickRoute(old)
	for index := 0; index < MaxRouteStates+10; index++ {
		p.PickRoute(RouteQuery{PublicModel: fmt.Sprintf("model-%d", index)})
		p.PickRoute(hot)
	}
	if len(p.routeUsage) != MaxRouteStates || len(p.lastPicked) > MaxRouteStates ||
		len(p.lastRegion) > MaxRouteStates || len(p.weightCounter) > MaxRouteStates {
		t.Fatal("路由相关缓存超过容量上限")
	}
	if _, exists := p.routeUsage[rotationKey(hot)]; !exists {
		t.Fatal("正在使用的路由被淘汰")
	}
	if _, exists := p.weightCounter[rotationKey(old)]; exists {
		t.Fatal("旧路由的加权计数没有一起清理")
	}
	p.Remove("a")
	p.Remove("b")
	if len(p.routeUsage) != 0 || p.routeLRU.Len() != 0 {
		t.Fatal("空号池仍保留了路由状态")
	}
}

func TestOptimizationSyncPublishesWholeSnapshot(t *testing.T) {
	p := New(StrategyRoundRobin)
	left := []Item{item("a"), item("b")}
	right := []Item{item("c"), item("d")}
	p.SyncFromStore(left)
	done := make(chan struct{})
	var worker sync.WaitGroup
	worker.Add(1)
	go func() {
		defer worker.Done()
		defer close(done)
		for index := 0; index < 1000; index++ {
			p.SyncFromStore(right)
			p.SyncFromStore(left)
		}
	}()
	defer worker.Wait()
	for {
		snapshot := p.Items()
		if len(snapshot) != 2 || !((snapshot[0].ID == "a" && snapshot[1].ID == "b") || (snapshot[0].ID == "c" && snapshot[1].ID == "d")) {
			t.Fatalf("读到了同步过程中的半成品：%+v", snapshot)
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func TestOptimizationCooldownTransitionsNotify(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("a"))
	var observed []Item
	p.SetObserver(func(i Item) {
		// 回调里再次读取号池，验证回调没有持有号池锁。
		p.ByID(i.ID)
		observed = append(observed, i)
	})
	p.MarkDown("a", time.Minute, "测试连接失败")
	p.SweepExpired(time.Now().Add(time.Hour))
	if len(observed) != 2 || observed[0].DownUntil.IsZero() || !observed[1].DownUntil.IsZero() || observed[1].StateVersion <= observed[0].StateVersion {
		t.Fatal("冷却开始或到期没有发出正确的持久化快照")
	}
}
