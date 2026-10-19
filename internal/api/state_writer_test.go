package api

import (
	"sync"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/store"
)

// accountFixture builds a minimal account for these tests.
func accountFixture(id string) store.Account {
	return store.Account{
		ID:          id,
		Name:        id,
		Provider:    "workbuddy",
		Region:      "cn",
		Enabled:     true,
		MaxInflight: 4,
		Weight:      50,
	}
}

// The observer runs on the request path, so it must not block. This measures the
// coalescing: many state changes for one account must produce far fewer writes.
func TestStateWriterCoalescesBursts(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()

	// One account, created so the runtime update has a row to touch.
	if _, err := st.CreateAccount(ctx, accountFixture("acc_burst")); err != nil {
		t.Fatalf("create: %v", err)
	}

	w := newStateWriter(st)
	w.start()
	defer w.stop()

	// Simulate a burst of activity on one account: 200 state changes, as 200
	// successful requests would produce.
	start := time.Now()
	for i := 0; i < 200; i++ {
		w.observe(pool.Item{
			ID:        "acc_burst",
			DownUntil: time.Now().Add(time.Duration(i) * time.Second),
			LastError: "err",
			LastKind:  "rate_limit",
		})
	}
	// The observe calls must be effectively free: they only touch a map and a
	// buffered channel.
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Errorf("200 observe calls took %s; the observer must not block the "+
			"request path", elapsed)
	}

	// Let the writer flush.
	time.Sleep(500 * time.Millisecond)
	w.flush(ctx)

	// The final state must be persisted, and it must be the LAST one observed.
	account, err := st.GetAccount(ctx, "acc_burst")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if account.LastErrorKind != "rate_limit" {
		t.Errorf("LastKind=%q, want the last observed value", account.LastErrorKind)
	}
	if account.CooldownUntil.IsZero() {
		t.Errorf("the cooldown was not persisted")
	}
}

// The writer must still persist the final state on stop, so a clean shutdown
// does not lose a cooldown.
func TestStateWriterFlushesOnStop(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()

	if _, err := st.CreateAccount(ctx, accountFixture("acc_stop")); err != nil {
		t.Fatalf("create: %v", err)
	}

	w := newStateWriter(st)
	// A long interval so only the stop-time flush can persist anything.
	w.flushInterval = time.Hour
	w.start()

	w.observe(pool.Item{
		ID:        "acc_stop",
		DownUntil: time.Now().Add(time.Hour),
		LastError: "boom",
		LastKind:  "quota",
	})
	// Stop immediately: the flush-on-stop is what must save this.
	w.stop()

	account, err := st.GetAccount(ctx, "acc_stop")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if account.LastErrorKind != "quota" {
		t.Errorf("LastKind=%q after stop, want quota; the shutdown flush did not run",
			account.LastErrorKind)
	}
	if account.CooldownUntil.IsZero() {
		t.Errorf("the cooldown was lost across a clean shutdown")
	}
}

// Stopping twice must be safe.
func TestStateWriterStopIsIdempotent(t *testing.T) {
	st := openTestStore(t)
	w := newStateWriter(st)
	w.start()
	w.stop()
	w.stop()
}

// An observe on a nil writer must be a no-op rather than a panic: New() may run
// without a store in a test.
func TestStateWriterNilIsSafe(t *testing.T) {
	var w *stateWriter
	w.observe(pool.Item{ID: "x"})
}

// A concurrent observe storm must not race or lose the latest value.
func TestStateWriterConcurrentObserves(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()

	if _, err := st.CreateAccount(ctx, accountFixture("acc_conc")); err != nil {
		t.Fatalf("create: %v", err)
	}

	w := newStateWriter(st)
	w.start()
	defer w.stop()

	const workers = 16
	const perWorker = 50
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				w.observe(pool.Item{
					ID:        "acc_conc",
					DownUntil: time.Now().Add(time.Minute),
					LastError: "concurrent",
					LastKind:  "auth",
				})
			}
		}()
	}
	wg.Wait()

	// Give the flusher a pass, then force one.
	time.Sleep(300 * time.Millisecond)
	w.flush(ctx)

	account, err := st.GetAccount(ctx, "acc_conc")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if account.LastErrorKind != "auth" {
		t.Errorf("LastKind=%q, want auth; a concurrent observe was lost",
			account.LastErrorKind)
	}
}
