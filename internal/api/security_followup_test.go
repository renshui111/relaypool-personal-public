package api

import (
	"context"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/store"
)

func TestKeyReadAndMetadataPatchSecurity(t *testing.T) {
	s := newTestServerNoAdapter(t)
	for _, path := range []string{"/admin/keys", "/v1/key/permissions"} {
		w := permissionCall(t, s, "rp_test", "GET", path, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "rp_test") {
			t.Fatal("读取接口泄露完整短密钥")
		}
	}
	p := store.KeyPermissions{Models: []string{"workbuddy/glm-5.2"}, AccountIDs: []string{}}
	_, k, err := s.store.CreateAPIKeyWithPermissions(t.Context(), "测试", store.KeyScopeInference, p)
	if err != nil {
		t.Fatal(err)
	}
	w := permissionCall(t, s, "rp_test", "PATCH", "/admin/keys/"+k.ID, `{"name":"改名称"}`)
	if w.Code != 200 {
		t.Fatal("部分更新失败")
	}
	got, err := s.store.GetAPIKey(t.Context(), k.ID)
	if err != nil || !reflect.DeepEqual(got.Permissions, p) || got.Scope != k.Scope || got.Name != "改名称" {
		t.Fatal("部分更新改变了未指定的授权字段")
	}
	if err = s.store.DeleteAPIKey(t.Context(), k.ID); err != nil {
		t.Fatal(err)
	}
	if permissionCall(t, s, "rp_test", "PATCH", "/admin/keys/"+k.ID, `{"name":"再改"}`).Code != 404 {
		t.Fatal("撤销密钥仍允许修改")
	}
}

// 用明确的账号集合检查权限交集，包括空白名单、裸型号和跨渠道别名。
func TestIndexedPermissionRoutingIsolation(t *testing.T) {
	s := newTestServerNoAdapter(t)
	for _, a := range []pool.Item{{ID: "w1", Provider: "workbuddy"}, {ID: "w2", Provider: "workbuddy"}, {ID: "c1", Provider: "claude"}} {
		s.pool.Upsert(a)
	}
	for _, tc := range []struct {
		name           string
		p              store.KeyPermissions
		model, channel string
		want           []string
	}{
		{"空账号白名单", store.KeyPermissions{AllModels: true}, "glm-5.2", "", nil},
		{"空模型白名单", store.KeyPermissions{AllAccounts: true}, "glm-5.2", "", nil},
		{"具体渠道", store.KeyPermissions{AllAccounts: true, Models: []string{"workbuddy/GLM_5.2"}}, "glm-5.2", "", []string{"w1", "w2"}},
		{"裸型号跨渠道", store.KeyPermissions{AllAccounts: true, Models: []string{"GLM_5.2"}}, "glm-5.2", "", []string{"c1", "w1", "w2"}},
		{"账号及渠道交集", store.KeyPermissions{AccountIDs: []string{"w2", "c1"}, Models: []string{"glm-5.2"}}, "glm-5.2", "workbuddy", []string{"w2"}},
		{"不能越过渠道", store.KeyPermissions{AllAccounts: true, Models: []string{"workbuddy/glm-5.2"}}, "glm-5.2", "claude", nil},
		{"不能自动选型", store.KeyPermissions{AllAccounts: true, Models: []string{"workbuddy/glm-5.2"}}, "auto", "", nil},
		{"不能前缀授权", store.KeyPermissions{AllAccounts: true, Models: []string{"glm-5.2"}}, "glm-5.20", "", nil},
		{"全模型但限定账号", store.KeyPermissions{AccountIDs: []string{"c1"}, AllModels: true}, "auto", "", []string{"c1"}},
		{"全部授权", store.DefaultKeyPermissions(), "auto", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			r = r.WithContext(context.WithValue(r.Context(), keyPermissionsContextKey{}, store.APIKey{Permissions: tc.p}))
			w := httptest.NewRecorder()
			ids, ok := s.permittedRouteAccounts(w, r, tc.model, tc.channel)
			wantOK := tc.want != nil || tc.p.AllAccounts && tc.p.AllModels
			if ok != wantOK || !reflect.DeepEqual(ids, tc.want) || !ok && w.Code != 403 {
				t.Fatal("权限交集或拒绝语义发生变化")
			}
		})
	}
}

func TestNetworkAllModesRequireHeaderAuthentication(t *testing.T) {
	s := newTestServerNoAdapter(t)
	for _, key := range []string{"rp_test", ""} {
		if permissionCall(t, s, key, "GET", "/admin/network", "").Code != 404 {
			t.Fatal("自用版仍暴露网络配置")
		}
	}
}
