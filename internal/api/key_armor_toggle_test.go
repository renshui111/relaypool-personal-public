package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/armor"
	"github.com/relaypool/relaypool/internal/store"
)

// 获准和开启分别保存；仅改开关不会放宽账号、模型或后台权限。
func TestKeyArmorPermissionAndToggle(t *testing.T) {
	s := newTestServerNoAdapter(t)
	p := newStubProvider()
	s.registry.Register(p)
	addAccount(t, s, "allowed", p.family)
	s.armor.Register(armor.Preset{Name: "toggle-fixture", Family: "glm", Body: "请用中文回答"})
	policy := store.KeyPermissions{ArmorAllowed: true, AccountIDs: []string{"allowed"}, Models: []string{"workbuddy/glm-5.2"}}
	key, record, err := s.store.CreateAPIKeyWithPermissions(t.Context(), "获准但关闭", store.KeyScopeInference, policy)
	if err != nil {
		t.Fatal(err)
	}
	denied, _, err := s.store.CreateAPIKeyWithScope(t.Context(), "未授权", store.KeyScopeInference)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key, method, path, body string
		status                  int
	}{
		{denied, "PUT", "/v1/key/armor", `{"enabled":true}`, 403},
		{key, "PUT", "/v1/key/armor", `{"enabled":true,"armor_allowed":true}`, 400},
		{key, "PUT", "/v1/key/armor", `{"enabled":true,"id":"another"}`, 400},
		{key, "PUT", "/v1/key/armor", `{"enabled":true} {}`, 400},
		{key, "PUT", "/v1/key/armor", `{}`, 400},
		{key, "PUT", "/v1/key/armor", `{"enabled":null}`, 400},
		{key, "PUT", "/admin/keys/" + record.ID + "/armor", `{"enabled":true}`, 403},
		{"", "PUT", "/v1/key/armor", `{"enabled":true}`, 401},
	} {
		w := permissionCall(t, s, tc.key, tc.method, tc.path, tc.body)
		if w.Code != tc.status {
			t.Fatalf("%s 返回 %d，预期 %d", tc.path, w.Code, tc.status)
		}
	}
	checkChat := func(enabled bool) {
		t.Helper()
		for _, route := range []struct{ path, body string }{
			{"/v1/chat/completions", `{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}]}`},
			{"/v1/messages", `{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}],"max_tokens":32}`},
			{"/v1/responses", `{"model":"glm-5.2","input":"你好"}`},
		} {
			r := httptest.NewRequest("POST", route.path, strings.NewReader(route.body))
			r.Header.Set("Authorization", "Bearer "+key)
			r.Header.Set(armor.HeaderEnable, "true")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 200 || (w.Header().Get(armor.HeaderApplied) != "none") != enabled {
				t.Fatal("许可和实际开关未同时生效：" + route.path)
			}
		}
	}
	checkChat(false)
	for _, tc := range []struct {
		caller, path string
		enabled      bool
	}{
		{key, "/v1/key/armor", true},
		{"rp_test", "/admin/keys/" + record.ID + "/armor", false},
		{"rp_test", "/admin/keys/" + record.ID + "/armor", true},
	} {
		body, _ := json.Marshal(map[string]bool{"enabled": tc.enabled})
		w := permissionCall(t, s, tc.caller, http.MethodPut, tc.path, string(body))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		k, err := s.store.AuthenticateAPIKey(t.Context(), key)
		if err != nil || k == nil || !k.Permissions.ArmorAllowed || k.Permissions.ArmorEnabled != tc.enabled || k.Scope != store.KeyScopeInference || k.Name != record.Name || k.Permissions.AllAccounts || k.Permissions.AllModels || len(k.Permissions.AccountIDs) != 1 || len(k.Permissions.Models) != 1 {
			t.Fatal("切换开关改变了其他权限")
		}
		checkChat(tc.enabled)
	}
	policy.ArmorAllowed = false
	if _, err = s.store.UpdateAPIKey(t.Context(), record.ID, record.Name, record.Scope, policy); err != nil {
		t.Fatal(err)
	}
	if w := permissionCall(t, s, key, "PUT", "/v1/key/armor", `{"enabled":true}`); w.Code != 403 {
		t.Fatal("撤销许可后仍能开启")
	}
	checkChat(false)
	if err = s.store.DeleteAPIKey(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	if w := permissionCall(t, s, "rp_test", "PUT", "/admin/keys/"+record.ID+"/armor", `{"enabled":true}`); w.Code != 404 {
		t.Fatal("已撤销密钥被开关复活")
	}
}
