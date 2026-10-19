package api

import "net/http"

// 敏感响应不得缓存；授权回调同样不向外部页面泄露来源地址。
func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		// 管理端不提供跨站调用，拒绝浏览器明确标记的跨站管理请求。
		if len(r.URL.Path) >= 7 && r.URL.Path[:7] == "/admin/" && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeError(w, http.StatusForbidden, "cross_site_request", "管理接口不接受跨站浏览器请求")
			return
		}
		next.ServeHTTP(w, r)
	})
}
