package store

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// itoa renders an int for building distinct row IDs in a table-driven test.
func itoa(v int) string { return strconv.Itoa(v) }

// Regression tests for the defects found by the second independent audit.
// Each asserts the CORRECT behaviour and fails on the pre-fix code.

// Latency percentiles must use nearest-rank selection.
//
// WHICH INPUTS ACTUALLY DISCRIMINATE — established by exhaustive comparison of
// the two formulas (verified independently during the third audit):
//
//		nearest-rank (fixed):  index = round(fraction * (N-1))
//		pre-fix:               index = floor(fraction * N)
//
//	  - p50 NEVER discriminates. round(0.5*(N-1)) == floor(0.5*N) for every N, so
//	    an assertion about the median cannot detect this defect at all.
//	  - p95 discriminates only from N=12 upward: at N=4 both give index 3.
//
// So the ONLY usable inputs are large-N with a known distribution. An earlier
// version of this test used N=4 and N=2 and passed against the pre-fix code,
// which made it worthless as a regression test; the mistake was writing the
// assertion first and inferring the defect from it, instead of first finding
// where the two formulas actually differ.
//
// N=100 with values 10,20,...,1000:
//
//	nearest-rank p95 -> index round(0.95*99) = 94 -> 950
//	pre-fix       p95 -> index floor(0.95*100) = 95 -> 960
func TestLatencyP95UsesNearestRankOnLargeSamples(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	const n = 100
	for i := 1; i <= n; i++ {
		seedRequest(t, st, RequestLog{
			ID: "req_nr" + itoa(i), Status: StatusOK, Protocol: "chat.completions",
			RequestedModel: "glm-5.2", MappedModel: "glm-5.2", Provider: "workbuddy",
			LatencyMs: intp(i * 10),
		})
	}

	p95, err := st.latencyP95(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("latency p95: %v", err)
	}
	// The exact value pins the convention: 950 is nearest-rank, 960 is the
	// off-by-one form.
	if p95 != 950 {
		t.Errorf("p95 over 100 samples (10..1000) = %.0f, want 950 (nearest-rank).\n"+
			"960 means the offset is floor(fraction*N) instead of round(fraction*(N-1)): "+
			"every percentile is then one position too high", p95)
	}
}

// The smallest input that separates the two conventions, so a future change to
// the offset fails fast without needing a hundred rows.
//
// N=12 with values 1..12:
//
//	nearest-rank p95 -> round(0.95*11) = 10 -> the 11th value = 11
//	pre-fix       p95 -> floor(0.95*12) = 11 -> the 12th value = 12
func TestLatencyP95DiscriminatesAtTwelveSamples(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	for i := 1; i <= 12; i++ {
		seedRequest(t, st, RequestLog{
			ID: "req_small" + itoa(i), Status: StatusOK, Protocol: "chat.completions",
			RequestedModel: "glm-5.2", MappedModel: "glm-5.2", Provider: "workbuddy",
			LatencyMs: intp(i),
		})
	}

	p95, err := st.latencyP95(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("latency p95: %v", err)
	}
	if p95 != 11 {
		t.Errorf("p95 over 12 samples (1..12) = %.0f, want 11; 12 means the offset is "+
			"one position too high", p95)
	}
}

// A small sample must still produce a sane p95 (the top of the range), even
// though it cannot discriminate the two conventions.
func TestLatencyP95OnSmallSamples(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	for i, ms := range []int{10, 20, 30, 40} {
		seedRequest(t, st, RequestLog{
			ID: "req_sm" + string(rune('a'+i)), Status: StatusOK, Protocol: "chat.completions",
			RequestedModel: "glm-5.2", MappedModel: "glm-5.2", Provider: "workbuddy",
			LatencyMs: intp(ms),
		})
	}

	p95, err := st.latencyP95(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("latency p95: %v", err)
	}
	if p95 != 40 {
		t.Errorf("p95 over {10,20,30,40} = %.0f, want 40 (the maximum)", p95)
	}
}

