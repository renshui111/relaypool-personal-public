package scheduler

import (
	"context"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

// AccountState is the volatile half of an account: the fields the scheduler
// needs that live in the in-memory pool rather than in the database.
type AccountState struct {
	ID          string
	Enabled     bool
	AutoCheckin bool
}

// StoreAccounts adapts the store to the scheduler's AccountSource.
//
// It is a thin adapter rather than the scheduler depending on store directly,
// which keeps the scheduler testable without a database and keeps the store
// free of scheduling concepts.
type StoreAccounts struct {
	Store *store.Store

	// States supplies the in-memory view of each account. When nil, the
	// durable rows are authoritative, which is the correct behaviour for a
	// scheduler running without a live pool.
	States func() []AccountState

	// OnCatalog receives a freshly fetched catalogue. It is a callback rather
	// than a method so the scheduler never imports the pool package, which
	// would invert the dependency direction.
	OnCatalog           func(accountID string, models []provider.ModelInfo)
	OnCredentialRefresh func(accountID string)
}

// SchedulableAccounts merges the durable account rows with the volatile state.
func (a StoreAccounts) SchedulableAccounts() []AccountRef {
	accounts, err := a.Store.ListAccounts(context.Background())
	if err != nil {
		// A store failure yields no work rather than a partial, misleading
		// list: scheduling against an incomplete set could skip an account's
		// check-in for the day.
		return nil
	}

	// Index the volatile state so a lookup is not a linear scan per account.
	state := map[string]AccountState{}
	if a.States != nil {
		for _, entry := range a.States() {
			state[entry.ID] = entry
		}
	}

	out := make([]AccountRef, 0, len(accounts))
	for _, account := range accounts {
		ref := AccountRef{
			ID:          account.ID,
			Name:        account.Name,
			Provider:    account.Provider,
			Region:      account.Region,
			Enabled:     account.Enabled,
			AutoCheckin: account.AutoCheckin,
		}
		if volatile, ok := state[account.ID]; ok {
			// The pool is the live source for enablement: an operator who
			// disabled an account expects the scheduler to stop touching it
			// immediately, without waiting for a database round trip.
			ref.Enabled = volatile.Enabled
			ref.AutoCheckin = volatile.AutoCheckin
		}
		out = append(out, ref)
	}
	return out
}

// RecordCheckin persists a check-in outcome.
func (a StoreAccounts) RecordCheckin(ctx context.Context, accountID, status, message string) error {
	return a.Store.RecordCheckin(ctx, accountID, status, message)
}

// LastCheckinAt reports when an account last checked in successfully.
func (a StoreAccounts) LastCheckinAt(ctx context.Context, accountID string) (time.Time, error) {
	return a.Store.LastCheckinAt(ctx, accountID)
}

// MergeCatalog installs a freshly fetched catalogue.
func (a StoreAccounts) MergeCatalog(accountID string, models []provider.ModelInfo) {
	if a.OnCatalog == nil {
		return
	}
	a.OnCatalog(accountID, models)
}

// SaveRefreshedCredential persists a credential the keep-alive job refreshed.
func (a StoreAccounts) SaveRefreshedCredential(ctx context.Context, accountID string, cred provider.Credential) error {
	payload := cred.Raw
	if len(payload) == 0 {
		return nil
	}
	format := cred.Provider + "-oauth-v1"
	if len(cred.PreviousRaw) > 0 {
		saved, err := a.Store.ReplaceCredentialIfUnchanged(ctx, cred.PreviousRaw, store.Credential{AccountID: accountID, Format: format, Payload: payload, ExpiresAt: cred.ExpiresAt})
		if saved && a.OnCredentialRefresh != nil {
			a.OnCredentialRefresh(accountID)
		}
		return err
	}
	return a.Store.UpsertCredential(ctx, store.Credential{
		AccountID: accountID,
		Format:    format,
		Payload:   payload,
		ExpiresAt: cred.ExpiresAt,
	})
}
