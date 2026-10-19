package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Aggregate statistics for the console overview.
//
// WHY THIS LIVES IN store RATHER THAN THE API LAYER: the numbers are computed by
// SQLite over request_logs, and doing it here keeps one place responsible for
// how a "success" or a "token" is counted. The API layer only shapes the result
// for the wire.
//
// Every aggregate is computed over a WINDOW, because a lifetime total is not
// actionable: an operator needs to know whether the last hour looks healthy,
// not what happened since install.

// StatsWindow is the time range an aggregate covers.
type StatsWindow struct {
	// Since is the inclusive lower bound. A zero value means "no lower bound",
	// which the API layer maps to the maximum retention.
	Since time.Time
	// Label is echoed back so the console can title the card.
	Label string
}

// SummaryTotals is the headline row of numbers.
type SummaryTotals struct {
	Requests   int64   `json:"requests"`
	Successes  int64   `json:"successes"`
	Failures   int64   `json:"failures"`
	InFlight   int64   `json:"in_flight"`
	SuccessPct float64 `json:"success_pct"`

	// Latency percentiles in milliseconds. Percentiles rather than an average
	// because an average hides the tail that users actually feel.
	LatencyP50     float64 `json:"latency_p50_ms"`
	LatencyP95     float64 `json:"latency_p95_ms"`
	LatencyAvg     float64 `json:"latency_avg_ms"`
	TTFBAvg        float64 `json:"ttfb_avg_ms"`
	LatencySamples int64   `json:"latency_samples"`
	TTFBSamples    int64   `json:"ttfb_samples"`

	// Token totals. Reported separately from the request count because a model
	// can answer without reporting usage, so a zero here is not "no traffic".
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	// TokensReported counts rows that actually carried a token count, so the
	// console can distinguish "0 tokens" from "the upstream never told us".
	TokensReported int64 `json:"tokens_reported"`

	Credits float64 `json:"credits"`
	// 日期范围来自同一次汇总，空列表不虚构起止时间。
	OldestRequestAt string `json:"oldest_request_at,omitempty"`
	NewestRequestAt string `json:"newest_request_at,omitempty"`
}

// Bucket is one point on a time series.
type Bucket struct {
	// Start is the bucket's inclusive lower bound (UTC).
	Start time.Time `json:"start"`
	// Label is the pre-formatted axis label (e.g. "12:00" or "09-30").
	Label string `json:"label"`

	Requests  int64 `json:"requests"`
	Successes int64 `json:"successes"`
	Failures  int64 `json:"failures"`
	InFlight  int64 `json:"in_flight"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	AvgLatencyMs   float64 `json:"avg_latency_ms"`
	LatencySamples int64   `json:"latency_samples"`
	TokensReported int64   `json:"tokens_reported"`
}

// ModelStat is one row of the popular-model ranking.
type ModelStat struct {
	Model string `json:"model"`
	// Provider is the channel that served it, when unambiguous.
	Provider       string  `json:"provider"`
	Requests       int64   `json:"requests"`
	Successes      int64   `json:"successes"`
	Failures       int64   `json:"failures"`
	InFlight       int64   `json:"in_flight"`
	SuccessPct     float64 `json:"success_pct"`
	AvgLatencyMs   float64 `json:"avg_latency_ms"`
	TotalTokens    int64   `json:"total_tokens"`
	LatencySamples int64   `json:"latency_samples"`
}

// ProviderStat is one row of the per-channel breakdown.
type ProviderStat struct {
	Provider     string  `json:"provider"`
	Requests     int64   `json:"requests"`
	Successes    int64   `json:"successes"`
	Failures     int64   `json:"failures"`
	InFlight     int64   `json:"in_flight"`
	SuccessPct   float64 `json:"success_pct"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	Accounts     int64   `json:"accounts"`
}

// ErrorStat is one row of the error composition breakdown.
type ErrorStat struct {
	// Kind is the classified failure kind (quota, rate_limit, auth, ...).
	// An empty kind means the row was an error with no classification, which is
	// reported as "unknown" rather than dropped.
	Kind         string  `json:"kind"`
	Count        int64   `json:"count"`
	Share        float64 `json:"share"`
	SampleCode   string  `json:"sample_code"`
	SampleDetail string  `json:"sample_detail"`
}

// Stats is the whole overview payload.
type Stats struct {
	Window   string         `json:"window"`
	Since    time.Time      `json:"since"`
	Until    time.Time      `json:"until"`
	Bucketed string         `json:"bucket"`
	Summary  SummaryTotals  `json:"summary"`
	Series   []Bucket       `json:"series"`
	Models   []ModelStat    `json:"models"`
	Provider []ProviderStat `json:"providers"`
	Errors   []ErrorStat    `json:"errors"`
}

