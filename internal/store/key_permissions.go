package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// KeyPermissions 区分不限范围与空白名单；空白名单禁止调用，不能回退为全部允许。
type KeyPermissions struct {
	ArmorAllowed bool     `json:"armor_allowed"`
	ArmorEnabled bool     `json:"armor_enabled"`
	AllAccounts  bool     `json:"all_accounts"`
	AccountIDs   []string `json:"account_ids"`
	AllModels    bool     `json:"all_models"`
	Models       []string `json:"models"`
}

// UnmarshalJSON 兼容旧权限：原先启用的密钥保留许可和开关，显式取消许可不能被旧字段覆盖。
func (p *KeyPermissions) UnmarshalJSON(raw []byte) error {
	type wire KeyPermissions
	var value wire
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if _, present := fields["armor_allowed"]; !present {
		value.ArmorAllowed = value.ArmorEnabled
	}
	*p = KeyPermissions(value)
	return nil
}

func DefaultKeyPermissions() KeyPermissions {
	return KeyPermissions{AllAccounts: true, AllModels: true, AccountIDs: []string{}, Models: []string{}}
}

func (p KeyPermissions) AllowsAccount(id string) bool {
	if p.AllAccounts {
		return true
	}
	for _, allowed := range p.AccountIDs {
		if allowed == id {
			return true
		}
	}
	return false
}

// NormalizeKeyPermissions 对白名单去重并限制大小，避免授权数据无限膨胀。
func NormalizeKeyPermissions(p KeyPermissions) (KeyPermissions, error) {
	if p.ArmorEnabled && !p.ArmorAllowed {
		return p, fmt.Errorf("当前密钥未获准使用破甲，不能开启")
	}
	clean := func(values []string) ([]string, error) {
		if len(values) > 1024 {
			return nil, fmt.Errorf("白名单最多包含 1024 项")
		}
		out := []string{}
		seen := map[string]bool{}
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
				return nil, fmt.Errorf("白名单项不能为空、超过 256 字节或包含换行")
			}
			if !seen[value] {
				out = append(out, value)
				seen[value] = true
			}
		}
		return out, nil
	}
	var err error
	if p.AccountIDs, err = clean(p.AccountIDs); err != nil {
		return p, err
	}
	if p.Models, err = clean(p.Models); err != nil {
		return p, err
	}
	if p.AllAccounts {
		p.AccountIDs = []string{}
	}
	if p.AllModels {
		p.Models = []string{}
	}
	return p, nil
}

func decodeKeyPermissions(raw string) (KeyPermissions, error) {
	var p KeyPermissions
	// 未迁移或损坏的权限不能当作不限范围。
	if raw == "" {
		return p, fmt.Errorf("密钥权限尚未初始化")
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, fmt.Errorf("密钥权限数据不可用")
	}
	return NormalizeKeyPermissions(p)
}

// InitializeAPIKeyPermissions 仅初始化旧版记录；后续修改配置或旧全局设置不再影响密钥。
func (s *Store) InitializeAPIKeyPermissions(ctx context.Context, legacyArmor bool) error {
	p := DefaultKeyPermissions()
	p.ArmorAllowed = legacyArmor
	p.ArmorEnabled = legacyArmor
	raw, _ := json.Marshal(p)
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET permissions = ? WHERE permissions = ''`, string(raw))
	return err
}

var ErrArmorNotAllowed = errors.New("当前密钥未获准使用破甲")

// SetAPIKeyArmor 只改当前开关，事务内读取最新许可，不覆盖账号、型号或其他权限。
func (s *Store) SetAPIKeyArmor(ctx context.Context, id string, enabled bool) (APIKey, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return APIKey{}, err
	}
	defer tx.Rollback()
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT permissions FROM api_keys WHERE id=? AND enabled=1`, id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return APIKey{}, ErrNotFound
		}
		return APIKey{}, err
	}
	p, err := decodeKeyPermissions(raw)
	if err != nil {
		return APIKey{}, err
	}
	if enabled && !p.ArmorAllowed {
		return APIKey{}, ErrArmorNotAllowed
	}
	p.ArmorEnabled = enabled
	if _, err = tx.ExecContext(ctx, `UPDATE api_keys SET permissions=? WHERE id=? AND enabled=1`, permissionJSON(p), id); err != nil {
		return APIKey{}, err
	}
	if err = tx.Commit(); err != nil {
		return APIKey{}, err
	}
	return s.GetAPIKey(ctx, id)
}

