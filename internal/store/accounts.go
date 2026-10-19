package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Account is the durable record of one upstream account.
//
// Runtime scheduling state (in-flight counters, live cooldown deadlines) lives
// in the in-memory pool, not here. Only what must survive a restart is stored.
//
// NOTE on the armor_preset column: migration 003 added it when a preset could be
// bound to an account. Selection is now by requested MODEL (see internal/armor),
// so the column is no longer read or written. It is deliberately NOT dropped:
// the migration checksum guard makes editing an applied migration fatal, and
// dropping a column would buy nothing but a schema-history risk.
type Account struct {
	ID        string
	Name      string
	RemoteUID string
	Provider  string // workbuddy | qoder | trae
	Region    string // cn | global
	AuthType  string
	// 只读凭证元数据；查询账号不解密、不返回凭证正文。
	HasCredential       bool
	CredentialExpiresAt time.Time
	Enabled             bool
	// MaxInflight caps concurrent requests routed to this account. Zero
	// means "use the pool default".
	MaxInflight int
	Priority    int
	// Weight biases weighted-round-robin. It is NOT a hard quota: with equal
	// weights the pool degrades to plain round-robin.
	Weight        int
	Status        string
	LastError     string
	LastErrorKind string
	CooldownUntil time.Time
	// QuotaScope 为 shared、account 或 model，决定额度耗尽后是否换号。
	QuotaScope string
	QuotaGroup string
	// QuotaExpiresAt 是额度包到期时间，与登录令牌有效期无关；零值表示未知。
	QuotaExpiresAt time.Time
	Runtime        AccountRuntime

	// DropSystemPrompt strips the caller's system message before relaying.
	// Defaults on: a client-controlled system prompt is the usual source of
	// upstream content-filter rejections.
	DropSystemPrompt bool
	AutoCheckin      bool

	LastCheckinAt     time.Time
	LastCheckinStatus string
	LastCheckinMsg    string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Credential is the opaque provider credential blob plus the metadata needed
// to display it. Format tells the provider package how to decode payload.
type Credential struct {
	AccountID    string
	Provider     string
	Region       string
	Format       string
	AuthType     string
	Payload      []byte
	Nickname     string
	Domain       string
	EnterpriseID string
	ExpiresAt    time.Time
	UpdatedAt    time.Time
}

// NewAccountID returns a fresh acc_<12 hex> identifier.
func NewAccountID() (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("store: generate account id: %w", err)
	}
	return "acc_" + hex.EncodeToString(raw[:]), nil
}

const accountColumns = `id, name, remote_uid, provider, provider_region, auth_type, enabled,
  max_inflight, priority, weight, status, last_error, last_error_kind, cooldown_until,
  drop_system_prompt, auto_checkin, last_checkin_at, last_checkin_status, last_checkin_msg,
  armor_preset, created_at, updated_at, pool_runtime, quota_scope, quota_group, quota_expires_at`

const accountSelectColumns = accountColumns + `, EXISTS(SELECT 1 FROM account_credentials WHERE account_id=accounts.id),
  (SELECT expires_at FROM credential_metadata WHERE account_id=accounts.id)`

