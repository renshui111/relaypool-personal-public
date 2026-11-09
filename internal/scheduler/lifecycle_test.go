package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRestartWaitsForPreviousWorkers(t *testing.T) {
	t.Run("draining", func(t *testing.T) {
		h := newHarness(t, time.Now())
		s := h.scheduler
		s.opts.RetentionDays = 7
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered := make(chan struct{}, 2)
		draining := make(chan struct{})
		release := make(chan struct{})
		var calls atomic.Int32
		s.SetLogPruner(func(ctx context.Context, _ time.Duration) (int64, error) {
			n := calls.Add(1)
			entered <- struct{}{}
			<-ctx.Done()
			if n == 1 {
				close(draining)
				<-release
			}
			return 0, ctx.Err()
		})
		s.Start(ctx)
		<-entered
		stopped := make(chan struct{})
		go func() { s.Stop(); close(stopped) }()
		<-draining
		restarted := make(chan struct{})
		go func() { s.Start(ctx); close(restarted) }()
		select {
		case <-restarted:
			// Unblock all generations before reporting a regression.
			cancel()
			close(release)
			<-stopped
			t.Fatal("restart returned before previous workers finished")
		case <-time.After(30 * time.Millisecond):
		}
		select {
		case <-stopped:
			t.Error("Stop returned before the cleanup callback finished")
		default:
		}
		close(release)
		<-stopped
		<-restarted
		<-entered
		s.Stop()
		if calls.Load() != 2 {
			t.Fatalf("got %d cleanup generations, want 2", calls.Load())
		}
	})
}

func TestConcurrentSchedulerStartStop(t *testing.T) {
	h := newHarness(t, time.Now())
	s := h.scheduler
	s.opts.RetentionDays = 7
	var active, entered, exited, overlap atomic.Int32
	s.SetLogPruner(func(ctx context.Context, _ time.Duration) (int64, error) {
		if active.Add(1) != 1 {
			overlap.Add(1)
		}
		entered.Add(1)
		<-ctx.Done()
		active.Add(-1)
		exited.Add(1)
		return 0, ctx.Err()
	})
	var callers sync.WaitGroup
	for range 32 {
		callers.Go(func() {
			for range 20 {
				s.Start(t.Context())
				s.Stop()
			}
		})
	}
	callers.Wait()
	s.Stop()
	if active.Load() != 0 || entered.Load() != exited.Load() || overlap.Load() != 0 {
		t.Fatalf("workers remain or overlap: active=%d entered=%d exited=%d overlap=%d", active.Load(), entered.Load(), exited.Load(), overlap.Load())
	}
}

func TestCanceledSchedulerStartIsNoop(t *testing.T) {
	h := newHarness(t, time.Now())
	s := h.scheduler
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.Start(ctx)
	if s.started || s.cancel != nil {
		t.Fatal("canceled context registered a running scheduler")
	}
	// A canceled attempt must not prevent a later valid start.
	s.Start(t.Context())
	if !s.started {
		t.Fatal("valid start was blocked by previous canceled attempt")
	}
	s.Stop()
	s.Stop()
}
