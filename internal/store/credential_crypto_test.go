package store

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestCompleteCredentialEncryptionMigrationAndAuthentication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	ctx := t.Context()
	plain, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	account, err := plain.CreateAccount(ctx, Account{Name: "测试", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte(`{"access_token":"synthetic-secret"}`)
	if err := plain.UpsertCredential(ctx, Credential{AccountID: account.ID, Payload: secret}); err != nil {
		t.Fatal(err)
	}
	plain.Close()
	key := bytes.Repeat([]byte{1}, 32)
	sealed, err := OpenWithCredentialKey(ctx, path, key)
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := sealed.db.QueryRowContext(ctx, "SELECT payload FROM account_credentials WHERE account_id=?", account.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("synthetic-secret")) || !bytes.HasPrefix(stored, credentialPrefix) {
		t.Fatal("凭证未加密")
	}
	loaded, err := sealed.GetCredential(ctx, account.ID)
	if err != nil || !bytes.Equal(loaded.Payload, secret) {
		t.Fatal("解密未保持原始凭证")
	}
	if _, err := sealed.decryptCredential("其他账号", stored); err == nil {
		t.Fatal("密文可被复制到其他账号")
	}
	stored[len(stored)-1] ^= 1
	if _, err := sealed.decryptCredential(account.ID, stored); err == nil {
		t.Fatal("未检测密文篡改")
	}
	sealed.Close()
	if bad, err := OpenWithCredentialKey(ctx, path, bytes.Repeat([]byte{2}, 32)); err == nil {
		bad.Close()
		t.Fatal("错误密钥启动成功")
	}
	if bad, err := Open(ctx, path); err == nil {
		bad.Close()
		t.Fatal("遗漏密钥启动成功")
	}
	reopened, err := OpenWithCredentialKey(ctx, path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.GetCredential(ctx, account.ID); err != nil {
		t.Fatal("拒绝错误密钥时破坏原数据库")
	}
}

func TestCompleteQuotaMetadataSurvivesRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAccount(ctx, Account{QuotaGroup: "项目", QuotaScope: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	a.QuotaExpiresAt = a.CreatedAt.AddDate(0, 0, 7)
	if err := s.UpdateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePoolRuntime(ctx, a.ID, AccountRuntime{GroupDownUntil: a.QuotaExpiresAt}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetAccount(ctx, a.ID)
	if err != nil || got.QuotaGroup != "项目" || !got.QuotaExpiresAt.Equal(a.QuotaExpiresAt) || !got.Runtime.GroupDownUntil.Equal(a.QuotaExpiresAt) {
		t.Fatal("额度配置或组冷却未持久化")
	}
}
