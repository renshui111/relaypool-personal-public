package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestKeyPermissionMigrationInheritanceAndPersistence(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{true: "开启", false: "关闭"}[enabled], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			legacy := &Store{db: db, path: path}
			db.Exec(`CREATE TABLE schema_migrations(filename TEXT PRIMARY KEY,checksum TEXT NOT NULL,applied_at TEXT NOT NULL)`)
			for _, m := range migrations[:7] {
				if err := legacy.applyMigration(t.Context(), m, checksumOf(m.sql)); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range []string{"active", "revoked"} {
				value := 1
				if id == "revoked" {
					value = 0
				}
				if _, err := db.Exec(`INSERT INTO api_keys(id,name,key_hash,enabled,created_at,scope) VALUES(?,?,?,?,?,?)`, id, id, HashAPIKey("fixture-"+id), value, now(), KeyScopeInference); err != nil {
					t.Fatal(err)
				}
			}
			value := "false"
			if enabled {
				value = "true"
			}
			legacy.SetSetting(t.Context(), SettingArmorEnabled, value)
			db.Close()
			s, err := Open(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.InitializeAPIKeyPermissions(t.Context(), enabled); err != nil {
				t.Fatal(err)
			}
			k, err := s.AuthenticateAPIKey(t.Context(), "fixture-active")
			if err != nil || k == nil || k.Permissions.ArmorEnabled != enabled || !k.Permissions.AllAccounts || !k.Permissions.AllModels {
				t.Fatal("旧权限继承失败")
			}
			if k, err := s.AuthenticateAPIKey(t.Context(), "fixture-revoked"); err != nil || k != nil {
				t.Fatal("迁移复活撤销密钥")
			}
			p := KeyPermissions{ArmorAllowed: true, ArmorEnabled: !enabled, AccountIDs: []string{"one"}, Models: []string{"gpt-model"}}
			if _, err := s.UpdateAPIKey(t.Context(), "active", "已编辑", KeyScopeInference, p); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s, err = Open(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.InitializeAPIKeyPermissions(t.Context(), enabled); err != nil {
				t.Fatal(err)
			}
			k, err = s.AuthenticateAPIKey(t.Context(), "fixture-active")
			if err != nil || k.Permissions.ArmorEnabled == enabled || k.Permissions.AllAccounts || k.Permissions.AllModels || !k.Permissions.AllowsAccount("one") {
				t.Fatal("重启或旧全局值覆盖了密钥独立权限")
			}
			_, newKey, err := s.CreateAPIKeyWithScope(t.Context(), "新密钥", KeyScopeInference)
			if err != nil || newKey.Permissions.ArmorEnabled {
				t.Fatal("新密钥误继承旧开关")
			}
		})
	}
}

func TestKeyPermissionLastAdminAndCorruptPolicy(t *testing.T) {
	s := openTestStore(t)
	key, k, err := s.CreateAPIKey(t.Context(), "唯一管理员")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateAPIKey(t.Context(), k.ID, k.Name, KeyScopeInference, k.Permissions); err == nil {
		t.Fatal("唯一管理员降权成功")
	}
	if _, err = s.db.Exec(`UPDATE api_keys SET permissions='broken' WHERE id=?`, k.ID); err != nil {
		t.Fatal(err)
	}
	if record, err := s.AuthenticateAPIKey(t.Context(), key); record != nil || err == nil {
		t.Fatal("损坏授权扩大为全部允许")
	}
}
