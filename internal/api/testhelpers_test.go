package api

import (
	"path/filepath"
	"testing"

	"github.com/relaypool/relaypool/internal/executor"
	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

// openTestStore creates a store backed by a temporary database.
//
// A real store (rather than a stub) is used deliberately: the request path
// touches request logging, credential lookup and runtime-state persistence, and
// stubbing all three would let a schema or query bug pass unnoticed.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relaypool.db")
	st, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

// poolItem builds a routable pool entry.
func poolItem(id, family string) pool.Item {
	return pool.Item{
		ID:          id,
		Name:        id,
		Provider:    family,
		Region:      "cn",
		Enabled:     true,
		Weight:      50,
		MaxInflight: 4,
	}
}

// rebuildExecutor re-creates the executor so a test's fake adapter is used.
//
// New() registers the real WorkBuddy adapter, and registering a fake under the
// same family name would replace it in the registry, which is what the tests
// rely on. The executor holds a credential source bound to the store, so it is
// rebuilt here rather than reassigned piecemeal.
func rebuildExecutor(s *Server, adapter *stubProvider) *executor.Executor {
	exec := executor.New(s.pool, s.registry, s.sessions, credentialSource{
		store:    s.store,
		registry: s.registry,
	}, executor.Options{
		MaxAttempts:          4,
		DefaultCooldown:      s.cfg.Pool.DefaultCooldown,
		MaxRetryAfter:        s.cfg.Pool.MaxRetryAfter,
		MinRateLimitCooldown: s.cfg.Pool.MinRateLimitCooldown,
		BreakerThreshold:     s.cfg.Pool.BreakerThreshold,
	})
	return exec
}

// seedCredential stores a credential blob for an account so the credential
// source can resolve it.
func seedCredential(t *testing.T, st *store.Store, accountID, family string) {
	t.Helper()
	ctx := t.Context()
	if _, err := st.CreateAccount(ctx, store.Account{
		ID:          accountID,
		Name:        accountID,
		Provider:    family,
		Region:      "cn",
		Enabled:     true,
		MaxInflight: 4,
		Weight:      50,
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := st.UpsertCredential(ctx, store.Credential{
		AccountID: accountID,
		Format:    family + "-oauth-v1",
		Payload:   []byte(`{"access_token":"at_test"}`),
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
}

// unusedProviderReference keeps the provider import meaningful for the alias
// declared in transport.go.
var _ = provider.KindInProcess