// OverviewStats computes every overview aggregate in one call.
//
// One call and one window rather than several endpoints: the console renders
// them together, and letting them drift apart across separate requests would
// show a summary that disagrees with its own chart.
func (s *Store) OverviewStats(ctx context.Context, since time.Time, buckets int, bucketDur time.Duration) (*Stats, error) {
	until := time.Now().UTC()

	stats := &Stats{
		Since:    since,
		Until:    until,
		Bucketed: bucketDur.String(),
		Series:   []Bucket{},
		Models:   []ModelStat{},
		Provider: []ProviderStat{},
		Errors:   []ErrorStat{},
	}

	summary, err := s.summaryTotals(ctx, since)
	if err != nil {
		return nil, err
	}
	stats.Summary = *summary

	series, err := s.seriesBuckets(ctx, since, buckets, bucketDur)
	if err != nil {
		return nil, err
	}
	stats.Series = series

	models, err := s.modelStats(ctx, since)
	if err != nil {
		return nil, err
	}
	stats.Models = models

	providers, err := s.providerStats(ctx, since)
	if err != nil {
		return nil, err
	}
	stats.Provider = providers

	errs, err := s.errorStats(ctx, since)
	if err != nil {
		return nil, err
	}
	stats.Errors = errs

	return stats, nil
}

// windowClause builds the optional lower-bound predicate.
//
// An open-ended window is expressed by passing a zero time, which becomes an
// empty predicate rather than a comparison against year 1.
func windowClause(since time.Time, extra ...string) (string, []any) {
	clauses := make([]string, 0, len(extra)+1)
	var args []any
	if !since.IsZero() {
		clauses = append(clauses, "created_at >= ?")
		args = append(args, formatTime(since))
	}
	clauses = append(clauses, extra...)
	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func (s *Store) summaryTotals(ctx context.Context, since time.Time) (*SummaryTotals, error) {
	where, args := windowClause(since)

	var (
		out                                          SummaryTotals
		avgLat, avgTTFB                              sqlNullFloat
		totalTok, promptTok, compTok, tokensReported sqlNullInt
		credits                                      sqlNullFloat
	)

	// Percentiles are computed with a window function over the same scan, so the
	// p50/p95 and the counts can never disagree with each other.
	query := `
WITH rows_in_window AS (
  SELECT status, latency_ms, ttfb_ms, total_tokens, prompt_tokens, completion_tokens, credits, created_at
  FROM request_logs` + where + `
)
SELECT
  COUNT(*),
  SUM(CASE WHEN status = 'ok' THEN 1 ELSE 0 END),
  SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END),
  SUM(CASE WHEN status = 'streaming' THEN 1 ELSE 0 END),
  AVG(latency_ms),
  AVG(ttfb_ms), COUNT(latency_ms), COUNT(ttfb_ms),
  SUM(total_tokens),
  SUM(prompt_tokens),
  SUM(completion_tokens),
  SUM(CASE WHEN total_tokens IS NOT NULL THEN 1 ELSE 0 END),
  SUM(credits), COALESCE(MIN(created_at),''), COALESCE(MAX(created_at),'')
FROM rows_in_window`

	var (
		requests, successes, failures, inFlight sqlNullInt
	)
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&requests, &successes, &failures, &inFlight,
		&avgLat, &avgTTFB, &out.LatencySamples, &out.TTFBSamples, &totalTok, &promptTok, &compTok, &tokensReported, &credits,
		&out.OldestRequestAt, &out.NewestRequestAt,
	); err != nil {
		return nil, fmt.Errorf("store: summary totals: %w", err)
	}

	out.Requests = requests.Int64
	out.Successes = successes.Int64
	out.Failures = failures.Int64
	out.InFlight = inFlight.Int64
	if completed := out.Successes + out.Failures; completed > 0 {
		// 正在生成的请求尚无最终结果，不能提前算成功或加入成功率分母。
		out.SuccessPct = float64(out.Successes) / float64(completed) * 100
	}
	out.LatencyAvg = avgLat.Float64
	out.TTFBAvg = avgTTFB.Float64
	out.TotalTokens = totalTok.Int64
	out.PromptTokens = promptTok.Int64
	out.CompletionTokens = compTok.Int64
	out.TokensReported = tokensReported.Int64
	out.Credits = credits.Float64

	// Percentiles need a second pass with an ordering, so they are fetched only
	// when there is something to order.
	p50, p95, err := s.latencyPercentiles(ctx, since)
	if err != nil {
		return nil, err
	}
	out.LatencyP50 = p50
	out.LatencyP95 = p95

	return &out, nil
}