// CreateAccount inserts a new account row and returns it as stored.
//
// It RETURNS the stored account rather than mutating the caller's value. The
// earlier signature took Account by value, so a generated ID never reached the
// caller, and the API layer had to re-read "the newest row with this
// provider and region" to learn the ID. That re-read is racy: two concurrent
// creates of the same provider and region could both resolve to the same row,
// and a credential attached at creation time would be written against an empty
// account ID.
func (s *Store) CreateAccount(ctx context.Context, a Account) (Account, error) {
	if strings.TrimSpace(a.ID) == "" {
		id, err := NewAccountID()
		if err != nil {
			return Account{}, err
		}
		a.ID = id
	}
	if strings.TrimSpace(a.Name) == "" {
		a.Name = a.ID
	}
	if strings.TrimSpace(a.Provider) == "" {
		a.Provider = "workbuddy"
	}
	if strings.TrimSpace(a.Region) == "" {
		a.Region = "cn"
	}
	if a.MaxInflight < 0 {
		return Account{}, fmt.Errorf("账号并发上限不能为负数")
	}
	if a.QuotaScope == "" {
		a.QuotaScope = "shared"
	}
	if !validQuotaScope(a.QuotaScope) {
		return Account{}, fmt.Errorf("额度范围必须是 shared、account 或 model")
	}
	if a.Weight <= 0 {
		if a.Weight < 0 {
			return Account{}, fmt.Errorf("账号权重不能为负数")
		}
		a.Weight = 50
	}
	if a.Weight > 1000000 {
		return Account{}, fmt.Errorf("账号权重不能超过 1000000")
	}
	if strings.TrimSpace(a.Status) == "" {
		a.Status = "offline"
	}
	stamp := now()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO accounts (`+accountColumns+`)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.RemoteUID, a.Provider, a.Region, a.AuthType, boolToInt(a.Enabled),
		a.MaxInflight, a.Priority, a.Weight, a.Status, a.LastError, a.LastErrorKind,
		formatTime(a.CooldownUntil), boolToInt(a.DropSystemPrompt), boolToInt(a.AutoCheckin),
		// The last three values are last_checkin_at/status/msg, then
		// armor_preset and the timestamps. armor_preset is written empty
		// because selection is by model now; the column is kept so the schema
		// history stays untouched.
		"", "", "", "", stamp, stamp, "{}", a.QuotaScope, a.QuotaGroup, formatTime(a.QuotaExpiresAt))
	if err != nil {
		return Account{}, fmt.Errorf("store: create account %s: %w", a.ID, err)
	}
	// Report back the row as stored so the caller has the generated ID and the
	// normalized defaults without a second query.
	a.CreatedAt = parseTime(stamp)
	a.UpdatedAt = a.CreatedAt
	return a, nil
}

