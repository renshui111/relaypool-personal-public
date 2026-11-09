package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

type snapshotCredentials struct{ db *store.Store }

func (c snapshotCredentials) CredentialFor(id string) (provider.Credential, error) {
	r, err := c.db.GetCredential(context.Background(), id)
	return provider.Credential{AccountID: id, Provider: r.Provider, Raw: r.Payload, ExpiresAt: r.ExpiresAt}, err
}

type snapshotRefreshProvider struct {
	*fakeProvider
	entered chan struct{}
	release chan struct{}
	mutate  bool
	claimed string
}

func (p *snapshotRefreshProvider) NeedsCheckinRefresh(provider.Credential) bool { return true }

func (p *snapshotRefreshProvider) Refresh(ctx context.Context, c provider.Credential) (provider.Credential, error) {
	close(p.entered)
	select {
	case <-p.release:
	case <-ctx.Done():
		return provider.Credential{}, ctx.Err()
	}
	raw := []byte(`{"token":"new"}`)
	if p.mutate {
		copy(c.Raw, raw)
		raw = c.Raw
	}
	// Deliberately omit PreviousRaw, as an adapter is allowed to do.
	return provider.Credential{Provider: "workbuddy", Raw: raw, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (p *snapshotRefreshProvider) Checkin(ctx context.Context, c provider.Credential) (provider.CheckinResult, error) {
	p.claimed = string(c.Raw)
	return p.fakeProvider.Checkin(ctx, c)
}

func snapshotStore(t *testing.T) (*store.Store, store.Account) {
	t.Helper()
	db, err := store.OpenWithCredentialKey(t.Context(), filepath.Join(t.TempDir(), "snapshot.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a, err := db.CreateAccount(t.Context(), store.Account{Name: "test", Provider: "workbuddy", Region: "cn", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertCredential(t.Context(), store.Credential{AccountID: a.ID, Format: "workbuddy-oauth-v1", Payload: []byte(`{"token":"old"}`), Nickname: "preserve-me", Domain: "test.invalid", EnterpriseID: "test-enterprise"}); err != nil {
		t.Fatal(err)
	}
	return db, a
}

func TestScheduledRefreshPreservesConcurrentCredentialEdits(t *testing.T) {
	for _, job := range []string{"keepalive", "checkin"} {
		for _, mode := range []string{"unchanged", "replaced", "deleted", "mutated-input"} {
			t.Run(job+"/"+mode, func(t *testing.T) {
				db, a := snapshotStore(t)
				p := &snapshotRefreshProvider{fakeProvider: newFakeProvider(), entered: make(chan struct{}), release: make(chan struct{}), mutate: mode == "mutated-input"}
				reg := provider.NewRegistry()
				reg.Register(p)
				resets := 0
				source := StoreAccounts{Store: db, OnCredentialRefresh: func(string) { resets++ }}
				s := New(reg, source, snapshotCredentials{db}, Options{})
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				finished := make(chan error, 1)
				go func() {
					if job == "keepalive" {
						s.keepaliveOne(ctx, account(a.ID))
						finished <- nil
					} else {
						_, err := s.Checkin(ctx, account(a.ID), true)
						finished <- err
					}
				}()
				select {
				case <-p.entered:
				case <-ctx.Done():
					t.Fatal("refresh did not start")
				}
				want := `{"token":"new"}`
				switch mode {
				case "replaced":
					want = `{"token":"user-replacement"}`
					if err := db.UpsertCredential(ctx, store.Credential{AccountID: a.ID, Format: "workbuddy-oauth-v1", Payload: []byte(want), Nickname: "preserve-me", Domain: "test.invalid", EnterpriseID: "test-enterprise"}); err != nil {
						t.Fatal(err)
					}
				case "deleted":
					if err := db.DeleteCredential(ctx, a.ID); err != nil {
						t.Fatal(err)
					}
				}
				close(p.release)
				jobErr := <-finished
				current, err := db.GetCredential(ctx, a.ID)
				if mode == "deleted" {
					if !errors.Is(err, store.ErrNotFound) || resets != 0 || p.claimed != "" || (job == "checkin" && jobErr == nil) {
						t.Fatal("deleted credential was restored or used by check-in")
					}
					return
				}
				if err != nil || jobErr != nil || string(current.Payload) != want {
					t.Fatalf("unexpected saved credential or job error: read=%v job=%v", err, jobErr)
				}
				if current.Nickname != "preserve-me" || current.Domain != "test.invalid" || current.EnterpriseID != "test-enterprise" {
					t.Fatal("refresh erased credential display metadata")
				}
				wantResets := 1
				if mode == "replaced" {
					wantResets = 0
				}
				if resets != wantResets || (job == "checkin" && p.claimed != want) {
					t.Fatal("health reset or check-in did not match the persisted credential")
				}
			})
		}
	}
}

func TestRefreshSaveRejectsMissingSnapshotOrResult(t *testing.T) {
	for _, mode := range []string{"snapshot", "result"} {
		t.Run(mode, func(t *testing.T) {
			db, a := snapshotStore(t)
			source := StoreAccounts{Store: db, OnCredentialRefresh: func(string) { t.Error("invalid save reset account health") }}
			cred := provider.Credential{Provider: "workbuddy", Raw: []byte(`{"token":"new"}`), PreviousRaw: []byte(`{"token":"old"}`)}
			if mode == "snapshot" {
				cred.PreviousRaw = nil
			} else {
				cred.Raw = nil
			}
			if err := source.SaveRefreshedCredential(t.Context(), a.ID, cred); err == nil {
				t.Fatal("invalid refresh save succeeded")
			}
			current, err := db.GetCredential(t.Context(), a.ID)
			if err != nil || string(current.Payload) != `{"token":"old"}` {
				t.Fatal("invalid refresh changed the stored credential")
			}
		})
	}
}

func TestRefreshWithoutOriginalPayloadDoesNotCallUpstream(t *testing.T) {
	p := newFakeProvider()
	if _, err := refreshWithSnapshot(t.Context(), p, provider.Credential{}); err == nil {
		t.Fatal("refresh without comparison data succeeded")
	}
	if _, _, calls := p.counts(); calls != 0 {
		t.Fatal("rotated a token that could not be safely saved")
	}
}
