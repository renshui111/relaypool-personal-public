package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/armor"
	"github.com/relaypool/relaypool/internal/store"
)

func setTestKeyArmor(t *testing.T, s *Server, enabled bool) error {
	t.Helper()
	k, err := s.store.AuthenticateAPIKey(t.Context(), "rp_test")
	if err != nil {
		return err
	}
	p := k.Permissions
	p.ArmorAllowed = enabled
	p.ArmorEnabled = enabled
	_, err = s.store.UpdateAPIKey(t.Context(), k.ID, k.Name, k.Scope, p)
	return err
}
func testKeyArmor(t *testing.T, s *Server) bool {
	t.Helper()
	k, err := s.store.AuthenticateAPIKey(t.Context(), "rp_test")
	if err != nil {
		t.Fatal(err)
	}
	return k.Permissions.ArmorEnabled
}
func testKeyRequest(t *testing.T, s *Server, r *http.Request) *http.Request {
	t.Helper()
	k, err := s.store.AuthenticateAPIKey(t.Context(), "rp_test")
	if err != nil {
		t.Fatal(err)
	}
	return r.WithContext(context.WithValue(r.Context(), keyPermissionsContextKey{}, *k))
}

func permissionCall(t *testing.T, s *Server, key, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestKeyPermissionModelAccountAndProtocolIsolation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, route := range []struct{ path, body string }{
			{"/v1/chat/completions", `{"model":"MODEL","messages":[{"role":"user","content":"你好"}],"stream":STREAM}`},
			{"/v1/messages", `{"model":"MODEL","messages":[{"role":"user","content":"你好"}],"max_tokens":32,"stream":STREAM}`},
			{"/v1/responses", `{"model":"MODEL","input":"你好","stream":STREAM}`},
		} {
			t.Run(route.path+boolString(stream), func(t *testing.T) {
				s := newTestServerNoAdapter(t)
				p := newStubProvider()
				s.registry.Register(p)
				for _, id := range []string{"allowed", "forbidden"} {
					addAccount(t, s, id, p.family)
					s.pool.MergeModels(id, []string{"glm-5.2", "glm-5.3"})
				}
				policy := store.KeyPermissions{AccountIDs: []string{"allowed"}, Models: []string{"workbuddy/glm-5.2"}}
				key, _, err := s.store.CreateAPIKeyWithPermissions(t.Context(), "受限", store.KeyScopeInference, policy)
				if err != nil {
					t.Fatal(err)
				}
				for _, tc := range []struct {
					model  string
					status int
				}{{"glm-5.2", 200}, {"workbuddy/GLM_5.2", 200}, {"glm-5.3", 403}, {"auto", 403}, {"openai/glm-5.2", 403}, {"glm-5.20", 403}} {
					body := strings.ReplaceAll(strings.ReplaceAll(route.body, "MODEL", tc.model), "STREAM", boolString(stream))
					w := permissionCall(t, s, key, "POST", route.path, body)
					if w.Code != tc.status {
						t.Fatalf("%s 返回 %d：%s", tc.model, w.Code, w.Body.String())
					}
					if w.Code == 200 && w.Header().Get("X-RelayPool-Account") != "allowed" {
						t.Fatal("请求到了未授权账号")
					}
				}
				catalog := permissionCall(t, s, key, "GET", "/v1/models", "")
				if strings.Contains(catalog.Body.String(), "glm-5.3") || strings.Contains(catalog.Body.String(), "/cn") {
					t.Fatal("目录暴露了未授权型号或自动别名")
				}
				before := p.calls
				r := httptest.NewRequest("POST", route.path, strings.NewReader(strings.ReplaceAll(strings.ReplaceAll(route.body, "MODEL", "glm-5.2"), "STREAM", boolString(stream))))
				r.Header.Set("Authorization", "Bearer "+key)
				r.Header.Set("X-RelayPool-Account", "forbidden")
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				if w.Code != 403 || p.calls != before {
					t.Fatal("指定未授权账号绕过了限制")
				}
			})
		}
	}
}

