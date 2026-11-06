package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

type rewardRefreshProvider struct {
	*fakeProvider
	seen string
}

func (p *rewardRefreshProvider) NeedsCheckinRefresh(provider.Credential) bool { return true }
func (p *rewardRefreshProvider) Checkin(ctx context.Context, c provider.Credential) (provider.CheckinResult, error) {
	p.seen = string(c.Raw)
	return p.fakeProvider.Checkin(ctx, c)
}

type rewardSavingAccounts struct {
	*fakeAccounts
	creds       *fakeCreds
	failure     bool
	replacement bool
	saved       bool
}

func (a *rewardSavingAccounts) SaveRefreshedCredential(_ context.Context, id string, c provider.Credential) error {
	if a.failure {
		return errors.New("test persistence failed")
	}
	a.saved = true
	if a.replacement {
		c.Raw = []byte(`{"access_token":"user-replaced"}`)
	}
	a.creds.byID[id] = c
	return nil
}

func TestCheckinRefreshPersistsBeforeClaim(t *testing.T) {
	for _, mode := range []string{"success", "save-failure", "refresh-failure", "no-saver", "replaced"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, time.Now(), account("a"))
			adapter := &rewardRefreshProvider{fakeProvider: h.provider}
			h.scheduler.registry.Register(adapter)
			source := &rewardSavingAccounts{fakeAccounts: h.accounts, creds: h.creds, failure: mode == "save-failure", replacement: mode == "replaced"}
			if mode != "no-saver" {
				h.scheduler.accounts = source
			}
			if mode == "refresh-failure" {
				h.provider.refreshErr = errors.New("test refresh failed")
			}
			result, err := h.scheduler.Checkin(t.Context(), account("a"), true)
			_, claims, refreshes := h.provider.counts()
			if mode == "success" || mode == "replaced" {
				want := `{"access_token":"refreshed"}`
				if mode == "replaced" {
					want = `{"access_token":"user-replaced"}`
				}
				if err != nil || result.Status != provider.CheckinSuccess || !source.saved || adapter.seen != want || claims != 1 || refreshes != 1 {
					t.Fatalf("claim did not use persisted credential: %v", err)
				}
			} else if err == nil || claims != 0 {
				t.Fatal("claim proceeded after refresh/persistence failure")
			}
			if mode == "no-saver" && refreshes != 0 {
				t.Fatal("rotated one-use token without persistence")
			}
		})
	}
}
