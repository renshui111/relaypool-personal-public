package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider/workbuddy"
	"github.com/relaypool/relaypool/internal/store"
)

// pollLoginStatus calls the status endpoint and returns the reported status.
func pollLoginStatus(t *testing.T, srvURL, id string) string {
	t.Helper()
	req := authRequest(t, http.MethodGet, srvURL+"/admin/accounts/login/"+id, "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode poll response %q: %v", raw, err)
	}
	return body.Status
}

// The sweeper must never delete an account whose login SUCCEEDED.
//
// THE BUG THIS PINS: reapLogins snapshots the expired ids under the lock,
// releases it, then for each id calls finishLogin and deletes the account row.
// finishLogin silently does nothing when the entry is already gone, and the
// delete happened anyway. So if a poll completed the login between the snapshot
// and the delete, the sweep destroyed a WORKING account, and because
// credentials.account_id references accounts(id) ON DELETE CASCADE, its
// credential vanished with it. That is unrecoverable data loss.
//
// The window is real, not theoretical: the console polls every 2 seconds. A poll
// that passes its own expiry check and is waiting on the upstream token exchange
// can cross the timeout boundary while the sweeper ticks.

// enableAccountWithCredential makes an account look like a completed login:
// enabled, with a readable credential.
func enableAccountWithCredential(t *testing.T, srv *Server, id string) store.Account {
	t.Helper()
	ctx := context.Background()

	payload := []byte(`{"access_token":"at_live","refresh_token":"rt_live","uid":"u_live","domain":"codebuddy.cn"}`)
	if err := srv.store.UpsertCredential(ctx, store.Credential{
		AccountID: id,
		Format:    workbuddy.CredentialFormat,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("store credential: %v", err)
	}
	if err := srv.store.SetAccountEnabled(ctx, id, true); err != nil {
		t.Fatalf("enable account: %v", err)
	}
	account, err := srv.store.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return account
}

// The exact race: the sweep has already decided this id is abandoned, but the
// login completed in the meantime.
func TestSweepMustNotDeleteAnAccountThatJustCompleted(t *testing.T) {
	srv, server := newTestServer(t, newStubProvider())
	ctx := context.Background()

	// An expired-looking login, so the next sweep will target it.
	account, err := server.store.CreateAccount(ctx, store.Account{
		Name: "just-completed", Provider: "workbuddy", Region: "cn", Enabled: false,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	server.beginLogin(account, "workbuddy", false)
	server.loginMu.Lock()
	pending := server.logins[account.ID]
	pending.started = time.Now().Add(-2 * workbuddy.LoginTimeout)
	server.logins[account.ID] = pending
	server.loginMu.Unlock()

	// A poll finishes the login FIRST: credential stored, account enabled, and
	// the login removed from the map. This is what handleAccountLoginStatus does
	// on success.
	live := enableAccountWithCredential(t, server, account.ID)
	server.finishLogin(account.ID, true)

	// Now the sweep runs with its (already stale) view that this id expired.
	server.reapLogins(ctx)

	// The account was live and must survive.
	got, err := server.store.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("DATA LOSS: the sweep deleted an account whose login had already "+
			"succeeded: %v", err)
	}
	if !got.Enabled {
		t.Errorf("the surviving account was left disabled (enabled=%v)", got.Enabled)
	}
	if _, err := server.store.GetCredential(ctx, account.ID); err != nil {
		t.Errorf("DATA LOSS: the account survived but its credential is gone "+
			"(credentials cascade on account delete): %v", err)
	}
	_ = live
	_ = srv
}

// The same race, made deterministic instead of hoped for.
//
// The sweep does two things: it picks candidates from the map, and it later
// deletes rows. A completing poll can interleave between them, in two distinct
// ways, and BOTH must leave the account alone:
//
//	(a) the poll removes the map entry first (its last step), so the sweep's
//	    removal loses;
//	(b) the poll has written the credential and enabled the account but has not
//	    yet reached its own finishLogin, so the map entry is STILL present and the
//	    sweep's removal wins.
//
// Case (b) is the one a map-ownership check alone cannot catch, so it is tested
// directly here. Guessing at the timing would make this test pass by luck.
func TestDiscardLoginLeavesACompletedLoginAlone(t *testing.T) {
	cases := []struct {
		name string
		// removeEntryFirst models the poll having finished (a); otherwise the
		// poll is mid-flight and still holds the map entry (b).
		removeEntryFirst bool
	}{
		{name: "poll already removed the entry", removeEntryFirst: true},
		{name: "poll is mid-flight and still holds the entry", removeEntryFirst: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, server := newTestServer(t, newStubProvider())
			ctx := context.Background()

			account, err := server.store.CreateAccount(ctx, store.Account{
				Name: "racing", Provider: "workbuddy", Region: "cn", Enabled: false,
			})
			if err != nil {
				t.Fatalf("create account: %v", err)
			}
			server.beginLogin(account, "workbuddy", false)

			// The poll's work: credential stored, account enabled.
			enableAccountWithCredential(t, server, account.ID)

			if tc.removeEntryFirst {
				server.finishLogin(account.ID, true)
			}

			// The sweep acts on its stale candidate list.
			server.discardLogin(ctx, account.ID)

			got, err := server.store.GetAccount(ctx, account.ID)
			if err != nil {
				t.Fatalf("DATA LOSS: discarding an abandoned login deleted an account "+
					"whose login had succeeded: %v", err)
			}
			if !got.Enabled {
				t.Errorf("the surviving account was left disabled")
			}
			if _, err := server.store.GetCredential(ctx, account.ID); err != nil {
				t.Errorf("DATA LOSS: the account survived but its credential is gone "+
					"(credentials cascade on account delete): %v", err)
			}
		})
	}
}

// Conversely, a genuinely abandoned login must still be cleaned up: the guards
// must not turn the sweeper into a no-op.
func TestDiscardLoginStillRemovesAnAbandonedAccount(t *testing.T) {
	_, server := newTestServer(t, newStubProvider())
	ctx := context.Background()

	account, err := server.store.CreateAccount(ctx, store.Account{
		Name: "abandoned", Provider: "workbuddy", Region: "cn", Enabled: false,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	server.beginLogin(account, "workbuddy", false)

	server.discardLogin(ctx, account.ID)

	if _, err := server.store.GetAccount(ctx, account.ID); err == nil {
		t.Error("a genuinely abandoned login's account row was not removed; the guards " +
			"have made the sweeper a no-op and credential-less rows will accumulate")
	}
	server.loginMu.Lock()
	_, tracked := server.logins[account.ID]
	server.loginMu.Unlock()
	if tracked {
		t.Error("the login is still tracked after being discarded")
	}
}

// A poll that hits its own expiry must not leave a permanently orphaned row.
//
// THE BUG THIS PINS: the expired branch in handleAccountLoginStatus calls
// finishLogin (which drops the map entry) but never deletes the account row, and
// the sweeper's only source of candidates is that map. So the row becomes
// unreachable: nothing can ever delete it again. The console makes this path
// likely, because on "expired" it stops polling without calling cancel.
func TestPollExpiryLeavesNoPermanentOrphanRow(t *testing.T) {
	srv, server := newTestServer(t, newStubProvider())
	ctx := context.Background()

	account, err := server.store.CreateAccount(ctx, store.Account{
		Name: "expired-by-poll", Provider: "workbuddy", Region: "cn", Enabled: false,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	server.beginLogin(account, "workbuddy", false)
	server.loginMu.Lock()
	pending := server.logins[account.ID]
	pending.started = time.Now().Add(-2 * workbuddy.LoginTimeout)
	server.logins[account.ID] = pending
	server.loginMu.Unlock()

	// Poll it: the handler sees an expired login and reports "expired".
	status := pollLoginStatus(t, srv.URL, account.ID)
	if status != "expired" {
		t.Fatalf("expected the poll to report expired, got %q", status)
	}

	// A sweep afterwards cannot help, because the map entry is gone: that is
	// exactly the point. The row must already be gone.
	server.reapLogins(ctx)

	if _, err := server.store.GetAccount(ctx, account.ID); err == nil {
		rows, _ := server.store.ListAccounts(ctx)
		t.Errorf("PERMANENT ORPHAN: the poll reported \"expired\" and the disabled row still "+
			"exists (accounts now: %d). The sweeper cannot find it, because this branch removed "+
			"the map entry it would have used as a candidate, so nothing can ever delete it", len(rows))
	}
}
