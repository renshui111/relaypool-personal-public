package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
)

// 刷新结果仅能替换本次读取的旧凭证；事务内比较明文，写回仍使用随机密文。
func (s *Store) ReplaceCredentialIfUnchanged(ctx context.Context, previous []byte, c Credential) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var stored []byte
	if err := tx.QueryRowContext(ctx, "SELECT payload FROM account_credentials WHERE account_id=?", c.AccountID).Scan(&stored); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	plain, err := s.decryptCredential(c.AccountID, stored)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(plain, previous) {
		return false, nil
	}
	payload, err := s.encryptCredential(c.AccountID, c.Payload)
	if err != nil {
		return false, err
	}
	stamp := now()
	if _, err := tx.ExecContext(ctx, "UPDATE account_credentials SET payload=?,format=?,updated_at=? WHERE account_id=?", payload, c.Format, stamp, c.AccountID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE credential_metadata SET expires_at=?,updated_at=? WHERE account_id=?", formatTime(c.ExpiresAt), stamp, c.AccountID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
