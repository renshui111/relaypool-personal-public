package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RequestStatus values stored in request_logs.status.
const (
	StatusOK        = "ok"
	StatusStreaming = "streaming"
	StatusError     = "error"
)

// RequestLog is one gateway request, written once the outcome is known.
//
// The row is created before the upstream call and finalized afterwards, which
// is why FinishRequest is separate from StartRequest: a streaming response can
// run for minutes, and an operator watching the console should see it appear
// immediately rather than only after it completes.
type RequestLog struct {
	ID       string
	APIKeyID string // 仅记录授权编号，用于用户自己的用量；禁止保存明文密钥。
	Protocol string // chat.completions | messages | responses

	RequestedModel string
	MappedModel    string

	AccountID string
	Provider  string
	Routing   string

	Stream bool
	Status string

	PromptTokens     *int
	CompletionTokens *int
	TotalTokens      *int
	Credits          *float64

	LatencyMs *int
	TTFBMs    *int

	ErrorKind    string
	ErrorCode    string
	ErrorMessage string

	AttemptCount int
	// 空值表示旧日志未记录；none 表示本次没有注入，其他值为实际预设名。
	ArmorPreset string

	CreatedAt  time.Time
	FinishedAt time.Time
}

// RequestAttempt is one account attempt inside a request. A request that
// failed over three times produces three of these, which is what makes
// "why did this request bounce around" answerable after the fact.
type RequestAttempt struct {
	ID           string
	RequestID    string
	AttemptIndex int

	AccountID string
	Provider  string

	Status       string
	ErrorKind    string
	ErrorCode    string
	ErrorMessage string

	LatencyMs  *int
	StartedAt  time.Time
	FinishedAt time.Time
}

// 请求编号使用 128 位随机数，避免高调用量下碰撞。
func NewRequestID() (string, error) {
	body, err := NewAPIKeyValue()
	if err != nil {
		return "", err
	}
	return "req_" + body[:32], nil
}

