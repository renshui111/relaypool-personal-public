package executor

import (
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestSessionRebindRenewsTTLAndPreservesOtherBindings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := NewSessionAffinity(time.Minute, 2)
		a.Bind("first", "old-account")
		a.Bind("second", "other-account")
		time.Sleep(40 * time.Second)
		a.Bind(" first ", " new-account ")
		a.Bind("first", "new-account")
		if got := a.Stats().Rebindings; got != 1 {
			t.Fatalf("rebindings = %d, want 1", got)
		}
		if id, ok := a.Get("second"); !ok || id != "other-account" {
			t.Fatal("rebinding changed another conversation")
		}
		time.Sleep(40 * time.Second)
		if id, ok := a.Get("first"); !ok || id != "new-account" {
			t.Fatal("rebinding did not renew the TTL or account")
		}
		a.Forget("first")
		if _, ok := a.Get("first"); ok {
			t.Fatal("forgotten binding is still reachable")
		}
		time.Sleep(20 * time.Second)
		if removed := a.Prune(); removed != 1 || a.Stats().Bindings != 0 {
			t.Fatal("idle binding was not pruned")
		}
		a.Bind("first", "fresh-account")
		if id, ok := a.Get("first"); !ok || id != "fresh-account" {
			t.Fatal("forgotten key could not be reused")
		}
	})
}

func TestSessionConcurrentRefreshAndMaintenance(t *testing.T) {
	a := NewSessionAffinity(time.Hour, 64)
	a.Bind("shared", "account")
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Go(func() {
			key := "worker-" + strconv.Itoa(worker)
			for i := 0; i < 100; i++ {
				a.Bind("shared", "account")
				if id, ok := a.Get("shared"); !ok || id != "account" {
					t.Error("shared binding lost during refresh")
				}
				a.Bind(key, "old")
				a.Bind(key, "new")
				if id, ok := a.Get(key); !ok || id != "new" {
					t.Error("worker binding lost or crossed with another worker")
				}
				a.Forget(key)
				a.Prune()
				a.Stats()
			}
		})
	}
	wg.Wait()
	if stats := a.Stats(); stats.Bindings != 1 || stats.Rebindings != 800 || stats.Hits != 1600 || stats.Misses != 0 {
		t.Fatalf("unexpected counters after concurrent access: %+v", stats)
	}
}

func TestSessionHitRenewsIdleTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := NewSessionAffinity(time.Minute, 10)
		a.Bind("active", "account")
		for i := 0; i < 3; i++ {
			time.Sleep(40 * time.Second)
			if id, ok := a.Get("active"); !ok || id != "account" {
				t.Fatalf("active conversation expired on hit %d: %q, %v", i, id, ok)
			}
		}
		time.Sleep(time.Minute)
		if _, ok := a.Get("active"); ok {
			t.Fatal("idle conversation survived its TTL")
		}
	})
}

func TestSessionPruneAfterHitPreservesExpiryOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := NewSessionAffinity(time.Minute, 2)
		a.Bind("first", "a")
		time.Sleep(20 * time.Second)
		a.Bind("second", "b")
		time.Sleep(20 * time.Second)
		if _, ok := a.Get("first"); !ok {
			t.Fatal("first binding unexpectedly expired")
		}
		time.Sleep(40 * time.Second)
		if got := a.Prune(); got != 1 {
			t.Fatalf("pruned %d bindings, want only the idle second binding", got)
		}
		if id, ok := a.Get("first"); !ok || id != "a" {
			t.Fatal("active binding lost after pruning")
		}
		a.Bind("third", "c")
		a.Bind("fourth", "d")
		if _, ok := a.Get("first"); ok {
			t.Fatal("capacity limit did not evict the least recently used binding")
		}
		if got := a.Stats().Bindings; got != 2 {
			t.Fatalf("bindings = %d, want 2", got)
		}
	})
}
