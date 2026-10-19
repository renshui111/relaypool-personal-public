package executor

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
)

// 已打开但还未结束的流必须继续占用并发名额。
func TestOptimizationStreamOwnsSlot(t *testing.T) {
	h := newHarness(t, "acc_a")
	h.pool.SetMaxInflight("acc_a", 1)
	result, err := h.executor.ChatStream(context.Background(), request("m"))
	if err != nil {
		t.Fatal(err)
	}
	defer result.Stream.Close()
	current, _ := h.pool.ByID("acc_a")
	if current.InFlight != 1 {
		t.Fatalf("流仍在运行，但并发数=%d，期望=1", current.InFlight)
	}
	_, err = h.executor.ChatNonStream(context.Background(), request("m"))
	var failure *FailureError
	if !errors.As(err, &failure) || failure.Code != "pool_saturated" {
		t.Fatalf("满载时应拒绝第二个请求，实际=%v", err)
	}
	if len(h.provider.callOrder()) != 1 {
		t.Fatal("满载请求仍然调用了上游")
	}
	result.Stream.Close()
	if _, err := h.executor.ChatNonStream(context.Background(), request("m")); err != nil {
		t.Fatalf("关闭流后未恢复名额：%v", err)
	}
}

func TestReviewStreamCancellationReleasesWithoutRead(t *testing.T) {
	h := newHarness(t, "acc_a")
	adapter := &countingStreamProvider{}
	h.registry.Register(adapter)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := h.executor.ChatStream(ctx, request("m"))
	if err != nil {
		t.Fatal(err)
	}
	defer result.Stream.Close()
	cancel()
	deadline := time.After(time.Second)
	for {
		current, _ := h.pool.ByID("acc_a")
		if current.InFlight == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("取消后名额未释放")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if adapter.closes.Load() != 1 {
		t.Fatal("取消未关闭上游")
	}
}

func TestReviewIndependentQuotaFailsOver(t *testing.T) {
	h := newHarness(t, "acc_a", "acc_b")
	a, _ := h.pool.ByID("acc_a")
	a.QuotaScope = "account"
	h.pool.Upsert(a)
	h.provider.behavior["acc_a"] = func(int) (*provider.ChatResult, error) {
		return nil, &provider.Failure{Status: 429, Body: `{"error":{"type":"insufficient_quota"}}`}
	}
	result, err := h.executor.ChatNonStream(context.Background(), request("m"))
	if err != nil || result.AccountID != "acc_b" || result.AttemptCount != 2 {
		t.Fatalf("独立额度未换号：结果=%+v，错误=%v", result, err)
	}
}

func TestReviewFailureReportsActualBreakerCooldown(t *testing.T) {
	h := newHarness(t, "acc_a")
	h.pool.SetBreakerCooldown(2*time.Minute, 5*time.Minute)
	h.executor.opts.BreakerThreshold = 1
	h.provider.behavior["acc_a"] = func(int) (*provider.ChatResult, error) {
		return nil, &provider.Failure{Status: 429, KindHint: pool.KindRateLimit, RetryAfter: time.Second}
	}
	_, err := h.executor.ChatNonStream(context.Background(), request("m"))
	var failure *FailureError
	if !errors.As(err, &failure) || failure.RetryAfter < 119*time.Second || failure.RetryAfter > 2*time.Minute {
		t.Fatalf("错误响应丢失了实际熔断恢复时间：%+v", failure)
	}
}

// EOF 与重复 Close 都必须释放名额，且不能重复关闭上游。
func TestOptimizationStreamEOFReleasesOnce(t *testing.T) {
	h := newHarness(t, "acc_a")
	adapter := &countingStreamProvider{}
	h.registry.Register(adapter)
	result, err := h.executor.ChatStream(context.Background(), request("m"))
	if err != nil {
		t.Fatal(err)
	}
	defer result.Stream.Close()
	if _, err := io.ReadAll(result.Stream.Body); err != nil {
		t.Fatal(err)
	}
	current, _ := h.pool.ByID("acc_a")
	if current.InFlight != 0 || adapter.closes.Load() != 1 {
		t.Fatalf("EOF 未收尾：并发=%d，关闭次数=%d", current.InFlight, adapter.closes.Load())
	}
	var workers sync.WaitGroup
	for index := 0; index < 16; index++ {
		workers.Add(1)
		go func() { defer workers.Done(); result.Stream.Close() }()
	}
	workers.Wait()
	if adapter.closes.Load() != 1 {
		t.Fatal("重复 Close 再次关闭了上游")
	}
}

func TestOptimizationPanicReleasesSlot(t *testing.T) {
	h := newHarness(t, "acc_a")
	func() {
		defer func() {
			if recover() == nil {
				t.Error("测试适配器没有触发预期异常")
			}
		}()
		h.executor.execute(context.Background(), request("m"), func(context.Context, pool.Item, provider.Provider, int) (*Result, *pool.Classified, error) {
			panic("测试异常")
		})
	}()
	current, _ := h.pool.ByID("acc_a")
	if current.InFlight != 0 {
		t.Fatalf("适配器异常后并发名额泄漏：%d", current.InFlight)
	}
}

func TestOptimizationCredentialFailureBlocksOtherModels(t *testing.T) {
	h := newHarness(t, "acc_a")
	h.creds.failOn["acc_a"] = errors.New("测试凭证已失效")
	_, _ = h.executor.ChatNonStream(context.Background(), request("model-a"))
	current, _ := h.pool.ByID("acc_a")
	if current.DownUntil.IsZero() {
		t.Fatal("凭证失败只冷却了单个模型，其他模型还会重复使用失效账号")
	}
}

func TestOptimizationProviderRetryAfterRespectsLimits(t *testing.T) {
	h := newHarness(t, "acc_a")
	for _, hint := range []time.Duration{time.Second, 24 * time.Hour} {
		failure := h.executor.classify(pool.Item{}, request("m"), &provider.Failure{Status: 429, KindHint: pool.KindRateLimit, RetryAfter: hint})
		if failure.Cooldown < h.executor.opts.MinRateLimitCooldown || failure.Cooldown > h.executor.opts.MaxRetryAfter {
			t.Fatalf("适配器提示绕过了冷却上下限：提示=%s，结果=%s", hint, failure.Cooldown)
		}
	}
}

// 请求发出之后出现的新故障，不能被同一个旧请求的成功覆盖。
func TestOptimizationLateSuccessPreservesNewFailure(t *testing.T) {
	for _, scopedModel := range []string{"", "m"} {
		t.Run("故障范围_"+scopedModel, func(t *testing.T) {
			h := newHarness(t, "acc_a")
			h.provider.defaultBehavior = func(string, int) (*provider.ChatResult, error) {
				h.pool.MarkClassified("acc_a", pool.Classified{Kind: pool.KindRateLimit, Model: scopedModel, Cooldown: time.Minute, Message: "较新的限流"}, 3)
				return &provider.ChatResult{Model: "m"}, nil
			}
			if _, err := h.executor.ChatNonStream(context.Background(), request("m")); err != nil {
				t.Fatal(err)
			}
			current, _ := h.pool.ByID("acc_a")
			if current.LastError != "较新的限流" {
				t.Fatal("迟到的成功覆盖了较新的错误状态")
			}
			if scopedModel == "" && current.DownUntil.IsZero() {
				t.Fatal("较新的账号冷却被清除了")
			}
			if scopedModel != "" && current.ModelDownUntil[scopedModel].IsZero() {
				t.Fatal("较新的模型冷却被清除了")
			}
		})
	}
}
