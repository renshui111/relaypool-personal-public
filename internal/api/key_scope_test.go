package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/relaypool/relaypool/internal/store"
)

// 调用密钥走真实鉴权和请求处理，只把上游替换为本地测试适配器。
func TestInferenceKeyCannotManageButCanComplete(t *testing.T) {
	_, s := newTestServer(t, newStubProvider())
	r := callComplete(t, s, "POST", "/admin/keys", `{"name":"测试调用密钥"}`)
	var created struct{ Key, Scope, ID string }
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &created) != nil || created.Scope != store.KeyScopeInference {
		t.Fatal("新密钥没有默认使用调用权限")
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := authRequest(t, method, path, body)
		req.Header.Set("Authorization", "Bearer "+created.Key)
		r := httptest.NewRecorder()
		s.Handler().ServeHTTP(r, req)
		return r
	}
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/admin/accounts", ""}, {"GET", "/admin/armor/pack", ""},
		{"PATCH", "/admin/keys/any", `{"permissions":{"armor_enabled":true}}`},
		{"POST", "/admin/keys", `{"name":"权限提升","scope":"admin"}`},
		{"GET", "/admin/keys", ""}, {"DELETE", "/admin/keys/" + created.ID, ""},
		{"GET", "/admin/checkin/schedule", ""},
	} {
		if got := call(tc.method, tc.path, tc.body); got.Code != 403 {
			t.Errorf("调用密钥访问 %s %s 得到 %d，期望 403", tc.method, tc.path, got.Code)
		}
	}
	if call("GET", "/v1/models", "").Code != 200 {
		t.Fatal("调用密钥不能读取模型目录")
	}
	if call("POST", "/v1/chat/completions", `{"model":"glm-5.2","messages":[{"role":"user","content":"测试"}]}`).Code != 200 {
		t.Fatal("调用密钥不能完成模型请求")
	}
	if testKeyArmor(t, s) {
		t.Fatal("越权请求修改了开关")
	}
	if err := s.store.DeleteAPIKey(t.Context(), created.ID); err != nil {
		t.Fatal(err)
	}
	if call("GET", "/v1/models", "").Code != 401 {
		t.Fatal("撤销的调用密钥仍然有效")
	}
}

func TestAdminKeyCreationAndInvalidScope(t *testing.T) {
	s := newTestServerNoAdapter(t)
	if callComplete(t, s, "POST", "/admin/keys", `{"scope":"owner"}`).Code != 400 {
		t.Fatal("非法权限未拒绝")
	}
	r := callComplete(t, s, "POST", "/admin/keys", `{"name":"第二管理入口","scope":"admin"}`)
	var created struct{ Key, Scope string }
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &created) != nil || created.Scope != store.KeyScopeAdmin {
		t.Fatal("管理密钥创建失败")
	}
	req := authRequest(t, "GET", "/admin/keys", "")
	req.Header.Set("Authorization", "Bearer "+created.Key)
	got := httptest.NewRecorder()
	s.Handler().ServeHTTP(got, req)
	if got.Code != 200 {
		t.Fatal("明确创建的管理密钥无管理权限")
	}
}
