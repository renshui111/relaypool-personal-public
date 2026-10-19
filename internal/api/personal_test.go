package api

import (
	"net/http/httptest"
	"testing"
)

func TestPersonalRemovedSurfaces(t *testing.T) {
	s := newTestServerNoAdapter(t)
	for _, path := range []string{"/user/", "/user/api/register", "/user/api/chat", "/admin/users", "/admin/commerce", "/payments/epay/notify", "/admin/network", "/admin/network/tunnel", "/console/user/", "/console/users.js", "/console/commerce.js", "/console/network.js"} {
		for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Authorization", "Bearer rp_test")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)
			if w.Code != 404 {
				t.Errorf("removed surface %s %s returned %d", method, path, w.Code)
			}
		}
	}
}
