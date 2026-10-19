package store

import (
	"reflect"
	"strings"
	"sync"
	"testing"
)

// 短密钥及旧版前缀不能通过读取接口变成完整明文，鉴权仍须兼容。
func TestShortAPIKeyNeverAppearsInDisplay(t *testing.T) {
	s := openTestStore(t)
	const secret = "tiny"
	if _, _, err := s.EnsureBootstrapAPIKey(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	k, err := s.AuthenticateAPIKey(t.Context(), secret)
	if err != nil || k == nil {
		t.Fatal("短密钥兼容鉴权失败")
	}
	if strings.Contains(k.KeyPrefix, secret) {
		t.Fatal("鉴权信息泄露完整短密钥")
	}
	var prefix string
	if err := s.db.QueryRow(`SELECT key_prefix FROM api_keys WHERE id=?`, k.ID).Scan(&prefix); err != nil || strings.Contains(prefix, secret) {
		t.Fatal("新记录存储了完整短密钥前缀")
	}
	// 模拟旧版本保存了完整短前缀，读取时应遮蔽而不改动哈希和授权。
	if _, err := s.db.Exec(`UPDATE api_keys SET key_prefix=? WHERE id=?`, secret, k.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAPIKey(t.Context(), k.ID)
	if err != nil || strings.Contains(got.KeyPrefix, secret) {
		t.Fatal("单条读取泄露旧短前缀")
	}
	list, err := s.ListAPIKeys(t.Context())
	if err != nil || len(list) != 1 || strings.Contains(list[0].KeyPrefix, secret) {
		t.Fatal("列表泄露旧短前缀")
	}
	if ok, err := s.VerifyAPIKey(t.Context(), secret); err != nil || !ok {
		t.Fatal("遮蔽展示影响了原密钥鉴权")
	}
	if KeyPrefix("rp_0123456789abcdef") != "rp_01234" {
		t.Fatal("正常随机密钥的展示前缀发生变化")
	}
}

// 重现名称修改读取旧快照后，另一请求已经撤回授权的执行顺序。
func TestAPIKeyMetadataPatchKeepsLatestPolicy(t *testing.T) {
	s := openTestStore(t)
	p := DefaultKeyPermissions()
	p.ArmorAllowed, p.ArmorEnabled = true, true
	_, stale, err := s.CreateAPIKeyWithPermissions(t.Context(), "旧名称", KeyScopeInference, p)
	if err != nil {
		t.Fatal(err)
	}
	p = KeyPermissions{AccountIDs: []string{"only-one"}, Models: []string{"gpt-model"}}
	if _, err = s.UpdateAPIKey(t.Context(), stale.ID, stale.Name, stale.Scope, p); err != nil {
		t.Fatal(err)
	}
	name := "新名称"
	got, err := s.PatchAPIKey(t.Context(), stale.ID, &name, nil, nil)
	if err != nil || !reflect.DeepEqual(got.Permissions, p) || got.Name != name {
		t.Fatal("只改名称覆盖了最新授权，撤回的权限被恢复")
	}
	if err = s.DeleteAPIKey(t.Context(), stale.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PatchAPIKey(t.Context(), stale.ID, &name, nil, nil); err != ErrNotFound {
		t.Fatal("名称修改复活了撤销密钥")
	}
}

// 并发名称修改与授权撤回必须互不覆盖，覆盖真实 SQLite 事务路径。
func TestConcurrentAPIKeyMetadataCannotRestoreAuthorization(t *testing.T) {
	s := openTestStore(t)
	p := DefaultKeyPermissions()
	p.ArmorAllowed, p.ArmorEnabled = true, true
	_, k, err := s.CreateAPIKeyWithPermissions(t.Context(), "原名称", KeyScopeInference, p)
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 33)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			<-start
			name := "修改名称"
			_, err := s.PatchAPIKey(t.Context(), k.ID, &name, nil, nil)
			errs <- err
		})
	}
	wg.Go(func() {
		<-start
		p.ArmorAllowed, p.ArmorEnabled = false, false
		_, err := s.PatchAPIKey(t.Context(), k.ID, nil, nil, &p)
		errs <- err
	})
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetAPIKey(t.Context(), k.ID)
	if err != nil || got.Permissions.ArmorAllowed || got.Permissions.ArmorEnabled || got.Name != "修改名称" {
		t.Fatal("并发名称修改覆盖了授权撤回")
	}
}
