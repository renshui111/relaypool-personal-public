package api

import (
	"context"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider/workbuddy"
	"github.com/relaypool/relaypool/internal/store"
)

// Abandoned browser logins must be reaped.
//
// WHY THIS FILE EXISTS: expiry was originally checked only inside the poll
// handler, so a login was cleaned up only if a client kept polling. An operator
// who opened the dialog and closed the tab -- the most likely way to abandon a
// login -- left both a map entry and a DISABLED ACCOUNT ROW behind, forever.
// The row is the visible half: it shows up in the console as a broken account
// that no longer has any way to be completed.

// seedLogin inserts an in-progress login directly, so the test does not need a
// reachable upstream.
func seedLogin(t *testing.T, srv *Server, age time.Duration) store.Account {
	t.Helper()
	ctx := context.Background()
	account, err := srv.store.CreateAccount(ctx, store.Account{
		Name: "abandoned", Provider: "workbuddy", Region: "cn", Enabled: false,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	srv.loginMu.Lock()
	if srv.logins == nil {
		srv.logins = map[string]pendingLogin{}
	}
	srv.logins[account.ID] = pendingLogin{
		id:       account.ID,
		account:  account,
		provider: "workbuddy",
		started:  time.Now().Add(-age),
	}
	srv.loginMu.Unlock()
	return account
}

// An expired login must be reaped WITHOUT anyone polling it, and its account row
// must be removed: that row is what the operator actually sees.
func TestExpiredLoginIsReapedWithoutAPoll(t *testing.T) {
	srv, server := newTestServer(t, newStubProvider())
	account := seedLogin(t, server, 2*workbuddy.LoginTimeout)

	server.reapLogins(context.Background())

	server.loginMu.Lock()
	_, stillTracked := server.logins[account.ID]
	server.loginMu.Unlock()
	if stillTracked {
		t.Error("an expired login is still tracked; nothing reaps it unless a client polls")
	}

	if _, err := server.store.GetAccount(context.Background(), account.ID); err == nil {
		t.Error("the abandoned account row still exists; it shows in the console as a " +
			"broken account with no way to complete the login")
	}
	_ = srv
}

// A login that is still within its window must NOT be reaped: the user may be
// midway through signing in.
func TestLiveLoginIsNotReaped(t *testing.T) {
	_, server := newTestServer(t, newStubProvider())
	account := seedLogin(t, server, time.Second)

	server.reapLogins(context.Background())

	server.loginMu.Lock()
	_, stillTracked := server.logins[account.ID]
	server.loginMu.Unlock()
	if !stillTracked {
		t.Error("a login that is still within its window was reaped; the user may be " +
			"midway through signing in")
	}
	if _, err := server.store.GetAccount(context.Background(), account.ID); err != nil {
		t.Errorf("a live login's account row was deleted: %v", err)
	}
}

// Reaping must free the map rather than merely dropping the row, or a long-lived
// process accumulates one entry per abandoned attempt.
func TestReapFreesManyAbandonedLogins(t *testing.T) {
	_, server := newTestServer(t, newStubProvider())

	const n = 25
	for i := 0; i < n; i++ {
		seedLogin(t, server, 2*workbuddy.LoginTimeout)
	}
	server.loginMu.Lock()
	before := len(server.logins)
	server.loginMu.Unlock()
	if before != n {
		t.Fatalf("setup seeded %d logins, expected %d", before, n)
	}

	server.reapLogins(context.Background())

	server.loginMu.Lock()
	after := len(server.logins)
	server.loginMu.Unlock()
	if after != 0 {
		t.Errorf("logins tracked after the sweep = %d, want 0; the map is not being freed", after)
	}

	remaining, err := server.store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	for _, a := range remaining {
		if a.Name == "abandoned" {
			t.Errorf("account %s survived the sweep with no credential and no login", a.ID)
		}
	}
}

// The sweeper must stop when the server does, or a long-running test binary
// leaks a goroutine that keeps touching a closed store.
func TestLoginSweeperStopsOnClose(t *testing.T) {
	_, server := newTestServer(t, newStubProvider())
	if server.loginCancel == nil {
		t.Fatal("the login sweeper was never started, so abandoned logins are never reaped")
	}
	// Close must not panic, and must be safe to call with a live sweeper.
	server.Close()
}
