package store

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"strings"
)

var credentialPrefix = []byte("relaypool:credential:v1:")

func (s *Store) initCredentialCipher(key []byte) error {
	if len(key) == 0 {
		return nil
	}
	if len(key) != 32 {
		return fmt.Errorf("凭证加密密钥必须是 32 字节")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("初始化凭证加密失败")
	}
	s.credentialCipher, err = cipher.NewGCM(block)
	return err
}

// 随机 nonce 防止相同凭证产生相同密文，账号 ID 作为附加认证数据防止串号。
func (s *Store) encryptCredential(accountID string, raw []byte) ([]byte, error) {
	if s.credentialCipher == nil {
		return raw, nil
	}
	nonce := make([]byte, s.credentialCipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("生成凭证加密随机数失败")
	}
	out := append([]byte{}, credentialPrefix...)
	out = append(out, nonce...)
	return s.credentialCipher.Seal(out, nonce, raw, []byte(accountID)), nil
}

func (s *Store) decryptCredential(accountID string, raw []byte) ([]byte, error) {
	if !bytes.HasPrefix(raw, credentialPrefix) {
		return raw, nil
	}
	if s.credentialCipher == nil {
		return nil, fmt.Errorf("数据库包含加密凭证，请提供原凭证加密密钥")
	}
	data := raw[len(credentialPrefix):]
	n := s.credentialCipher.NonceSize()
	if len(data) < n {
		return nil, fmt.Errorf("加密凭证损坏")
	}
	plain, err := s.credentialCipher.Open(nil, data[:n], data[n:], []byte(accountID))
	if err != nil {
		return nil, fmt.Errorf("凭证解密失败，请核对加密密钥或数据库完整性")
	}
	return plain, nil
}

// 启动时事务迁移旧凭证；任一密文无法认证时回滚并停止启动。
func (s *Store) migrateCredentialEncryption(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT account_id, payload FROM account_credentials")
	if err != nil {
		return err
	}
	type record struct {
		id      string
		payload []byte
	}
	var pending []record
	for rows.Next() {
		var row record
		if err = rows.Scan(&row.id, &row.payload); err != nil {
			rows.Close()
			return err
		}
		if bytes.HasPrefix(row.payload, credentialPrefix) {
			if _, err = s.decryptCredential(row.id, row.payload); err != nil {
				rows.Close()
				return err
			}
		} else if s.credentialCipher != nil {
			pending = append(pending, row)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, row := range pending {
		sealed, err := s.encryptCredential(row.id, row.payload)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE account_credentials SET payload=? WHERE account_id=?", sealed, row.id); err != nil {
			return err
		}
	}
	// 与账号凭证处于同一事务：任一秘密认证失败时不迁移任何记录。
	secretRows, err := tx.QueryContext(ctx, "SELECT name, value FROM app_secrets")
	if err != nil {
		return err
	}
	var secrets [][2]string
	for secretRows.Next() {
		var name, value string
		if err := secretRows.Scan(&name, &value); err != nil {
			secretRows.Close()
			return err
		}
		if _, err := s.decryptSecret(name, value); err != nil {
			secretRows.Close()
			return err
		}
		if s.credentialCipher != nil && !strings.HasPrefix(value, secretPrefix) {
			secrets = append(secrets, [2]string{name, value})
		}
	}
	err = secretRows.Err()
	secretRows.Close()
	if err != nil {
		return err
	}
	for _, secret := range secrets {
		sealed, err := s.encryptSecret(secret[0], secret[1])
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE app_secrets SET value=? WHERE name=?", sealed, secret[0]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CredentialsEncrypted() bool { return s != nil && s.credentialCipher != nil }
