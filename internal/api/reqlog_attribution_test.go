package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A FAILED request must be logged with the account AND the provider that was
// tried.
//
// REGRESSION: the error branch of finishRequest wrote account_id, provider and
// attempt_count from three requestLogEntry fields (accountID, provider,
// attempts) that were never assigned anywhere in the codebase. account_id was
// rescued later by FailureError.LastAccountID, but there was no equivalent for
// the provider, so every failed request was stored with an empty provider.
//
// The user-visible damage is that the console's provider filter -- the control
// an operator reaches for precisely when debugging one channel -- returned an
// empty list for failures on that channel, hiding the rows it exists to show.
// No existing test caught it because none asserted the field.
func TestFailedRequestLogRecordsAccountAndProvider(t *testing.T) {
	srv, adapter := failingTestServer(t)

	resp := driveFailingChat(t, srv)
	if resp == http.StatusOK {
		t.Fatalf("the stubbed upstream was supposed to fail, got 200")
	}

	row := firstRowWithStatus(t, srv, "error")
	if row == nil {
		t.Fatalf("no failed request was recorded in the log")
	}

	// account_id is what makes a failure attributable at all.
	if got, _ := row["account_id"].(string); got == "" {
		t.Errorf("failed request logged with an empty account_id; the console cannot "+
			"show which account was tried (row=%v)", row)
	}
	// provider is what the console FILTERS on, so it must survive a failure.
	got, _ := row["provider"].(string)
	if got == "" {
		t.Errorf("failed request logged with an empty provider; filtering the log by "+
			"channel would hide this failure (row=%v)", row)
	}
	if got != adapter.family {
		t.Errorf("provider=%q, want %q (the family that was actually tried)",
			got, adapter.family)
	}
}

// The console's provider filter must FIND a failed request, which is the
// observable consequence of the field being populated.
//
// This is the assertion that would have failed before the fix: filtering by the
// channel that just failed returned zero rows.
func TestProviderFilterFindsFailedRequests(t *testing.T) {
	srv, adapter := failingTestServer(t)
	driveFailingChat(t, srv)

	// Both filters describe the same row, so they must agree on how many rows
	// exist. Disagreeing is the bug: one of them silently matches nothing.
	byStatus := len(requestRowsAt(t, srv, "/admin/requests?status=error"))
	byProvider := len(requestRowsAt(t, srv, "/admin/requests?provider="+adapter.family))

	if byStatus == 0 {
		t.Fatalf("no error rows at all; the failure was not logged")
	}
	if byProvider == 0 {
		t.Errorf("filtering by provider=%s returned 0 rows while status=error returned %d; "+
			"the provider filter cannot find failed requests", adapter.family, byStatus)
	}
}

// failingTestServer builds a server whose upstream always fails, so a request
// exhausts its attempts and the error path of finishRequest runs.
func failingTestServer(t *testing.T) (*httptest.Server, *stubProvider) {
	t.Helper()
	adapter := newStubProvider()
	adapter.failuresBeforeSuccess = 99
	adapter.failKind = "unavailable"
	srv, _ := newTestServer(t, adapter)
	return srv, adapter
}

// driveFailingChat issues one completion that is expected to fail, returning
// the status code.
func driveFailingChat(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	req := authRequest(t, http.MethodPost, srv.URL+"/v1/chat/completions",
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// firstRowWithStatus returns the first logged request with the given status.
func firstRowWithStatus(t *testing.T, srv *httptest.Server, status string) map[string]any {
	t.Helper()
	for _, row := range requestRowsAt(t, srv, "/admin/requests?limit=50") {
		if row["status"] == status {
			return row
		}
	}
	return nil
}

// requestRowsAt decodes a request-log listing from an admin query path.
func requestRowsAt(t *testing.T, srv *httptest.Server, path string) []map[string]any {
	t.Helper()
	req := authRequest(t, http.MethodGet, srv.URL+path, "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return payload.Data
}
