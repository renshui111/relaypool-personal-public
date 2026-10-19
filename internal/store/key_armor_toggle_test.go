package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// 升级保留旧开关；新许可字段明确存在时，以明确许可为准。
func TestKeyArmorLegacyAndPersistence(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		raw, _ := json.Marshal(map[string]any{"armor_enabled": enabled, "all_accounts": true, "all_models": true})
		p, err := decodeKeyPermissions(string(raw))
		if err != nil || p.ArmorAllowed != enabled || p.ArmorEnabled != enabled {
			t.Fatal("旧版破甲状态未保留")
		}
	}
	if _, err := decodeKeyPermissions(`{"armor_allowed":false,"armor_enabled":true}`); err == nil {
		t.Fatal("明确取消许可后仍可开启")
	}
	if _, err := decodeKeyPermissions(`{"armor_allowed":true,"unknown":true}`); err == nil {
		t.Fatal("未知权限字段被接受")
	}
	path := filepath.Join(t.TempDir(), "keys.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultKeyPermissions()
	p.ArmorAllowed = true
	key, k, err := s.CreateAPIKeyWithPermissions(t.Context(), "切换测试", KeyScopeInference, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetAPIKeyArmor(t.Context(), k.ID, true); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	k2, err := s.AuthenticateAPIKey(t.Context(), key)
	if err != nil || k2 == nil || !k2.Permissions.ArmorAllowed || !k2.Permissions.ArmorEnabled {
		t.Fatal("重启后开关丢失")
	}
	if _, err = s.SetAPIKeyArmor(t.Context(), k.ID, false); err != nil {
		t.Fatal(err)
	}
	k2, err = s.AuthenticateAPIKey(t.Context(), key)
	if err != nil || !k2.Permissions.ArmorAllowed || k2.Permissions.ArmorEnabled {
		t.Fatal("关闭时删除了许可")
	}
}
