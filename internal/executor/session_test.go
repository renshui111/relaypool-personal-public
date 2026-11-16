package executor

import (
	"testing"
	"testing/synctest"
	"time"
)

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