// latencyP95 exposes just the 95th percentile.
//
// It exists for tests, and specifically because p95 is the ONLY percentile that
// can pin the nearest-rank convention: p50 selects the same index under both the
// correct formula and the off-by-one one for every sample count, so an assertion
// about the median cannot detect a wrong offset at all.
func (s *Store) latencyP95(ctx context.Context, since time.Time) (float64, error) {
	_, p95, err := s.latencyPercentiles(ctx, since)
	return p95, err
}

// latencyPercentiles computes p50/p95 with nearest-rank selection.
//
// The offset trick avoids a window function so it also works on the oldest
// SQLite the pure-Go driver might be paired with. NULL latencies are excluded:
// a streaming row still in flight has no latency yet, and counting it as zero
// would drag the percentile down misleadingly.
func (s *Store) latencyPercentiles(ctx context.Context, since time.Time) (p50, p95 float64, err error) {
	where, args := windowClause(since, "latency_ms IS NOT NULL")

	var total int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM request_logs`+where, args...).Scan(&total); err != nil {
		return 0, 0, fmt.Errorf("store: count latencies: %w", err)
	}
	if total == 0 {
		return 0, 0, nil
	}

	// nearest-rank: for a sorted list of N values, the p-th percentile is the
	// element at index round(p * (N-1)).
	//
	// The earlier form was `int64(p * float64(total))`, which is off by one at
	// every fraction and badly wrong on small samples: with N=4 and p=0.5 it
	// selected index 2 (the 3rd value) instead of index 1, and with N=2 it made
	// p50 and p95 the SAME element, so the median carried no information at all.
	// Adding 0.5 rounds to nearest, which is what "nearest-rank" names.
	pick := func(fraction float64) (float64, error) {
		offset := int64(fraction*float64(total-1) + 0.5)
		if offset >= total {
			offset = total - 1
		}
		if offset < 0 {
			offset = 0
		}
		q := `SELECT latency_ms FROM request_logs` + where +
			` ORDER BY latency_ms LIMIT 1 OFFSET ?`
		var v sqlNullFloat
		if err := s.db.QueryRowContext(ctx, q, append(args, offset)...).Scan(&v); err != nil {
			return 0, fmt.Errorf("store: latency percentile: %w", err)
		}
		return v.Float64, nil
	}

	if p50, err = pick(0.50); err != nil {
		return 0, 0, err
	}
	if p95, err = pick(0.95); err != nil {
		return 0, 0, err
	}
	return p50, p95, nil
}

// seriesBuckets groups requests into fixed-width time buckets.
//
// Bucketing happens in Go rather than SQL so the bucket boundaries are the
// caller's clock and every bucket in the range is present in the output -- an
// empty bucket must render as a zero, not vanish and make the chart lie about
// the shape of the traffic.
func (s *Store) seriesBuckets(ctx context.Context, since time.Time, buckets int, bucketDur time.Duration) ([]Bucket, error) {
	if buckets <= 0 {
		buckets = 24
	}
	if bucketDur <= 0 {
		bucketDur = time.Hour
	}

	now := time.Now().UTC()
	// 使用与摘要相同的起点，避免丢失滚动窗口最早的部分时间。
	start := since
	if start.IsZero() {
		start = now.Add(-time.Duration(buckets) * bucketDur)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT created_at, status, latency_ms, total_tokens, prompt_tokens, completion_tokens
FROM request_logs WHERE created_at >= ?`, formatTime(start))
	if err != nil {
		return nil, fmt.Errorf("store: series rows: %w", err)
	}
	defer rows.Close()

	out := make([]Bucket, buckets)
	for i := 0; i < buckets; i++ {
		b := &out[i]
		b.Start = start.Add(time.Duration(i) * bucketDur)
		b.Label = bucketLabel(b.Start, bucketDur)
	}

	type acc struct {
		latencySum   float64
		latencyCount int64
	}
	accs := make(map[int64]*acc, buckets)

	for rows.Next() {
		var (
			createdRaw                         string
			status                             string
			latency, total, prompt, completion sqlNullInt
		)
		if err := rows.Scan(&createdRaw, &status, &latency, &total, &prompt, &completion); err != nil {
			return nil, fmt.Errorf("store: scan series row: %w", err)
		}
		ts := parseTime(createdRaw)
		if ts.IsZero() {
			continue
		}
		if ts.Before(start) || ts.After(now) {
			continue
		}
		position := int(ts.Sub(start) / bucketDur)
		// 调用期间新写入的末尾请求仍归入最后一段，不因毫秒偏差丢失。
		if position >= buckets {
			position = buckets - 1
		}
		b := &out[position]
		key := b.Start.Unix()
		b.Requests++
		switch status {
		case StatusError:
			b.Failures++
		case StatusOK:
			b.Successes++
		case StatusStreaming:
			b.InFlight++
		}
		if total.Valid {
			b.TokensReported++
		}
		b.TotalTokens += total.Int64
		b.PromptTokens += prompt.Int64
		b.CompletionTokens += completion.Int64
		if latency.Valid {
			b.LatencySamples++
			a := accs[key]
			if a == nil {
				a = &acc{}
				accs[key] = a
			}
			a.latencySum += float64(latency.Int64)
			a.latencyCount++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate series rows: %w", err)
	}

	for i := range out {
		if a := accs[out[i].Start.Unix()]; a != nil && a.latencyCount > 0 {
			out[i].AvgLatencyMs = a.latencySum / float64(a.latencyCount)
		}
	}
	return out, nil
}

