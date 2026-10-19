package api

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// The console is served by the gateway itself rather than from a CDN or a
// separate web server, for the same reason the admin API binds to loopback:
// the console drives credential uploads, so fetching it should not involve any
// third party. The vendored libraries ship inside the binary via embed, so the
// page is always in sync with the running gateway version.
//
// The directory is populated from web/dist at build time (see the `web` target
// in the Makefile). A checkout that never builds the console still compiles:
// the embed pattern tolerates an otherwise-empty directory via the sentinel.
//
//go:embed all:console
var consoleFS embed.FS

// mountConsole registers the console at /console/ on the server's mux.
func (s *Server) mountConsole() {
	sub, err := fs.Sub(consoleFS, "console")
	if err != nil {
		// Cannot happen unless the embed directive is broken at build time.
		return
	}

	// The console is NOT wrapped in auth: the page is a static shell that
	// prompts for the key and then calls the admin API with it. Requiring auth
	// to load the shell would block the very first visit, when the browser has
	// no key yet. Every call the shell makes remains fully authenticated.
	s.mux.Handle("GET /console", http.RedirectHandler("/console/", http.StatusPermanentRedirect))
	s.mux.Handle("GET /console/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/console/")
		if name == "" {
			name = "index.html"
		}

		// Serve from the embedded tree by name. Anything not present is a JSON
		// 404, not the file server's text/plain page: every other response from
		// this process uses the error envelope, and a caller parsing JSON
		// should not have to special-case a missing asset.
		data, readErr := fs.ReadFile(sub, name)
		if readErr != nil {
			s.handleNotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentTypeFor(name))
		// 网络访问时避免控制台被第三方页面嵌入，以及向外链发送来源地址。
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// The assets change only when the binary does, but the binary is
		// replaced in place during development, so revalidation is cheap and
		// avoids serving a stale console after an upgrade.
		w.Header().Set("Cache-Control", "no-store")
		// 模板求值需要 unsafe-eval；禁止内联脚本、外部脚本和页面嵌入。
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; connect-src 'self' http: https:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
}

// contentTypeFor maps a console asset name to its response content type.
//
// An explicit table beats sniffing: the embedded filesystem carries no
// extension metadata, and a wrong type for a module script makes the page fail
// to load with a MIME error that says nothing about the real cause.
func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".json"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	default:
		return "application/octet-stream"
	}
}
