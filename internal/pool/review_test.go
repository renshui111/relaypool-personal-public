package pool

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// 同类失败达到阈值之前，使用分类器给出的冷却，不提前翻倍。
func TestReviewBreakerHonorsThreshold(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("a"))
	for index := 1; index <= 2; index++ {
		p.MarkClassified("a", Classified{Kind: KindUnavailable, Cooldown: time.Minute}, 3)
		got, _ := p.ByID("a")
		if remaining := time.Until(got.DownUntil); remaining > time.Minute || remaining < 59*time.Second {
			t.Fatalf("第 %d 次失败提前熔断：%s", index, remaining)
		}
	}
}

func TestReviewInvalidModelsDoNotAccumulateBackoff(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("a"))
	for index := 0; index < 5000; index++ {
		p.MarkClassified("a", Classified{Kind: KindModelNotAvailable, Model: fmt.Sprintf("invalid-%d", index)}, 3)
	}
	current, _ := p.ByID("a")
	if len(current.ModelBackoff) != 0 || len(current.ModelLastKind) != 0 {
		t.Fatal("无效模型名持续扩大退避状态")
	}
}

func TestReviewModelFailureHistoryBoundedAcrossSweeps(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("a"))
	for index := 0; index < MaxModelFailureStates+100; index++ {
		p.MarkClassified("a", Classified{Kind: KindUnavailable, Model: fmt.Sprintf("m-%d", index), Cooldown: time.Second}, 3)
		p.SweepExpired(time.Now().Add(time.Hour))
	}
	current, _ := p.ByID("a")
	if len(current.ModelBackoff) != MaxModelFailureStates || len(current.ModelLastKind) != MaxModelFailureStates {
		t.Fatal("模型故障历史容量未限制")
	}
	for index := 0; index < 2; index++ {
		p.MarkClassified("a", Classified{Kind: KindUnavailable, Model: "m-0", Cooldown: time.Second}, 3)
		if index == 0 {
			p.SweepExpired(time.Now().Add(time.Hour))
		}
	}
	current, _ = p.ByID("a")
	if current.ModelBackoff["m-0"] != 3 || time.Until(current.ModelDownUntil["m-0"]) < 29*time.Second {
		t.Fatal("清扫后的连续失败未触发熔断")
	}
}

func TestReviewBreakerConfiguredBoundAndIsolation(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.SetBreakerCooldown(2*time.Minute, 5*time.Minute)
	p.Upsert(item("a"))
	for index, want := range []time.Duration{time.Second, time.Second, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute} {
		p.MarkClassified("a", Classified{Kind: KindUnavailable, Cooldown: time.Second, Model: "m"}, 3)
		got, _ := p.ByID("a")
		if remaining := time.Until(got.ModelDownUntil["m"]); remaining > want || remaining < want-time.Second {
			t.Fatalf("第 %d 次失败冷却=%s，期望=%s", index+1, remaining, want)
		}
		if !p.PickRoute(RouteQuery{PublicModel: "other"}).OK {
			t.Fatal("模型退避影响其他模型")
		}
	}
	p.MarkClassified("a", Classified{Kind: KindAuth, Cooldown: time.Second, Model: "m"}, 3)
	got, _ := p.ByID("a")
	if got.ModelBackoff["m"] != 1 {
		t.Fatal("错误类型变化未重置失败计数")
	}
	if remaining := time.Until(got.ModelDownUntil["m"]); remaining < 4*time.Minute {
		t.Fatal("迟到错误缩短了已有冷却")
	}
}

func TestReviewOldAttemptCannotMutateRecreatedAccount(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("a"))
	old := p.AcquireRoute(RouteQuery{}).Item
	p.Remove("a")
	p.Upsert(item("a"))
	newItem := p.AcquireRoute(RouteQuery{}).Item
	p.ReleaseItem(old)
	p.MarkAttemptClassified(old, Classified{Kind: KindAuth, Cooldown: time.Hour}, 3)
	p.MarkAttemptOK(old, "unproven")
	current, _ := p.ByID("a")
	if current.InFlight != 1 || !current.DownUntil.IsZero() || len(current.ProvenModels) != 0 {
		t.Fatalf("旧账号结果影响了新实例：%+v", current)
	}
	p.ReleaseItem(newItem)
}

func TestReviewCredentialReplacementIgnoresOldFailure(t *testing.T) {
	p := New(StrategyRoundRobin)
	p.Upsert(item("a"))
	old := p.AcquireRoute(RouteQuery{}).Item
	p.ResetCredentialHealth("a")
	p.MarkAttemptClassified(old, Classified{Kind: KindAuth, Cooldown: time.Hour}, 3)
	p.ReleaseItem(old)
	current, _ := p.ByID("a")
	if current.InFlight != 0 || !current.DownUntil.IsZero() {
		t.Fatal("更换凭证后仍受到旧请求影响")
	}
	p.MarkClassified("a", Classified{Kind: KindQuota, Cooldown: time.Hour}, 3)
	p.ResetCredentialHealth("a")
	current, _ = p.ByID("a")
	if current.DownUntil.IsZero() {
		t.Fatal("更换凭证错误清除了额度冷却")
	}
}

func TestReviewRetryAfterDatesAndOverflow(t *testing.T) {
	deadline := time.Now().UTC().Add(2 * time.Minute).Format(http.TimeFormat)
	if got := ParseRetryAfter(deadline, time.Second, 10*time.Minute); got < 119*time.Second || got > 2*time.Minute {
		t.Fatalf("未解析日期格式：%s", got)
	}
	for _, raw := range []string{"1e30", "999999999999999999999999"} {
		if got := ParseRetryAfter(raw, time.Second, 10*time.Minute); got != 10*time.Minute {
			t.Fatalf("巨大数值未钳制：%s", got)
		}
	}
}

func TestReviewQuotaScopes(t *testing.T) {
	for _, scope := range []string{"shared", "account", "model"} {
		t.Run(scope, func(t *testing.T) {
			classified := Classify(429, `{"error":{"type":"insufficient_quota"}}`, "120", "", ClassifyOptions{Model: "m", QuotaScope: scope})
			if classified.Failover != (scope != "shared") || classified.Cooldown != 2*time.Minute {
				t.Fatalf("额度策略错误：%+v", classified)
			}
			p := New(StrategyRoundRobin)
			p.Upsert(item("a", func(i *Item) { i.QuotaScope = scope }))
			p.MarkClassified("a", classified, 3)
			if p.PickRoute(RouteQuery{PublicModel: "m"}).OK {
				t.Fatal("耗尽模型仍可调用")
			}
			if p.PickRoute(RouteQuery{PublicModel: "other"}).OK != (scope == "model") {
				t.Fatal("额度范围与调度不一致")
			}
		})
	}
}