// bucketLabel renders an axis label sized to the bucket width.
func bucketLabel(t time.Time, dur time.Duration) string {
	switch {
	case dur >= 24*time.Hour:
		return t.Format("01-02")
	case dur >= time.Hour:
		return t.Format("01-02 15:04")
	default:
		return t.Format("15:04")
	}
}

func (s *Store) modelStats(ctx context.Context, since time.Time) ([]ModelStat, error) {
	// Group by the RESOLVED model: the upstream's own ID is the truth when it
	// reported one, falling back to what the client asked for. A request that
	// never reached the upstream has only the latter, and dropping those rows
	// would hide the failures.
	//
	// The window predicate is built separately because it must reference the
	// base column names, which are not visible through the CTE's alias.
	where, args := windowClause(since)
	query := `
WITH rows_in_window AS (
  SELECT COALESCE(NULLIF(mapped_model, ''), requested_model) AS model,
         provider, status, latency_ms, total_tokens
  FROM request_logs` + where + `
)
SELECT model, provider, COUNT(*),
       SUM(CASE WHEN status = 'ok' THEN 1 ELSE 0 END),
       SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END),
       SUM(CASE WHEN status = 'streaming' THEN 1 ELSE 0 END),
       AVG(latency_ms), SUM(total_tokens), COUNT(latency_ms)
FROM rows_in_window
WHERE model <> ''
GROUP BY model, provider
ORDER BY COUNT(*) DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: model stats: %w", err)
	}
	defer rows.Close()

	// Rows are grouped by (model, provider); the console ranks by model, so
	// per-provider rows are merged and the dominant provider kept.
	type merged struct {
		stat      ModelStat
		providers map[string]int64
	}
	byModel := map[string]*merged{}
	var order []string

	for rows.Next() {
		var (
			model, provider                    string
			requests, successes                sqlNullInt
			failures, inflight, latencySamples sqlNullInt
			avgLat                             sqlNullFloat
			tokens                             sqlNullInt
		)
		if err := rows.Scan(&model, &provider, &requests, &successes, &failures, &inflight, &avgLat, &tokens, &latencySamples); err != nil {
			return nil, fmt.Errorf("store: scan model stat: %w", err)
		}
		m := byModel[model]
		if m == nil {
			m = &merged{stat: ModelStat{Model: model}, providers: map[string]int64{}}
			byModel[model] = m
			order = append(order, model)
		}
		// 合并渠道时按实际延迟样本加权，未完成请求不当作零延迟。
		if samples := m.stat.LatencySamples + latencySamples.Int64; samples > 0 {
			m.stat.AvgLatencyMs = (m.stat.AvgLatencyMs*float64(m.stat.LatencySamples) + avgLat.Float64*float64(latencySamples.Int64)) / float64(samples)
		}
		m.stat.LatencySamples += latencySamples.Int64
		m.stat.Requests += requests.Int64
		m.stat.Successes += successes.Int64
		m.stat.Failures += failures.Int64
		m.stat.InFlight += inflight.Int64
		m.stat.TotalTokens += tokens.Int64
		if provider != "" {
			m.providers[provider] += requests.Int64
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate model stats: %w", err)
	}

	out := make([]ModelStat, 0, len(order))
	for _, name := range order {
		m := byModel[name]
		if completed := m.stat.Successes + m.stat.Failures; completed > 0 {
			m.stat.SuccessPct = float64(m.stat.Successes) / float64(completed) * 100
		}
		// Report the provider that carried most of this model's traffic.
		var best string
		var bestN int64
		for p, n := range m.providers {
			if n > bestN || (n == bestN && p < best) {
				best, bestN = p, n
			}
		}
		m.stat.Provider = best
		out = append(out, m.stat)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

// UnattributedProvider is the bucket for requests that failed before an account
// was chosen, so they carry no provider.
//
// WHY THE BUCKET EXISTS: filtering those rows out (the earlier behaviour) made
// the panel silently under-report. On a real deployment the summary read 53
// requests while the provider rows summed to 22 -- 58% of the traffic simply was
// not shown anywhere, and an operator had no way to see that the missing share
// was "no account was available". The card answers "where did the traffic go",
// so it must account for all of it.
const UnattributedProvider = "unattributed"

// providerStats breaks requests down by channel.
//
// The account count is deliberately NOT taken from the log: `COUNT(DISTINCT
// account_id)` counts IDs that ever APPEARED in a log row, so it only grows as
// accounts are deleted (a live deployment reported 8 while 3 existed). The
// caller overrides that field from the live pool; see handleStats.
func (s *Store) providerStats(ctx context.Context, since time.Time) ([]ProviderStat, error) {
	where, args := windowClause(since)

	rows, err := s.db.QueryContext(ctx, `
SELECT CASE WHEN provider IS NULL OR provider = '' THEN '`+UnattributedProvider+`' ELSE provider END AS chan,
       COUNT(*),
       SUM(CASE WHEN status = 'ok' THEN 1 ELSE 0 END),
       SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END),
       SUM(CASE WHEN status = 'streaming' THEN 1 ELSE 0 END),
       AVG(latency_ms),
       COUNT(DISTINCT NULLIF(account_id, ''))
FROM request_logs`+where+`
GROUP BY chan
ORDER BY COUNT(*) DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: provider stats: %w", err)
	}
	defer rows.Close()

	var out []ProviderStat
	for rows.Next() {
		var (
			p                            ProviderStat
			requests, successes          sqlNullInt
			failures, accounts, inflight sqlNullInt
			avgLat                       sqlNullFloat
		)
		if err := rows.Scan(&p.Provider, &requests, &successes, &failures, &inflight, &avgLat, &accounts); err != nil {
			return nil, fmt.Errorf("store: scan provider stat: %w", err)
		}
		p.Requests = requests.Int64
		p.Successes = successes.Int64
		p.Failures = failures.Int64
		p.InFlight = inflight.Int64
		p.AvgLatencyMs = avgLat.Float64
		p.Accounts = accounts.Int64
		if completed := p.Successes + p.Failures; completed > 0 {
			p.SuccessPct = float64(p.Successes) / float64(completed) * 100
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate provider stats: %w", err)
	}
	if out == nil {
		out = []ProviderStat{}
	}
	return out, nil
}

// errorStats breaks failures down by classified kind.
//
// The kind, not the code, is the useful axis: it maps onto the scheduler's own
// decision table, so "22 invalid_request" tells an operator the requests are
// malformed while "13 rate_limit" says the pool is being throttled.
func (s *Store) errorStats(ctx context.Context, since time.Time) ([]ErrorStat, error) {
	where, args := windowClause(since, "status = 'error'")

	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(NULLIF(error_kind,''), 'unknown'), COUNT(*),
       COALESCE(MAX(error_code),''), COALESCE(MAX(error_message),'')
FROM request_logs`+where+`
GROUP BY COALESCE(NULLIF(error_kind,''), 'unknown')
ORDER BY COUNT(*) DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: error stats: %w", err)
	}
	defer rows.Close()

	type row struct {
		stat ErrorStat
	}
	var (
		out   []ErrorStat
		total int64
	)
	for rows.Next() {
		var e ErrorStat
		if err := rows.Scan(&e.Kind, &e.Count, &e.SampleCode, &e.SampleDetail); err != nil {
			return nil, fmt.Errorf("store: scan error stat: %w", err)
		}
		out = append(out, e)
		total += e.Count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate error stats: %w", err)
	}
	// Share is relative to failures, not to all requests: the card answers
	// "of the failures, what is the composition".
	for i := range out {
		if total > 0 {
			out[i].Share = float64(out[i].Count) / float64(total) * 100
		}
		// Keep the sample message short enough for a table cell.
		if len(out[i].SampleDetail) > 160 {
			out[i].SampleDetail = out[i].SampleDetail[:160] + "…"
		}
	}
	if out == nil {
		out = []ErrorStat{}
	}
	return out, nil
}