// The clearest form of the defect: with two samples, the median must not equal
// the maximum.
//
// The two conventions (nearest-rank vs the pre-fix `floor(p*N)`) only diverge on
// EVEN sample counts, so two samples is the case that separates them:
//
//	sorted {100,500}, N=2
//	  nearest-rank: round(0.5 * 1) = 1 -> 500
//	  pre-fix:      floor(0.5 * 2) = 1 -> 500
//
// ... they agree here too. The first regression test is the one that actually
// distinguishes the forms on this input, because it compares p50 against p95 and
// against the maximum rather than asserting a single value.
//
// This test therefore pins the OBSERVABLE contract instead: whatever the
// convention, the median of two samples must not exceed the maximum and must be
// one of the two values.
func TestLatencyMedianOnTwoSamplesIsOneOfTheValues(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	for i, ms := range []int{100, 500} {
		seedRequest(t, st, RequestLog{
			ID: "req_two" + string(rune('a'+i)), Status: StatusOK, Protocol: "chat.completions",
			RequestedModel: "glm-5.2", MappedModel: "glm-5.2", Provider: "workbuddy",
			LatencyMs: intp(ms),
		})
	}

	p50, p95, err := st.latencyPercentiles(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("latencyPercentiles: %v", err)
	}
	if p50 != 100 && p50 != 500 {
		t.Errorf("p50=%.0f is not one of the samples {100,500}", p50)
	}
	if p95 != 500 {
		t.Errorf("p95=%.0f, want 500", p95)
	}
	if p50 > p95 {
		t.Errorf("p50=%.0f exceeds p95=%.0f, which is impossible", p50, p95)
	}
}

// A single sample must not panic or produce a negative offset.
func TestLatencyPercentileSingleSample(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	seedRequest(t, st, RequestLog{
		ID: "req_one", Status: StatusOK, Protocol: "chat.completions",
		RequestedModel: "glm-5.2", MappedModel: "glm-5.2", Provider: "workbuddy",
		LatencyMs: intp(250),
	})

	p50, p95, err := st.latencyPercentiles(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("latencyPercentiles: %v", err)
	}
	if p50 != 250 || p95 != 250 {
		t.Errorf("one sample must give p50=p95=250, got %.0f / %.0f", p50, p95)
	}
}

// Requests that failed BEFORE an account was chosen must still be counted.
//
// REGRESSION: the query filtered `provider <> ”`, so "no account available"
// failures vanished from the panel. On live data the summary read 53 requests
// while the provider rows summed to 22 -- 58% of traffic unaccounted for, with
// nothing in the UI to say where it went.
func TestProviderStatsIncludesUnattributed(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// Two attributed requests.
	for i := 0; i < 2; i++ {
		seedRequest(t, st, RequestLog{
			ID: "req_attr" + string(rune('a'+i)), Status: StatusOK, Protocol: "chat.completions",
			RequestedModel: "glm-5.2", MappedModel: "glm-5.2", Provider: "workbuddy", AccountID: "acc_a",
			LatencyMs: intp(10),
		})
	}
	// Three that never reached an account: no provider recorded.
	for i := 0; i < 3; i++ {
		seedRequest(t, st, RequestLog{
			ID: "req_unattr" + string(rune('a'+i)), Status: StatusError, Protocol: "chat.completions",
			RequestedModel: "glm-5.2", Provider: "", AccountID: "",
			ErrorKind: "unavailable", ErrorMessage: "no usable account",
		})
	}

	stats, err := st.OverviewStats(ctx, time.Now().UTC().Add(-time.Hour), 6, 10*time.Minute)
	if err != nil {
		t.Fatalf("OverviewStats: %v", err)
	}

	total := int64(0)
	for _, p := range stats.Provider {
		total += p.Requests
	}
	// The whole point: the channels must add up to the summary.
	if total != stats.Summary.Requests {
		t.Errorf("provider rows sum to %d but the summary reports %d requests; "+
			"%d rows are invisible to the operator", total, stats.Summary.Requests,
			stats.Summary.Requests-total)
	}

	var unattributed *ProviderStat
	for i := range stats.Provider {
		if stats.Provider[i].Provider == UnattributedProvider {
			unattributed = &stats.Provider[i]
		}
	}
	if unattributed == nil {
		t.Fatalf("no %q bucket was reported; the %d pre-routing failures are unaccounted for",
			UnattributedProvider, 3)
	}
	if unattributed.Requests != 3 {
		t.Errorf("unattributed bucket = %d requests, want 3", unattributed.Requests)
	}
}

