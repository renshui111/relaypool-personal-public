package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 重复或恶意客户端编号不能覆盖已有调用日志，返回的编号必须对应实际记录。
func TestSecurityClientIDCannotOverwriteRequestLog(t *testing.T) {
	s := newTestServerNoAdapter(t)
	p := newStubProvider()
	s.registry.Register(p)
	addAccount(t, s, "security-account", p.family)
	ids := map[string]bool{}
	for _, external := range []string{"known-old-request", "known-old-request", "injected\nlog", strings.Repeat("x", 8192)} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"测试"}]}`))
		r.Header.Set("Authorization", "Bearer rp_test")
		r.Header.Set("X-Request-ID", external)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		id := w.Header().Get("X-Request-ID")
		if w.Code != 200 || id == external || ids[id] || strings.ContainsAny(id, "\r\n") || len(id) > 128 {
			t.Fatal("外部编号污染请求编号或重复请求被合并")
		}
		ids[id] = true
	}
	w := permissionCall(t, s, "rp_test", "GET", "/admin/requests?limit=20", "")
	var logs struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(w.Body.Bytes(), &logs) != nil || len(logs.Data) != 4 {
		t.Fatal("调用日志被覆盖或未记录")
	}
	for _, row := range logs.Data {
		if !ids[row.ID] {
			t.Fatal("日志与返回编号不一致")
		}
	}
}

func TestSecurityRejectsTrailingJSONBeforeUpstream(t *testing.T) {
	s := newTestServerNoAdapter(t)
	for _, body := range []string{`{} {"injected":true}`, `{} trailing`, `{}` + strings.Repeat(" ", 32) + `[]`} {
		w := permissionCall(t, s, "rp_test", "POST", "/admin/accounts", body)
		if w.Code != 400 {
			t.Fatal("接受尾随 JSON 或非 JSON 内容")
		}
	}
}

func TestSecuritySensitiveResponseHeadersAndCrossSite(t *testing.T) {
	s := newTestServerNoAdapter(t)
	for _, path := range []string{"/readyz", "/admin/accounts", "/auth/callback?code=synthetic&state=wrong", "/console/"} {
		w := permissionCall(t, s, "rp_test", "GET", path, "")
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s 缺少敏感响应保护", path)
		}
		if path == "/console/" {
			csp := w.Header().Get("Content-Security-Policy")
			if !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
				t.Fatal("控制台未限制脚本和嵌入")
			}
		}
	}
	r := httptest.NewRequest("GET", "/admin/accounts", nil)
	r.Header.Set("Authorization", "Bearer rp_test")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("跨站浏览器可访问管理接口")
	}
}