// UpdateAccount writes back the mutable account fields.
//
// Cooldown, status and last-error are deliberately excluded: those are
// runtime facts owned by the pool, and persisting a stale copy here would let
// a restart resurrect a cooldown that already expired.
func (s *Store) UpdateAccount(ctx context.Context, a Account) error {
	if a.MaxInflight < 0 || a.Weight < 1 || a.Weight > 1000000 || !validQuotaScope(a.QuotaScope) {
		return fmt.Errorf("账号并发、权重或额度范围无效")
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE accounts SET
  name = ?, remote_uid = ?, provider = ?, provider_region = ?, auth_type = ?,
  enabled = ?, max_inflight = ?, priority = ?, weight = ?,
  drop_system_prompt = ?, auto_checkin = ?, updated_at = ?, quota_scope = ?, quota_group = ?, quota_expires_at = ?
WHERE id = ?`,
		a.Name, a.RemoteUID, a.Provider, a.Region, a.AuthType,
		boolToInt(a.Enabled), a.MaxInflight, a.Priority, a.Weight,
		boolToInt(a.DropSystemPrompt), boolToInt(a.AutoCheckin), now(), a.QuotaScope, a.QuotaGroup, formatTime(a.QuotaExpiresAt), a.ID)
	if err != nil {
		return fmt.Errorf("store: update account %s: %w", a.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: update account %s: %w", a.ID, ErrNotFound)
	}
	return nil
}

// SetAccountEnabled flips the enabled flag without touching other fields.
func (s *Store) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), now(), id)
	if err != nil {
		return fmt.Errorf("store: set account %s enabled: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: set account %s enabled: %w", id, ErrNotFound)
	}
	return nil
}

// DeleteAccount removes an account. Credentials cascade via foreign key.
func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: delete account %s: %w", id, ErrNotFound)
	}
	return nil
}

// GetAccount loads one account by ID.
func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+accountSelectColumns+` FROM accounts WHERE id = ?`, id)
	a, err := scanAccount(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Account{}, fmt.Errorf("store: account %s: %w", id, ErrNotFound)
		}
		return Account{}, fmt.Errorf("store: get account %s: %w", id, err)
	}
	return a, nil
}

// ListAccounts returns every account, enabled and disabled alike, ordered so
// the console shows a stable list.
func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountSelectColumns+` FROM accounts ORDER BY provider, provider_region, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan account: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate accounts: %w", err)
	}
	return out, nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanAccount(sc scanner) (Account, error) {
	var (
		a           Account
		enabled     int
		dropSystem  int
		autoCheckin int
		cooldownRaw sql.NullString
		lastCheckin string
		createdRaw  string
		updatedRaw  string
		// armorPreset is the legacy account-level preset column. It is scanned
		// into a discarded variable because the column still exists (the
		// migration is immutable) but selection is by model now.
		armorPreset          string
		runtimeRaw           string
		quotaExpiresRaw      string
		credentialExpiresRaw sql.NullString
	)
	if err := sc.Scan(
		&a.ID, &a.Name, &a.RemoteUID, &a.Provider, &a.Region, &a.AuthType, &enabled,
		&a.MaxInflight, &a.Priority, &a.Weight, &a.Status, &a.LastError, &a.LastErrorKind,
		&cooldownRaw, &dropSystem, &autoCheckin, &lastCheckin, &a.LastCheckinStatus,
		&a.LastCheckinMsg, &armorPreset, &createdRaw, &updatedRaw, &runtimeRaw, &a.QuotaScope, &a.QuotaGroup, &quotaExpiresRaw, &a.HasCredential, &credentialExpiresRaw,
	); err != nil {
		return Account{}, err
	}
	if err := json.Unmarshal([]byte(runtimeRaw), &a.Runtime); err != nil {
		return Account{}, fmt.Errorf("账号 %s 的号池状态损坏：%w", a.ID, err)
	}
	a.Enabled = enabled != 0
	a.QuotaExpiresAt = parseTime(quotaExpiresRaw)
	a.CredentialExpiresAt = parseTime(credentialExpiresRaw.String)
	a.DropSystemPrompt = dropSystem != 0
	a.AutoCheckin = autoCheckin != 0
	if cooldownRaw.Valid {
		a.CooldownUntil = parseTime(cooldownRaw.String)
	}
	a.LastCheckinAt = parseTime(lastCheckin)
	a.CreatedAt = parseTime(createdRaw)
	a.UpdatedAt = parseTime(updatedRaw)
	return a, nil
}

// UpdateAccountRuntime persists the pool's runtime health for one account.
//
// Only the three runtime columns are written: status, error and cooldown. The
// configuration columns are owned by the admin API and must not be clobbered by
// a scheduling update racing an operator's edit.
func (s *Store) UpdateAccountRuntime(ctx context.Context, id string, cooldownUntil time.Time, lastError, lastKind string) error {
	status := "ready"
	if !cooldownUntil.IsZero() && cooldownUntil.After(time.Now()) {
		status = "cooldown"
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE accounts SET
  cooldown_until = ?, last_error = ?, last_error_kind = ?, status = ?, updated_at = ?
WHERE id = ?`,
		formatTime(cooldownUntil), lastError, lastKind, status, now(), id)
	if err != nil {
		return fmt.Errorf("store: update runtime state for %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: update runtime state for %s: %w", id, ErrNotFound)
	}
	return nil
}

// UpsertCredential stores the credential blob and its display metadata.
func (s *Store) UpsertCredential(ctx context.Context, c Credential) error {
	if c.AuthType != "" && c.AuthType != "api_key" && c.AuthType != "oauth" {
		return fmt.Errorf("凭证类型无效")
	}
	payload, err := s.encryptCredential(c.AccountID, c.Payload)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin upsert credential: %w", err)
	}
	defer tx.Rollback()

	stamp := now()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO account_credentials (account_id, format, payload, updated_at)
VALUES (?,?,?,?)
ON CONFLICT(account_id) DO UPDATE SET
  format = excluded.format, payload = excluded.payload, updated_at = excluded.updated_at`,
		c.AccountID, c.Format, payload, stamp); err != nil {
		return fmt.Errorf("store: upsert credential for %s: %w", c.AccountID, err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO credential_metadata (account_id, nickname, domain, enterprise_id, expires_at, updated_at)
VALUES (?,?,?,?,?,?)
ON CONFLICT(account_id) DO UPDATE SET
  nickname = excluded.nickname, domain = excluded.domain,
  enterprise_id = excluded.enterprise_id, expires_at = excluded.expires_at,
  updated_at = excluded.updated_at`,
		c.AccountID, c.Nickname, c.Domain, c.EnterpriseID, formatTime(c.ExpiresAt), stamp); err != nil {
		return fmt.Errorf("store: upsert credential metadata for %s: %w", c.AccountID, err)
	}
	// 凭证和类型同一事务保存，避免替换到一半导致管理页与真实鉴权方式不一致。
	if c.AuthType != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET auth_type=?, updated_at=? WHERE id=?`, c.AuthType, stamp, c.AccountID); err != nil {
			return fmt.Errorf("更新凭证类型失败：%w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit upsert credential for %s: %w", c.AccountID, err)
	}
	return nil
}

// GetCredential loads one credential blob.
func (s *Store) GetCredential(ctx context.Context, accountID string) (Credential, error) {
	out := Credential{AccountID: accountID}
	// updated_at is a TEXT column holding an RFC3339 string, so it must be
	// scanned into a string and parsed. Scanning it straight into a time.Time
	// fails at runtime, not at compile time: the driver hands back a string and
	// database/sql refuses the conversion.
	var updatedRaw, expiresRaw string
	err := s.db.QueryRowContext(ctx, `
SELECT c.format, c.payload, c.updated_at,
       COALESCE(m.nickname,''), COALESCE(m.domain,''), COALESCE(m.enterprise_id,''), COALESCE(m.expires_at,''),
       a.provider, a.provider_region
FROM account_credentials c
JOIN accounts a ON a.id = c.account_id
LEFT JOIN credential_metadata m ON m.account_id = c.account_id
WHERE c.account_id = ?`, accountID).Scan(
		&out.Format, &out.Payload, &updatedRaw,
		&out.Nickname, &out.Domain, &out.EnterpriseID, &expiresRaw, &out.Provider, &out.Region)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Credential{}, fmt.Errorf("store: credential for %s: %w", accountID, ErrNotFound)
		}
		return Credential{}, fmt.Errorf("store: get credential for %s: %w", accountID, err)
	}
	out.UpdatedAt = parseTime(updatedRaw)
	out.Payload, err = s.decryptCredential(accountID, out.Payload)
	if err != nil {
		return Credential{}, err
	}
	out.ExpiresAt = parseTime(expiresRaw)
	return out, nil
}

