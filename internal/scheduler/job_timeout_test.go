package scheduler

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

// The first account stalls until cancellation; the second responds normally.
type stalledJobProvider struct {
	*fakeProvider
	seen        []string
	lateCatalog bool
	stopped     error
}

func (p *stalledJobProvider) wait(ctx context.Context, c provider.Credential) error {
	p.seen = append(p.seen, c.AccountID)
	if c.AccountID != "slow" {
		return nil
	}
	<-ctx.Done()
	p.stopped = ctx.Err()
	return ctx.Err()
}

func (p *stalledJobProvider) FetchCatalog(ctx context.Context, c provider.Credential) ([]provider.ModelInfo, error) {
	if err := p.wait(ctx, c); err != nil && !p.lateCatalog {
		return nil, err
	}
	return []provider.ModelInfo{{NativeModel: "new-model"}}, nil
}

func (p *stalledJobProvider) Refresh(ctx context.Context, c provider.Credential) (provider.Credential, error) {
	if err := p.wait(ctx, c); err != nil {
		return provider.Credential{}, err
	}
	c.Raw = []byte(`{"access_token":"new"}`)
	return c, nil
}

type jobSavingAccounts struct {
	*fakeAccounts
	saved []string
}

func (a *jobSavingAccounts) SaveRefreshedCredential(ctx context.Context, id string, _ provider.Credential) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.saved = append(a.saved, id)
	return nil
}

func TestBackgroundJobTimeoutContinuesNextAccount(t *testing.T) {
	for _, job := range []string{"catalog", "late-catalog", "keepalive"} {
		t.Run(job, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t, time.Now(), account("slow"), account("fast"))
				p := &stalledJobProvider{fakeProvider: h.provider, lateCatalog: job == "late-catalog"}
				h.scheduler.registry.Register(p)
				a := &jobSavingAccounts{fakeAccounts: h.accounts}
				h.scheduler.accounts = a
				h.accounts.catalogs["slow"] = []provider.ModelInfo{{NativeModel: "previous-model"}}
				start := time.Now()
				wantElapsed := 30 * time.Second
				if job == "keepalive" {
					wantElapsed = 60 * time.Second
					h.scheduler.runKeepalive(t.Context())
					if !reflect.DeepEqual(a.saved, []string{"fast"}) {
						t.Fatalf("persisted accounts = %v, want only fast", a.saved)
					}
				} else {
					h.scheduler.refreshCatalogs(t.Context())
					if got := h.accounts.catalogs["slow"][0].NativeModel; got != "previous-model" {
						t.Fatalf("failed refresh replaced previous catalog: %s", got)
					}
					if got := h.accounts.catalogs["fast"]; len(got) != 1 || got[0].NativeModel != "new-model" {
						t.Fatalf("next account catalog not refreshed: %v", got)
					}
				}
				if elapsed := time.Since(start); elapsed != wantElapsed {
					t.Fatalf("elapsed %s, want %s", elapsed, wantElapsed)
				}
				if !errors.Is(p.stopped, context.DeadlineExceeded) || !reflect.DeepEqual(p.seen, []string{"slow", "fast"}) {
					t.Fatalf("sweep did not isolate timeout: seen=%v err=%v", p.seen, p.stopped)
				}
			})
		})
	}
}

func TestBackgroundJobsRespectParentCancellation(t *testing.T) {
	for _, job := range []string{"catalog", "keepalive"} {
		for _, mode := range []string{"deadline", "cancel"} {
			t.Run(job+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					h := newHarness(t, time.Now(), account("slow"), account("fast"))
					p := &stalledJobProvider{fakeProvider: h.provider}
					h.scheduler.registry.Register(p)
					ctx, cancel := context.WithCancel(t.Context())
					wantErr := context.Canceled
					if mode == "deadline" {
						cancel()
						ctx, cancel = context.WithTimeout(t.Context(), time.Second)
						wantErr = context.DeadlineExceeded
					} else {
						time.AfterFunc(time.Second, cancel)
					}
					defer cancel()
					start := time.Now()
					if job == "catalog" {
						h.scheduler.refreshCatalogs(ctx)
					} else {
						h.scheduler.runKeepalive(ctx)
					}
					if time.Since(start) != time.Second || !errors.Is(p.stopped, wantErr) || !reflect.DeepEqual(p.seen, []string{"slow"}) {
						t.Fatalf("parent cancellation ignored: elapsed=%s seen=%v err=%v", time.Since(start), p.seen, p.stopped)
					}
				})
			})
		}
	}
}
