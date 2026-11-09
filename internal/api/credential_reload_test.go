package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

type reloadProvider struct {
	*stubProvider
	refresh func(context.Context, provider.Credential) (provider.Credential, error)
	calls   int
}

func (p *reloadProvider) DecodeCredential(raw []byte) (provider.Credential, error) {
	var fields map[string]string
	if err := json.Unmarshal(raw, &fields); err != nil {
		return provider.Credential{}, err
	}
	// Raw, Provider, and account identity are not guaranteed by all decoders.
	return provider.Credential{AccountID: "adapter-placeholder", Provider: fields["provider"], Region: provider.Region(fields["region"]), Fields: fields}, nil
}

func (p *reloadProvider) NeedsRefresh(provider.Credential) bool { return true }
func (p *reloadProvider) Refresh(ctx context.Context, c provider.Credential) (provider.Credential, error) {
	p.calls++
	return p.refresh(ctx, c)
}

func TestRequestCredentialReloadValidation(t *testing.T) {
	for _, mode := range []string{"success", "mutated-input", "empty-result", "refresh-provider", "refresh-region", "replacement", "replacement-provider", "replacement-region", "account-provider", "account-region", "deleted", "initial-provider"} {
		t.Run(mode, func(t *testing.T) {
			db := openTestStore(t)
			defer db.Close()
			const id = "reload-account"
			seedCredential(t, db, id, "workbuddy")
			old := `{"access_token":"old"}`
			if mode == "initial-provider" {
				old = `{"access_token":"old","provider":"other"}`
			}
			if err := db.UpsertCredential(t.Context(), store.Credential{AccountID: id, Format: "test", Payload: []byte(old)}); err != nil {
				t.Fatal(err)
			}
			wantSaved := old
			wantToken := "new"
			p := &reloadProvider{stubProvider: &stubProvider{family: "workbuddy"}}
			p.refresh = func(ctx context.Context, c provider.Credential) (provider.Credential, error) {
				fresh := provider.Credential{Raw: []byte(`{"access_token":"new"}`), Fields: map[string]string{"access_token": "new"}}
				switch mode {
				case "empty-result":
					fresh.Raw = nil
				case "refresh-provider":
					fresh.Provider = "other"
				case "refresh-region":
					fresh.Region = provider.RegionGlobal
				case "success", "mutated-input":
					wantSaved = string(fresh.Raw)
					if mode == "mutated-input" {
						copy(c.Raw, fresh.Raw)
						fresh.Raw = c.Raw
					}
				case "deleted":
					if err := db.DeleteCredential(ctx, id); err != nil {
						t.Fatal(err)
					}
				default:
					wantSaved = `{"access_token":"manual"}`
					wantToken = "manual"
					if mode == "replacement-provider" {
						wantSaved = `{"access_token":"manual","provider":"other"}`
					}
					if mode == "replacement-region" || mode == "account-region" {
						wantSaved = `{"access_token":"manual","region":"global"}`
					}
					if mode == "account-provider" || mode == "account-region" {
						a, err := db.GetAccount(ctx, id)
						if err != nil {
							t.Fatal(err)
						}
						if mode == "account-provider" {
							a.Provider = "other"
						} else {
							a.Region = "global"
						}
						if err := db.UpdateAccount(ctx, a); err != nil {
							t.Fatal(err)
						}
					}
					if err := db.UpsertCredential(ctx, store.Credential{AccountID: id, Format: "manual", Payload: []byte(wantSaved)}); err != nil {
						t.Fatal(err)
					}
				}
				return fresh, nil
			}
			registry := provider.NewRegistry()
			registry.Register(p)
			resets := 0
			source := credentialSource{store: db, registry: registry, OnRefreshed: func(string) { resets++ }}
			got, err := source.CredentialForContext(t.Context(), id)
			wantSuccess := mode == "success" || mode == "mutated-input" || mode == "replacement"
			if wantSuccess {
				if err != nil || got.AccountID != id || got.Provider != "workbuddy" || string(got.Raw) != wantSaved || got.Field("access_token") != wantToken {
					t.Fatalf("credential reload lost identity or payload: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid or changed credential route was accepted")
			} else if strings.Contains(err.Error(), "manual") || strings.Contains(err.Error(), "access_token") {
				t.Fatal("validation exposed credential contents")
			}
			wantResets := 0
			if mode == "success" || mode == "mutated-input" {
				wantResets = 1
			}
			if resets != wantResets || (mode == "initial-provider" && p.calls != 0) {
				t.Fatal("unexpected refresh or account health reset")
			}
			current, readErr := db.GetCredential(t.Context(), id)
			if mode == "deleted" {
				if readErr == nil {
					t.Fatal("deleted credential was recreated")
				}
			} else if readErr != nil || string(current.Payload) != wantSaved {
				t.Fatalf("unexpected persisted credential: %v", readErr)
			}
		})
	}
}