// StartRequest inserts the initial request row so a long stream is visible in
// the console while it is still running.
func (s *Store) StartRequest(ctx context.Context, r RequestLog) error {
	if strings.TrimSpace(r.ID) == "" {
		id, err := NewRequestID()
		if err != nil {
			return err
		}
		r.ID = id
	}
	if strings.TrimSpace(r.Status) == "" {
		r.Status = StatusStreaming
	}
	if strings.TrimSpace(r.Protocol) == "" {
		r.Protocol = "chat.completions"
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO request_logs (
  id, created_at, status, stream, protocol, requested_model, mapped_model,
  account_id, provider, routing, attempt_count, api_key_id
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, now(), r.Status, boolToInt(r.Stream), r.Protocol,
		r.RequestedModel, r.MappedModel, r.AccountID, r.Provider, r.Routing, r.AttemptCount, r.APIKeyID)
	if err != nil {
		return fmt.Errorf("store: start request %s: %w", r.ID, err)
	}
	return nil
}

// FinishRequest finalizes a request row with its outcome.
func (s *Store) FinishRequest(ctx context.Context, r RequestLog) error {
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("store: finish request requires an id")
	}
	finished := r.FinishedAt
	if finished.IsZero() {
		finished = time.Now().UTC()
	}
	status := r.Status
	if strings.TrimSpace(status) == "" {
		status = StatusOK
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE request_logs SET
  finished_at = ?, status = ?, stream = ?, mapped_model = ?, account_id = ?, provider = ?,
  routing = ?, prompt_tokens = ?, completion_tokens = ?, total_tokens = ?, credits = ?,
  latency_ms = ?, ttfb_ms = ?, error_kind = ?, error_code = ?, error_message = ?, attempt_count = ?, armor_preset = ?
WHERE id = ?`,
		formatTime(finished), status, boolToInt(r.Stream), r.MappedModel, r.AccountID, r.Provider,
		r.Routing, r.PromptTokens, r.CompletionTokens, r.TotalTokens, r.Credits,
		r.LatencyMs, r.TTFBMs, r.ErrorKind, r.ErrorCode, r.ErrorMessage, r.AttemptCount, r.ArmorPreset, r.ID)
	if err != nil {
		return fmt.Errorf("store: finish request %s: %w", r.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: finish request %s: %w", r.ID, ErrNotFound)
	}
	return nil
}

// RecordAttempt appends one account attempt. Failures here are non-fatal for
// the caller's request path: diagnostics must never break serving.
func (s *Store) RecordAttempt(ctx context.Context, a RequestAttempt) error {
	if strings.TrimSpace(a.RequestID) == "" {
		return fmt.Errorf("store: attempt requires a request id")
	}
	if strings.TrimSpace(a.ID) == "" {
		id, err := NewRequestID()
		if err != nil {
			return err
		}
		a.ID = "att_" + strings.TrimPrefix(id, "req_")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO request_attempts (
  id, request_id, attempt_index, account_id, provider, status,
  error_kind, error_code, error_message, latency_ms, started_at, finished_at
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.RequestID, a.AttemptIndex, a.AccountID, a.Provider, a.Status,
		a.ErrorKind, a.ErrorCode, a.ErrorMessage, a.LatencyMs,
		formatTime(a.StartedAt), formatTime(a.FinishedAt))
	if err != nil {
		return fmt.Errorf("store: record attempt for %s: %w", a.RequestID, err)
	}
	return nil
}

// RequestLogFilter narrows a request log query.
type RequestLogFilter struct {
	AccountID string
	Provider  string
	Status    string
	Limit     int
	Offset    int
}

// ListRequests returns request rows newest first.
func (s *Store) ListRequests(ctx context.Context, f RequestLogFilter) ([]RequestLog, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	var (
		clauses []string
		args    []any
	)
	if v := strings.TrimSpace(f.AccountID); v != "" {
		clauses = append(clauses, "account_id = ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(f.Provider); v != "" {
		clauses = append(clauses, "provider = ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(f.Status); v != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, v)
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, `
SELECT id, created_at, finished_at, status, stream, protocol, requested_model, mapped_model,
       COALESCE(account_id,''), provider, routing,
       prompt_tokens, completion_tokens, total_tokens, credits,
       latency_ms, ttfb_ms, error_kind, error_code, error_message, attempt_count, armor_preset
FROM request_logs`+where+`
ORDER BY created_at DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list requests: %w", err)
	}
	defer rows.Close()

	var out []RequestLog
	for rows.Next() {
		var (
			r                         RequestLog
			stream                    int
			createdRaw                string
			finishedRaw               sqlNullString
			prompt, completion, total sqlNullInt
			credits                   sqlNullFloat
			latency, ttfb             sqlNullInt
		)
		if err := rows.Scan(
			&r.ID, &createdRaw, &finishedRaw, &r.Status, &stream, &r.Protocol,
			&r.RequestedModel, &r.MappedModel, &r.AccountID, &r.Provider, &r.Routing,
			&prompt, &completion, &total, &credits, &latency, &ttfb,
			&r.ErrorKind, &r.ErrorCode, &r.ErrorMessage, &r.AttemptCount, &r.ArmorPreset,
		); err != nil {
			return nil, fmt.Errorf("store: scan request: %w", err)
		}
		r.Stream = stream != 0
		r.CreatedAt = parseTime(createdRaw)
		r.FinishedAt = parseTime(finishedRaw.String)
		r.PromptTokens = prompt.Ptr()
		r.CompletionTokens = completion.Ptr()
		r.TotalTokens = total.Ptr()
		r.Credits = credits.Ptr()
		r.LatencyMs = latency.Ptr()
		r.TTFBMs = ttfb.Ptr()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate requests: %w", err)
	}
	return out, nil
}

