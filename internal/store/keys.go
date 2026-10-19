package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// SecretAPIKey is the app_secrets row holding the bootstrap API key, so the
// generated key survives a restart instead of being reissued on every boot.
// 正式服务通过独立凭证密钥加密此值，调用密钥仍仅保留哈希。
const SecretAPIKey = "bootstrap_api_key"

// FormatAPIKey builds a key with a recognizable prefix. The prefix lets a
// secret scanner and a human both tell a relaypool key apart from an OpenAI
// key, which matters because the two look alike on the wire.
func FormatAPIKey(body string) string { return "rp_" + body }

// NewAPIKeyValue returns a fresh random key body (no prefix).
func NewAPIKeyValue() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("store: generate api key: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// HashAPIKey returns the stored representation of a key.
//
// A plain SHA-256 is correct here, unlike for user passwords: the key is 192
// bits of machine-generated entropy, so it is not brute-forceable and there is
// no rainbow-table risk. Using a slow KDF would only add latency to every
// gateway request.
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])
}

// KeyPrefix 仅展示正常密钥的前缀；兼容的短密钥必须全部遮蔽。
func KeyPrefix(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 8 {
		return "[已隐藏]"
	}
	return key[:8]
}

// 旧版短前缀可能就是完整密钥，读取时遮蔽，保留原哈希和撤销状态。
func safeStoredKeyPrefix(prefix, hash string) string {
	if HashAPIKey(prefix) == hash {
		return "[已隐藏]"
	}
	return prefix
}

const (
	KeyScopeAdmin     = "admin"
	KeyScopeInference = "inference"
)

// APIKey 保存密钥的展示信息和权限，不包含完整明文或哈希。
type APIKey struct {
	ID          string
	Name        string
	Scope       string
	KeyPrefix   string
	Enabled     bool
	CreatedAt   string
	LastUsed    string
	Permissions KeyPermissions
}

// EnsureBootstrapAPIKey returns the configured key, creating and persisting a
// random one when the operator did not supply one.
//
// The bool reports whether the key was newly generated, which is the caller's
// 启动日志不得打印明文；本机管理员可通过明确的获取命令读取初始密钥。
func (s *Store) EnsureBootstrapAPIKey(ctx context.Context, configured string) (string, bool, error) {
	key := strings.TrimSpace(configured)
	if key != "" {
		// An explicit key still gets a hash row so it can be listed and
		// revoked from the console like any other key.
		if err := s.ensureAPIKeyRow(ctx, key, "bootstrap"); err != nil {
			return "", false, err
		}
		return key, false, nil
	}

	stored, err := s.GetSecret(ctx, SecretAPIKey)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "", false, err
	}
	if stored != "" {
		return stored, false, nil
	}

	body, err := NewAPIKeyValue()
	if err != nil {
		return "", false, err
	}
	key = FormatAPIKey(body)
	if err := s.SetSecret(ctx, SecretAPIKey, key); err != nil {
		return "", false, err
	}
	if err := s.ensureAPIKeyRow(ctx, key, "bootstrap"); err != nil {
		return "", false, err
	}
	return key, true, nil
}

func (s *Store) ensureAPIKeyRow(ctx context.Context, key, name string) error {
	return s.ensureAPIKeyRowWithScope(ctx, key, name, KeyScopeAdmin)
}

// 已有记录不重新启用，也不通过重复导入提升权限。
func (s *Store) ensureAPIKeyRowWithScope(ctx context.Context, key, name, scope string) error {
	return s.ensureAPIKeyRowWithPermissions(ctx, key, name, scope, DefaultKeyPermissions())
}

func (s *Store) ensureAPIKeyRowWithPermissions(ctx context.Context, key, name, scope string, p KeyPermissions) error {
	hash := HashAPIKey(key)
	var existing string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM api_keys WHERE key_hash = ?`, hash).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: look up api key: %w", err)
	}
	id, err := NewAccountID()
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO api_keys (id, name, key_hash, key_prefix, enabled, created_at, last_used_at, scope, permissions)
VALUES (?,?,?,?,1,?,'',?,?)`,
		"key_"+strings.TrimPrefix(id, "acc_"), name, hash, KeyPrefix(key), now(), scope, permissionJSON(p)); err != nil {
		return fmt.Errorf("store: insert api key: %w", err)
	}
	return nil
}

// VerifyAPIKey reports whether key matches an enabled stored key.
//
// The stored hash is looked up by hash rather than by scanning every row, so
// this stays O(1) as keys accumulate. The comparison itself uses
// constant-time equality to avoid leaking the hash through timing.
func (s *Store) VerifyAPIKey(ctx context.Context, key string) (bool, error) {
	scope, err := s.APIKeyScope(ctx, key)
	return scope != "", err
}

// APIKeyScope 同时验证有效哈希并返回权限，避免验证与授权分别查询。
func (s *Store) APIKeyScope(ctx context.Context, key string) (string, error) {
	k, err := s.AuthenticateAPIKey(ctx, key)
	if k == nil {
		return "", err
	}
	return k.Scope, err
}