// DeleteCredential removes a credential blob, leaving the account in place.
func (s *Store) DeleteCredential(ctx context.Context, accountID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM account_credentials WHERE account_id = ?`, accountID); err != nil {
		return fmt.Errorf("store: delete credential for %s: %w", accountID, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM credential_metadata WHERE account_id = ?`, accountID); err != nil {
		return fmt.Errorf("store: delete credential metadata for %s: %w", accountID, err)
	}
	return nil
}

// RecordCheckin appends a check-in result and updates the account's summary
// columns in one transaction.
func (s *Store) RecordCheckin(ctx context.Context, accountID, status, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin record checkin: %w", err)
	}
	defer tx.Rollback()

	id, err := NewAccountID()
	if err != nil {
		return err
	}
	stamp := now()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO checkin_records (id, account_id, status, message, created_at) VALUES (?,?,?,?,?)`,
		"ck_"+strings.TrimPrefix(id, "acc_"), accountID, status, message, stamp); err != nil {
		return fmt.Errorf("store: insert checkin record: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE accounts SET last_checkin_at = ?, last_checkin_status = ?, last_checkin_msg = ?, updated_at = ?
WHERE id = ?`, stamp, status, message, stamp, accountID); err != nil {
		return fmt.Errorf("store: update account checkin summary: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit checkin: %w", err)
	}
	return nil
}

// LastCheckinAt reports the timestamp of an account's most recent successful
// check-in. It is used to skip duplicate attempts within the same local day.
func (s *Store) LastCheckinAt(ctx context.Context, accountID string) (time.Time, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(created_at) FROM checkin_records WHERE account_id = ? AND status IN ('success','already')`,
		accountID).Scan(&raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: last checkin for %s: %w", accountID, err)
	}
	if !raw.Valid {
		return time.Time{}, nil
	}
	return parseTime(raw.String), nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
