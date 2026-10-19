package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// 创建真正的旧结构，再由当前版本升级，校验旧配置不会被重写。
func TestReviewPoolMigrationUpgradesOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &Store{db: db, path: path}
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE schema_migrations (filename TEXT PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:4] {
		if err := legacy.applyMigration(t.Context(), migration, checksumOf(migration.sql)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO accounts(id,name,enabled,created_at,updated_at) VALUES('legacy','旧账号',1,?,?)`, now(), now()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	a, err := upgraded.GetAccount(t.Context(), "legacy")
	if err != nil || a.MaxInflight != 4 || a.QuotaScope != "shared" {
		t.Fatalf("升级改变了旧账号配置：%+v，%v", a, err)
	}
	deadline := time.Now().UTC().Add(time.Hour)
	state := AccountRuntime{BackoffLevel: 4, ModelDownUntil: map[string]time.Time{"m": deadline}, ModelBackoff: map[string]int{"m": 3}, ModelLastKind: map[string]string{"m": "rate_limit"}, ProvenModels: []string{"other"}}
	if err := upgraded.UpdatePoolRuntime(t.Context(), a.ID, state); err != nil {
		t.Fatal(err)
	}
	a, err = upgraded.GetAccount(t.Context(), a.ID)
	if err != nil || !a.Runtime.ModelDownUntil["m"].Equal(deadline) || a.Runtime.BackoffLevel != 4 || a.Runtime.ModelBackoff["m"] != 3 || len(a.Runtime.ProvenModels) != 1 {
		t.Fatal("升级后的完整健康状态不能保存")
	}
	if err := upgraded.DeleteAccount(t.Context(), a.ID); err != nil {
		t.Fatal(err)
	}
	if err := upgraded.UpdatePoolRuntime(t.Context(), a.ID, state); err == nil {
		t.Fatal("删除后的账号仍可写入健康状态")
	}
}
