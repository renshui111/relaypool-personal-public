package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider/workbuddy"
	"github.com/relaypool/relaypool/internal/store"
)

func TestWorkBuddyLoginSaveRetryKeepsExpiryAndCompletedStatus(t *testing.T) {
	_, s := newTestServer(t, newStubProvider())
	ctx := context.Background()
	a, err := s.store.CreateAccount(ctx, store.Account{Name: "login fixture", Provider: "workbuddy", Region: "cn", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	s.beginLogin(a, "workbuddy", false)
	p, _ := s.getLogin(a.ID)
	cred := workbuddy.Credential{AccessToken: "synthetic-token", RefreshToken: "synthetic-refresh", UID: "fixture-user", Domain: "codebuddy.cn", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(canceled)
	if err := s.storeBrowserCredential(r, p, cred); err == nil {
		t.Fatal("canceled save unexpectedly succeeded")
	}
	if _, exists := s.getLogin(a.ID); !exists {
		t.Fatal("failed save destroyed login")
	}
	if err := s.storeBrowserCredential(httptest.NewRequest(http.MethodGet, "/", nil), p, cred); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetAccountEnabled(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	s.finishLogin(a.ID, true)
	saved, err := s.store.GetAccount(ctx, a.ID)
	if err != nil || saved.AuthType != "oauth" || saved.CredentialExpiresAt.Unix() != cred.ExpiresAt {
		t.Fatal("saved login metadata missing")
	}
	w := httptest.NewRecorder()
	if !s.replyCompletedWorkBuddyLogin(w, ctx, a.ID) || w.Code != 200 {
		t.Fatal("late poll cannot recover completed login")
	}
	if s.discardLogin(ctx, a.ID) {
		t.Fatal("late cancellation deleted saved login")
	}
	if _, err := s.store.GetCredential(ctx, a.ID); err != nil {
		t.Fatal("saved credential lost")
	}
}

func TestExpiredWorkBuddyPollReportsAlreadySavedLogin(t *testing.T) {
	_, s := newTestServer(t, newStubProvider())
	a := seedLogin(t, s, 2*workbuddy.LoginTimeout)
	p, _ := s.getLogin(a.ID)
	cred := workbuddy.Credential{AccessToken: "synthetic", UID: "user", Domain: "codebuddy.cn", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if err := s.storeBrowserCredential(httptest.NewRequest(http.MethodGet, "/", nil), p, cred); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetAccountEnabled(t.Context(), a.ID, true); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	s.handleAccountLoginStatus(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"done"`) {
		t.Fatalf("completed login reported expired: %s", w.Body.String())
	}
}