// ListAttempts returns the attempts belonging to one request, in order.
func (s *Store) ListAttempts(ctx context.Context, requestID string) ([]RequestAttempt, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, request_id, attempt_index, account_id, provider, status,
       error_kind, error_code, error_message, latency_ms, started_at, finished_at
FROM request_attempts WHERE request_id = ? ORDER BY attempt_index`, requestID)
	if err != nil {
		return nil, fmt.Errorf("store: list attempts for %s: %w", requestID, err)
	}
	defer rows.Close()

	var out []RequestAttempt
	for rows.Next() {
		var (
			a           RequestAttempt
			latency     sqlNullInt
			startedRaw  string
			finishedRaw string
		)
		if err := rows.Scan(&a.ID, &a.RequestID, &a.AttemptIndex, &a.AccountID, &a.Provider,
			&a.Status, &a.ErrorKind, &a.ErrorCode, &a.ErrorMessage, &latency,
			&startedRaw, &finishedRaw); err != nil {
			return nil, fmt.Errorf("store: scan attempt: %w", err)
		}
		a.LatencyMs = latency.Ptr()
		a.StartedAt = parseTime(startedRaw)
		a.FinishedAt = parseTime(finishedRaw)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate attempts: %w", err)
	}
	return out, nil
}

// PruneRequests deletes request logs older than the retention window.
// Attachments cascade. Returns the number of requests removed.
func (s *Store) PruneRequests(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-olderThan)
	// 先用索引缩小范围，再按时间比较；分批释放 SQLite 写锁。
	upper := cutoff.Truncate(time.Second).Add(time.Second).Format(time.RFC3339)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE id IN (
SELECT id FROM request_logs WHERE created_at < ? AND julianday(created_at)<julianday(?) ORDER BY created_at LIMIT 500)`, upper, formatTime(cutoff))
		if err != nil {
			return total, fmt.Errorf("store: prune requests: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < 500 {
			return total, nil
		}
	}
}

// usageByAccount is a lightweight aggregate for the console summary cards.
type AccountUsage struct {
	AccountID    string
	Requests     int64
	Successes    int64
	Failures     int64
	AvgLatencyMs float64
}

// AccountUsageSince aggregates request outcomes per account over a window.
func (s *Store) AccountUsageSince(ctx context.Context, since time.Time) ([]AccountUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(account_id,''), COUNT(*),
       SUM(CASE WHEN status IN ('ok','streaming') THEN 1 ELSE 0 END),
       SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END),
       COALESCE(AVG(latency_ms), 0)
FROM request_logs
WHERE created_at >= ? AND account_id IS NOT NULL AND account_id <> ''
GROUP BY account_id`, formatTime(since))
	if err != nil {
		return nil, fmt.Errorf("store: account usage: %w", err)
	}
	defer rows.Close()

	var out []AccountUsage
	for rows.Next() {
		var u AccountUsage
		if err := rows.Scan(&u.AccountID, &u.Requests, &u.Successes, &u.Failures, &u.AvgLatencyMs); err != nil {
			return nil, fmt.Errorf("store: scan account usage: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate account usage: %w", err)
	}
	return out, nil
}

// sqlNullInt and sqlNullFloat are tiny adapters so a NULL column maps to a
// nil pointer in the API layer. A NULL token count means "upstream did not
// report it", which is different from zero.
type sqlNullInt struct {
	Valid bool
	Int64 int64
}

func (n *sqlNullInt) Scan(v any) error {
	if v == nil {
		n.Valid = false
		return nil
	}
	switch t := v.(type) {
	case int64:
		n.Int64, n.Valid = t, true
	case []byte:
		var parsed int64
		if _, err := fmt.Sscanf(string(t), "%d", &parsed); err == nil {
			n.Int64, n.Valid = parsed, true
		}
	case string:
		var parsed int64
		if _, err := fmt.Sscanf(t, "%d", &parsed); err == nil {
			n.Int64, n.Valid = parsed, true
		}
	}
	return nil
}

func (n sqlNullInt) Ptr() *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

type sqlNullFloat struct {
	Valid   bool
	Float64 float64
}

func (n *sqlNullFloat) Scan(v any) error {
	if v == nil {
		n.Valid = false
		return nil
	}
	switch t := v.(type) {
	case float64:
		n.Float64, n.Valid = t, true
	case int64:
		n.Float64, n.Valid = float64(t), true
	case []byte:
		var parsed float64
		if _, err := fmt.Sscanf(string(t), "%g", &parsed); err == nil {
			n.Float64, n.Valid = parsed, true
		}
	}
	return nil
}

func (n sqlNullFloat) Ptr() *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}

type sqlNullString struct {
	Valid  bool
	String string
}

func (n *sqlNullString) Scan(v any) error {
	if v == nil {
		n.Valid = false
		return nil
	}
	switch t := v.(type) {
	case string:
		n.String, n.Valid = t, true
	case []byte:
		n.String, n.Valid = string(t), true
	}
	return nil
}