func TestKeyPermissionsArmorIndependenceAndLiveEdit(t *testing.T) {
	s := newTestServerNoAdapter(t)
	p := newStubProvider()
	s.registry.Register(p)
	addAccount(t, s, "allowed", p.family)
	s.armor.Register(armor.Preset{Name: "permission-fixture", Family: "glm", Body: "请用中文回答"})
	policy := store.DefaultKeyPermissions()
	policy.ArmorAllowed = true
	policy.ArmorEnabled = true
	on, onRecord, err := s.store.CreateAPIKeyWithPermissions(t.Context(), "开", store.KeyScopeInference, policy)
	if err != nil {
		t.Fatal(err)
	}
	off, _, err := s.store.CreateAPIKeyWithScope(t.Context(), "关", store.KeyScopeInference)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}]}`
	for _, tc := range []struct {
		key, header string
		applied     bool
	}{{on, "", true}, {off, "true", false}, {off, "permission-fixture", false}, {on, "0", false}} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+tc.key)
		if tc.header == "permission-fixture" {
			r.Header.Set(armor.HeaderPreset, tc.header)
		} else {
			r.Header.Set(armor.HeaderEnable, tc.header)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 200 || (w.Header().Get(armor.HeaderApplied) != "none") != tc.applied {
			t.Fatal("密钥破甲权限被其他密钥或请求头影响")
		}
	}
	// 管理页修改调用权限，既有明文密钥不变；下次鉴权立即读取新值。
	policy.ArmorEnabled = false
	policy.AllModels = false
	policy.Models = []string{}
	raw, _ := json.Marshal(map[string]any{"permissions": policy})
	w := permissionCall(t, s, "rp_test", "PATCH", "/admin/keys/"+onRecord.ID, string(raw))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = permissionCall(t, s, on, "POST", "/v1/chat/completions", body); w.Code != 403 {
		t.Fatal("空模型白名单或即时更新失效")
	}
	policy.AllModels = true
	policy.AllAccounts = false
	policy.AccountIDs = []string{}
	if _, err = s.store.UpdateAPIKey(t.Context(), onRecord.ID, "开", store.KeyScopeInference, policy); err != nil {
		t.Fatal(err)
	}
	if w = permissionCall(t, s, on, "GET", "/v1/models", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Fatal("空账号白名单扩大为全部")
	}
	if w = permissionCall(t, s, on, "POST", "/v1/chat/completions", body); w.Code != 403 {
		t.Fatal("空账号白名单未阻止请求")
	}
	if w = permissionCall(t, s, on, "PATCH", "/admin/keys/"+onRecord.ID, `{"scope":"admin"}`); w.Code != 403 {
		t.Fatal("调用密钥能提升自身权限")
	}
	if w = permissionCall(t, s, on, "GET", "/v1/key/permissions", ""); w.Code != 200 || strings.Contains(w.Body.String(), on) {
		t.Fatal("当前权限查询失败或泄漏完整密钥")
	}
	for _, path := range []string{"/admin/armor/switch"} {
		if w = permissionCall(t, s, "rp_test", "PUT", path, `{"enabled":true}`); w.Code != 404 {
			t.Fatal("旧全局开关仍可写")
		}
	}
}

func TestRestrictedAdminCannotPinOrProbeUnauthorizedAccounts(t *testing.T) {
	s := newTestServerNoAdapter(t)
	p := newStubProvider()
	s.registry.Register(p)
	for _, id := range []string{"allowed", "forbidden"} {
		addAccount(t, s, id, p.family)
		s.pool.MergeModels(id, []string{"glm-5.2"})
	}
	policy := store.DefaultKeyPermissions()
	policy.AllAccounts = false
	policy.AccountIDs = []string{"allowed"}
	key, _, err := s.store.CreateAPIKeyWithPermissions(t.Context(), "受限管理员", store.KeyScopeAdmin, policy)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}]}`
	w := permissionCall(t, s, key, "POST", "/admin/accounts/forbidden/chat/completions", body)
	if w.Code != 403 || p.calls != 0 {
		t.Fatal("管理固定账号调用绕过限制")
	}
	w = permissionCall(t, s, key, "POST", "/admin/accounts/test", `{"ids":["forbidden"],"model":"glm-5.2"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "skipped") || p.calls != 0 {
		t.Fatal("批量测试绕过权限")
	}
	w = permissionCall(t, s, key, "GET", "/admin/armor/resolve?account_id=forbidden&model=glm-5.2", "")
	if w.Code != 403 {
		t.Fatal("固定账号预览未检查权限")
	}
}

// 验证白名单在故障换号及跨密钥的同名会话里始终有效。
func TestKeyPermissionFailoverAndSharedSession(t *testing.T) {
	s := newTestServerNoAdapter(t)
	p := newStubProvider()
	p.failuresBeforeSuccess = 1
	p.failKind = "network"
	s.registry.Register(p)
	for _, id := range []string{"one", "two"} {
		addAccount(t, s, id, p.family)
	}
	policy := store.DefaultKeyPermissions()
	policy.AllAccounts = false
	policy.AccountIDs = []string{"one"}
	key, _, err := s.store.CreateAPIKeyWithPermissions(t.Context(), "受限", store.KeyScopeInference, policy)
	if err != nil {
		t.Fatal(err)
	}
	call := func(key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}]}`))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("X-RelayPool-Session", "shared")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	w := call(key)
	if w.Code == 200 || p.calls != 1 {
		t.Fatal("失败后使用了未授权的备用账号")
	}
	w = call("rp_test")
	if w.Code != 200 || w.Header().Get("X-RelayPool-Account") != "two" {
		t.Fatal("管理密钥未使用可用备用账号")
	}
	policy.AccountIDs = []string{"two"}
	k, _ := s.store.AuthenticateAPIKey(t.Context(), key)
	s.store.UpdateAPIKey(t.Context(), k.ID, k.Name, k.Scope, policy)
	w = call(key)
	if w.Code != 200 || w.Header().Get("X-RelayPool-Account") != "two" {
		t.Fatal("会话绑定绕过更新后的账号白名单")
	}
	io.Copy(io.Discard, w.Result().Body)
}