// AuthenticateAPIKey 每个请求读取最新权限，修改或撤销在下一次请求即生效。
func (s *Store) AuthenticateAPIKey(ctx context.Context, key string) (*APIKey, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil
	}
	hash := HashAPIKey(key)
	var stored, raw string
	var enabled int
	var k APIKey
	err := s.db.QueryRowContext(ctx,
		`SELECT id,name,key_prefix,enabled,created_at,last_used_at,scope,key_hash,permissions FROM api_keys WHERE key_hash=?`, hash).
		Scan(&k.ID, &k.Name, &k.KeyPrefix, &enabled, &k.CreatedAt, &k.LastUsed, &k.Scope, &stored, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("验证密钥失败：%w", err)
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(hash)) != 1 || enabled != 1 || (k.Scope != KeyScopeAdmin && k.Scope != KeyScopeInference) {
		return nil, nil
	}
	k.Permissions, err = decodeKeyPermissions(raw)
	if err != nil {
		return nil, err
	}
	k.Enabled = true
	k.KeyPrefix = safeStoredKeyPrefix(k.KeyPrefix, stored)
	_, _ = s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at=? WHERE id=?`, now(), k.ID)
	return &k, nil
}

func permissionJSON(p KeyPermissions) string { raw, _ := json.Marshal(p); return string(raw) }

// ListAPIKeys 返回仍有效的密钥，已撤销的哈希仅用于阻止重新启用。
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, key_prefix, enabled, created_at, last_used_at, scope, permissions, key_hash FROM api_keys WHERE enabled = 1 ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list api keys: %w", err)
	}
	defer rows.Close()

	var out []APIKey
	for rows.Next() {
		var k APIKey
		var enabled int
		var raw, hash string
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, &enabled, &k.CreatedAt, &k.LastUsed, &k.Scope, &raw, &hash); err != nil {
			return nil, fmt.Errorf("store: scan api key: %w", err)
		}
		k.Enabled = enabled != 0
		k.KeyPrefix = safeStoredKeyPrefix(k.KeyPrefix, hash)
		k.Permissions, err = decodeKeyPermissions(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate api keys: %w", err)
	}
	return out, nil
}

// CreateAPIKey adds a named key and returns its plaintext value. This is the
// only moment the value exists in readable form.
func (s *Store) CreateAPIKey(ctx context.Context, name string) (string, APIKey, error) {
	// 保留内部旧调用的行为；管理接口的新密钥默认使用调用权限。
	return s.CreateAPIKeyWithScope(ctx, name, KeyScopeAdmin)
}

// CreateAPIKeyWithScope 创建具有明确权限的密钥，只在创建时返回一次明文。
func (s *Store) CreateAPIKeyWithScope(ctx context.Context, name, scope string) (string, APIKey, error) {
	return s.CreateAPIKeyWithPermissions(ctx, name, scope, DefaultKeyPermissions())
}

func (s *Store) CreateAPIKeyWithPermissions(ctx context.Context, name, scope string, p KeyPermissions) (string, APIKey, error) {
	var err error
	p, err = NormalizeKeyPermissions(p)
	if err != nil {
		return "", APIKey{}, err
	}
	if scope != KeyScopeAdmin && scope != KeyScopeInference {
		return "", APIKey{}, fmt.Errorf("密钥权限只能是 admin 或 inference")
	}
	body, err := NewAPIKeyValue()
	if err != nil {
		return "", APIKey{}, err
	}
	key := FormatAPIKey(body)
	if err := s.ensureAPIKeyRowWithPermissions(ctx, key, strings.TrimSpace(name), scope, p); err != nil {
		return "", APIKey{}, err
	}
	hash := HashAPIKey(key)
	var out APIKey
	var enabled int
	var raw string
	if err := s.db.QueryRowContext(ctx,
		`SELECT id, name, key_prefix, enabled, created_at, last_used_at, scope, permissions FROM api_keys WHERE key_hash = ?`, hash).
		Scan(&out.ID, &out.Name, &out.KeyPrefix, &enabled, &out.CreatedAt, &out.LastUsed, &out.Scope, &raw); err != nil {
		return "", APIKey{}, fmt.Errorf("store: read back created api key: %w", err)
	}
	out.Enabled = enabled != 0
	out.Permissions, err = decodeKeyPermissions(raw)
	return key, out, err
}

// DeleteAPIKey 在事务中保护最后一个管理密钥，调用密钥不能充当后台入口。
func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始撤销密钥事务：%w", err)
	}
	defer tx.Rollback()
	var adminCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE enabled = 1 AND scope = 'admin'`).Scan(&adminCount); err != nil {
		return fmt.Errorf("store: count api keys: %w", err)
	}
	var targetEnabled int
	var targetScope string
	err = tx.QueryRowContext(ctx, `SELECT enabled, scope FROM api_keys WHERE id = ? AND enabled = 1`, id).Scan(&targetEnabled, &targetScope)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: api key %s: %w", id, ErrNotFound)
		}
		return fmt.Errorf("store: read api key %s: %w", id, err)
	}
	if targetEnabled == 1 && targetScope == KeyScopeAdmin && adminCount <= 1 {
		return fmt.Errorf("不能撤销最后一个有效管理密钥")
	}
	// 保留失效哈希作为撤销记录，防止重启时重新导入同一个初始密钥。
	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET enabled = 0 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete api key %s: %w", id, err)
	}
	return tx.Commit()
}

// GetSecret reads an app_secrets row.
func (s *Store) GetSecret(ctx context.Context, name string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_secrets WHERE name = ?`, name).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("store: secret %s: %w", name, ErrNotFound)
		}
		return "", fmt.Errorf("store: get secret %s: %w", name, err)
	}
	return s.decryptSecret(name, value)
}

// SetSecret writes an app_secrets row.
func (s *Store) SetSecret(ctx context.Context, name, value string) error {
	sealed, err := s.encryptSecret(name, value)
	if err != nil {
		return err
	}
	stamp := now()
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO app_secrets (name, value, created_at, updated_at) VALUES (?,?,?,?)
ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		name, sealed, stamp, stamp); err != nil {
		return fmt.Errorf("store: set secret %s: %w", name, err)
	}
	return nil
}

// DeleteSecret 用于明确删除隧道配置；不会删除账号凭证或 API 密钥。
func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM app_secrets WHERE name = ?`, name)
	return err
}
