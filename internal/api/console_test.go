package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The console is served by the same binary as the API. These tests pin the
// contract that makes it usable and that is easy to break:
//
//   - the shell loads WITHOUT a credential, because the browser has no key
//     until the operator types one into the page
//   - its assets resolve (a missing vendor file leaves a blank page)
//   - unknown paths still answer JSON, so programmatic callers keep working
//     after the console took over the root path

func TestConsoleServedWithoutAuth(t *testing.T) {
	srv, _ := newTestServer(t, newStubProvider())

	resp, err := http.Get(srv.URL + "/console/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (the shell must load before any key exists)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type=%q, want HTML", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "<!DOCTYPE html>") {
		t.Errorf("body is not an HTML document: %.120s", body)
	}
	if !strings.Contains(string(body), "app.js") {
		t.Errorf("shell does not reference its script")
	}
}

func TestConsoleAssetsResolve(t *testing.T) {
	srv, _ := newTestServer(t, newStubProvider())

	// Every asset the shell loads. A missing one produces a page that renders
	// nothing, which is much harder to diagnose than a 404 here.
	assets := []struct {
		path        string
		contentType string
	}{
		{"/console/app.js", "javascript"},
		{"/console/api.js", "javascript"},
		{"/console/app.css", "css"},
		{"/console/vendor/petite-vue.js", "javascript"},
		{"/console/vendor/dayjs.min.js", "javascript"},
		{"/console/vendor/dayjs.zh-cn.js", "javascript"},
		{"/console/vendor/dayjs.relativeTime.js", "javascript"},
	}

	for _, asset := range assets {
		resp, err := http.Get(srv.URL + asset.path)
		if err != nil {
			t.Fatalf("GET %s: %v", asset.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status=%d, want 200", asset.path, resp.StatusCode)
			continue
		}
		if len(body) == 0 {
			t.Errorf("GET %s: empty body", asset.path)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, asset.contentType) {
			t.Errorf("GET %s: Content-Type=%q, want %q", asset.path, ct, asset.contentType)
		}
	}
}

func TestConsoleRedirectsBarePath(t *testing.T) {
	srv, _ := newTestServer(t, newStubProvider())

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(srv.URL + "/console")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPermanentRedirect && resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("status=%d, want a redirect to /console/", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasSuffix(loc, "/console/") {
		t.Errorf("Location=%q, want it to end with /console/", loc)
	}
}

// The console must not swallow the JSON 404: every programmatic caller relies
// on it, and the console is a page for humans.
func TestConsoleDoesNotBreakJSONRouting(t *testing.T) {
	srv, _ := newTestServer(t, newStubProvider())

	for _, path := range []string{"/nope", "/console/definitely-missing.js", "/admin/nonexistent"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status=%d, want 404", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("GET %s: Content-Type=%q, want JSON", path, ct)
		}
	}
}

// A non-GET on a console path has no meaning, and must not fall through to the
// file server (which would answer 405 with an HTML body).
func TestConsoleRejectsNonGET(t *testing.T) {
	srv, _ := newTestServer(t, newStubProvider())

	resp, err := http.Post(srv.URL+"/console/app.js", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404 for a POST to a static asset", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type=%q, want JSON", ct)
	}
}
