package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

type quotaRenewProvider struct {
	*reloadProvider
	seen   string
	claims int
}

func (*quotaRenewProvider) SupportsQuota() bool { return true }
func (*quotaRenewProvider) NeedsQuotaRefresh(c provider.Credential) bool {
	return c.Field("expired") == "yes"
}
func (p *quotaRenewProvider) FetchQuota(_ context.Context, c provider.Credential) (provider.QuotaSnapshot, error) {
	p.seen = c.Field("access_token")
	n := 123.0
	return provider.QuotaSnapshot{State: "ok", Amounts: []provider.QuotaAmount{{Unit: "积分", Remaining: &n}}}, nil
}
func (p *quotaRenewProvider) Checkin(context.Context, provider.Credential) (provider.CheckinResult, error) {
	p.claims++
	return provider.CheckinResult{}, errors.New("must not claim")
}

func TestQuotaRenewsOnlyWhenRequiredAndPersistsBeforeQuery(t *testing.T) {
	for _, mode := range []string{"expired", "fresh", "replaced", "failed"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestServerNoAdapter(t)
			seedCredential(t, s.store, "quota-renew", "workbuddy")
			item := poolItem("quota-renew", "workbuddy")
			item.Enabled = false
			s.pool.Upsert(item)
			if err := s.store.SetAccountEnabled(t.Context(), item.ID, false); err != nil {
				t.Fatal(err)
			}
			old := `{"access_token":"old","expired":"yes"}`
			if mode == "fresh" {
				old = `{"access_token":"old"}`
			}
			if err := s.store.UpsertCredential(t.Context(), store.Credential{AccountID: item.ID, Format: "test", Payload: []byte(old)}); err != nil {
				t.Fatal(err)
			}
			p := &quotaRenewProvider{reloadProvider: &reloadProvider{stubProvider: newStubProvider()}}
			p.refresh = func(ctx context.Context, c provider.Credential) (provider.Credential, error) {
				if mode == "failed" {
					return provider.Credential{}, errors.New("synthetic-secret")
				}
				if mode == "replaced" {
					if err := s.store.UpsertCredential(ctx, store.Credential{AccountID: item.ID, Format: "test", Payload: []byte(`{"access_token":"manual"}`)}); err != nil {
						t.Fatal(err)
					}
				}
				return provider.Credential{Raw: []byte(`{"access_token":"new"}`), Fields: map[string]string{"access_token": "new"}}, nil
			}
			s.registry.Register(p)
			out := callComplete(t, s, "GET", "/admin/accounts/"+item.ID+"/inspect/quota", "")
			if mode == "failed" {
				if !strings.Contains(out.Body.String(), `"state":"error"`) || strings.Contains(out.Body.String(), "synthetic-secret") || p.seen != "" {
					t.Fatal("failed renewal leaked or queried quota")
				}
			} else {
				want := "new"
				if mode == "fresh" {
					want = "old"
				}
				if mode == "replaced" {
					want = "manual"
				}
				stored, _ := s.store.GetCredential(t.Context(), item.ID)
				if p.seen != want || !strings.Contains(string(stored.Payload), want) || !strings.Contains(out.Body.String(), `"remaining":123`) {
					t.Fatal("quota did not use persisted current credential")
				}
				callComplete(t, s, "GET", "/admin/accounts/"+item.ID+"/inspect/quota", "")
			}
			wantCalls := 1
			if mode == "fresh" {
				wantCalls = 0
			}
			if p.calls != wantCalls || p.claims != 0 {
				t.Fatal("unnecessary rotation or reward claim")
			}
			a, _ := s.store.GetAccount(t.Context(), item.ID)
			live, _ := s.pool.ByID(item.ID)
			if a.Enabled || live.Enabled {
				t.Fatal("quota enabled disabled account")
			}
		})
	}
}
