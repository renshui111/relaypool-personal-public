package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestKeyScopeProtectsLastAdminAndDoesNotElevateBootstrap(t *testing.T) {
	s := openTestStore(t)
	_, admin, err := s.CreateAPIKey(t.Context(), "管理入口")
	if err != nil {
		t.Fatal(err)
	}
	clientValue, client, err := s.CreateAPIKeyWithScope(t.Context(), "调用入口", KeyScopeInference)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnsureBootstrapAPIKey(t.Context(), clientValue); err != nil {
		t.Fatal(err)
	}
	if scope, err := s.APIKeyScope(t.Context(), clientValue); err != nil || scope != KeyScopeInference {
		t.Fatal("重复导入提升了调用密钥权限")
	}
	if err := s.DeleteAPIKey(t.Context(), admin.ID); err == nil {
		t.Fatal("还有调用密钥时删除了最后一个管理密钥")
	}
	if err := s.DeleteAPIKey(t.Context(), client.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnsureBootstrapAPIKey(t.Context(), clientValue); err != nil {
		t.Fatal(err)
	}
	if scope, err := s.APIKeyScope(t.Context(), clientValue); err != nil || scope != "" {
		t.Fatal("重新导入恢复了撤销的调用密钥")
	}
}

// 建立前六步的真实旧数据库，检查追加权限迁移不改变密钥和撤销状态。
func TestKeyScopeMigrationPreservesLegacyCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &Store{db: db, path: path}
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE schema_migrations (filename TEXT PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:6] {
		if err := legacy.applyMigration(t.Context(), m, checksumOf(m.sql)); err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range []struct {
		id, key string
		enabled int
	}{{"legacy-admin", "legacy-test-active", 1}, {"legacy-revoked", "legacy-test-revoked", 0}} {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO api_keys(id,name,key_hash,key_prefix,enabled,created_at) VALUES(?,?,?,?,?,?)`, fixture.id, "旧密钥", HashAPIKey(fixture.key), KeyPrefix(fixture.key), fixture.enabled, now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InitializeAPIKeyPermissions(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if scope, err := s.APIKeyScope(t.Context(), "legacy-test-active"); err != nil || scope != KeyScopeAdmin {
		t.Fatal("旧管理密钥升级后失效")
	}
	if scope, err := s.APIKeyScope(t.Context(), "legacy-test-revoked"); err != nil || scope != "" {
		t.Fatal("升级恢复了撤销密钥")
	}
	var hash string
	if err := s.db.QueryRowContext(t.Context(), `SELECT key_hash FROM api_keys WHERE id='legacy-admin'`).Scan(&hash); err != nil || hash != HashAPIKey("legacy-test-active") {
		t.Fatal("升级改变了旧密钥哈希")
	}
	for _, m := range migrations[:6] {
		if ok, err := s.confirmConcurrentApply(t.Context(), m.filename, checksumOf(m.sql)); err != nil || !ok {
			t.Fatal("旧迁移校验和被修改")
		}
	}
}