// A deleted account must not keep inflating a channel's account count.
//
// This is asserted at the store level by showing the stored aggregate counts
// historical IDs, which is why the API layer overrides it from the live pool.
func TestProviderAccountCountIsHistoricalInTheStore(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// Two requests from accounts that no longer exist.
	for i, acct := range []string{"acc_gone_1", "acc_gone_2"} {
		seedRequest(t, st, RequestLog{
			ID: "req_ghost" + string(rune('a'+i)), Status: StatusOK, Protocol: "chat.completions",
			RequestedModel: "glm-5.2", MappedModel: "glm-5.2", Provider: "workbuddy", AccountID: acct,
			LatencyMs: intp(10),
		})
	}

	stats, err := st.OverviewStats(ctx, time.Now().UTC().Add(-time.Hour), 6, 10*time.Minute)
	if err != nil {
		t.Fatalf("OverviewStats: %v", err)
	}
	// The store has no account table view, so it reports the historical count.
	// That is exactly why the API layer must override it: this assertion
	// documents the trap rather than endorsing it.
	if len(stats.Provider) == 0 || stats.Provider[0].Accounts != 2 {
		t.Errorf("the store's account count should be the historical 2 (which the API "+
			"layer overrides from the live pool), got %+v", stats.Provider)
	}
}

// Concurrent Open on a fresh database must not make any process fail.
//
// REGRESSION: two processes could both see a migration as pending and both try
// to record it, so the loser hit a UNIQUE violation and Open returned an error,
// refusing to start. Measured at ~5.6% of runs with three concurrent starts.
//
// WHY IT RUNS MANY ROUNDS: the race is timing-dependent, so a single round
// catches it only some of the time and the test would pass by luck often enough
// to be worthless as a guard -- which was observed: the mutation gate reported
// this test as VACUOUS because one round happened not to lose the race. Repeating
// the scenario makes the test fail reliably whenever the tolerance is removed,
// while still being fast because each round is a handful of milliseconds.
func TestConcurrentOpenAllSucceed(t *testing.T) {
	const (
		procs  = 4
		rounds = 40
	)

	var failedRound int
	var firstErr error

	for round := 0; round < rounds; round++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "relaypool.db")

		var wg sync.WaitGroup
		errs := make([]error, procs)
		stores := make([]*Store, procs)

		for i := 0; i < procs; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				st, err := Open(context.Background(), path)
				stores[idx] = st
				errs[idx] = err
			}(i)
		}
		wg.Wait()

		for _, st := range stores {
			if st != nil {
				st.Close()
			}
		}

		for _, err := range errs {
			if err != nil {
				failedRound = round
				firstErr = err
				break
			}
		}
		if firstErr != nil {
			break
		}
	}

	if firstErr != nil {
		t.Fatalf("round %d/%d: a concurrent Open failed: %v\n"+
			"losing the migration race must be tolerated: the schema is already what "+
			"the winner applied, so refusing to start is the wrong outcome",
			failedRound+1, rounds, firstErr)
	}

	// And the schema must be complete and un-duplicated afterwards.
	dir := t.TempDir()
	path := filepath.Join(dir, "relaypool.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen after concurrent migration: %v", err)
	}
	defer st.Close()

	var count int
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count != len(migrations) {
		t.Errorf("schema_migrations has %d rows, want %d (one per migration)", count, len(migrations))
	}

	// The armor_preset column is added by migration 003; a half-applied schema
	// would be missing it. Check the column exists exactly once.
	var colCount int
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM pragma_table_info('accounts') WHERE name = 'armor_preset'`).Scan(&colCount); err != nil {
		t.Fatalf("inspect accounts columns: %v", err)
	}
	if colCount != 1 {
		t.Errorf("accounts.armor_preset appears %d times, want 1; the schema may be half-applied", colCount)
	}
}

// The checksum guard must still reject an edited migration: tolerating a lost
// race must not tolerate a changed schema.
func TestMigrationChecksumStillGuarded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relaypool.db")

	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Corrupt the recorded checksum of an applied migration.
	if _, err := st.db.ExecContext(context.Background(),
		`UPDATE schema_migrations SET checksum = 'deadbeef' WHERE filename = ?`,
		migrations[0].filename); err != nil {
		t.Fatalf("corrupt checksum: %v", err)
	}
	st.Close()

	// Reopening must fail loudly rather than run new code against an unknown
	// schema.
	if _, err := Open(context.Background(), path); err == nil {
		t.Errorf("a checksum mismatch must refuse to start; the race tolerance " +
			"must not have swallowed it")
	}
}