// UpdateAPIKey 显式替换全部配置；部分修改必须使用 PatchAPIKey。
func (s *Store) UpdateAPIKey(ctx context.Context, id, name, scope string, p KeyPermissions) (APIKey, error) {
	return s.PatchAPIKey(ctx, id, &name, &scope, &p)
}

// PatchAPIKey 在事务内仅修改传入字段，避免旧快照恢复已撤回的授权。
func (s *Store) PatchAPIKey(ctx context.Context, id string, name, scope *string, permissions *KeyPermissions) (APIKey, error) {
	if name == nil && scope == nil && permissions == nil {
		return APIKey{}, fmt.Errorf("请提供名称、权限或调用范围")
	}
	if scope != nil && *scope != KeyScopeAdmin && *scope != KeyScopeInference {
		return APIKey{}, fmt.Errorf("密钥权限只能是 admin 或 inference")
	}
	if name != nil && len(strings.TrimSpace(*name)) > 200 {
		return APIKey{}, fmt.Errorf("名称最多 200 字节")
	}
	var p KeyPermissions
	var err error
	if permissions != nil {
		p, err = NormalizeKeyPermissions(*permissions)
		if err != nil {
			return APIKey{}, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return APIKey{}, err
	}
	defer tx.Rollback()
	var oldScope string
	if err = tx.QueryRowContext(ctx, `SELECT scope FROM api_keys WHERE id=? AND enabled=1`, id).Scan(&oldScope); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return APIKey{}, ErrNotFound
		}
		return APIKey{}, err
	}
	if scope != nil && oldScope == KeyScopeAdmin && *scope != KeyScopeAdmin {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE scope='admin' AND enabled=1`).Scan(&count); err != nil {
			return APIKey{}, err
		}
		if count <= 1 {
			return APIKey{}, fmt.Errorf("不能降低最后一个有效管理密钥的管理权限")
		}
	}
	fields := []string{}
	args := []any{}
	if name != nil {
		fields, args = append(fields, "name=?"), append(args, strings.TrimSpace(*name))
	}
	if scope != nil {
		fields, args = append(fields, "scope=?"), append(args, *scope)
	}
	if permissions != nil {
		fields, args = append(fields, "permissions=?"), append(args, permissionJSON(p))
	}
	args = append(args, id)
	// SQL 字段只来自固定内部常量，用户值始终使用参数绑定。
	if _, err = tx.ExecContext(ctx, `UPDATE api_keys SET `+strings.Join(fields, ",")+` WHERE id=? AND enabled=1`, args...); err != nil {
		return APIKey{}, err
	}
	if err = tx.Commit(); err != nil {
		return APIKey{}, err
	}
	return s.GetAPIKey(ctx, id)
}

func (s *Store) GetAPIKey(ctx context.Context, id string) (APIKey, error) {
	var k APIKey
	var enabled int
	var raw, hash string
	err := s.db.QueryRowContext(ctx, `SELECT id,name,key_prefix,enabled,created_at,last_used_at,scope,permissions,key_hash FROM api_keys WHERE id=? AND enabled=1`, id).Scan(&k.ID, &k.Name, &k.KeyPrefix, &enabled, &k.CreatedAt, &k.LastUsed, &k.Scope, &raw, &hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return k, ErrNotFound
		}
		return k, err
	}
	k.Enabled = enabled != 0
	k.KeyPrefix = safeStoredKeyPrefix(k.KeyPrefix, hash)
	k.Permissions, err = decodeKeyPermissions(raw)
	return k, err
}
