package workbuddy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

const catalogFixture = `{"code":0,"data":{"models":[{"id":"fixture-model","supportsReasoning":true,"reasoning":{"supportedEfforts":["low","high"]}}]}}`

func catalogResponse(status int) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(catalogFixture))}
}

func catalogCredential(token, tenant string) provider.Credential {
	return provider.Credential{Fields: map[string]string{"access_token": token, "uid": "same-user", "domain": "codebuddy.cn", "enterprise_id": tenant}}
}

func waitCatalogWaiters(t *testing.T, c *Client, cred provider.Credential, n int) {
	t.Helper()
	decoded, err := c.decodeCredential(cred)
	if err != nil {
		t.Fatal(err)
	}
	key := catalogKey(decoded)
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		c.mu.RLock()
		call := c.catalogPending[key]
		got := 0
		if call != nil {
			got = call.waiters
		}
		c.mu.RUnlock()
		if got == n {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("expected %d catalog waiters, got %d", n, got)
		case <-tick.C:
		}
	}
}

func TestConcurrentCatalogMissesShareOneRequestAndIndependentResults(t *testing.T) {
	c := New(Config{})
	cred := catalogCredential("token", "tenant")
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var calls atomic.Int32
	c.http.Transport = checkinBusinessTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		select {
		case <-release:
			return catalogResponse(200), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	type result struct {
		models []provider.ModelInfo
		err    error
	}
	results := make(chan result, 20)
	for i := 0; i < 20; i++ {
		go func() { m, e := c.FetchCatalog(t.Context(), cred); results <- result{m, e} }()
	}
	waitCatalogWaiters(t, c, cred, 20)
	close(release)
	var all [][]provider.ModelInfo
	for i := 0; i < 20; i++ {
		r := <-results
		if r.err != nil || len(r.models) != 1 {
			t.Fatalf("catalog failed: %v", r.err)
		}
		all = append(all, r.models)
	}
	if calls.Load() != 1 {
		t.Fatalf("sent %d requests for one concurrent miss", calls.Load())
	}
	all[0][0].NativeModel = "modified"
	all[0][0].ReasoningOptions[0] = "modified"
	cached, err := c.FetchCatalog(t.Context(), cred)
	if err != nil || cached[0].NativeModel != "fixture-model" || cached[0].ReasoningOptions[0] == "modified" || all[1][0].NativeModel != "fixture-model" || all[1][0].ReasoningOptions[0] == "modified" || calls.Load() != 1 {
		t.Fatal("caller modified shared catalog or cache miss was repeated")
	}
}

func TestCatalogCancellationIsScopedToWaiters(t *testing.T) {
	c := New(Config{})
	cred := catalogCredential("token", "tenant")
	release := make(chan struct{})
	canceled := make(chan struct{}, 1)
	c.http.Transport = checkinBusinessTransport(func(r *http.Request) (*http.Response, error) {
		select {
		case <-release:
			return catalogResponse(200), nil
		case <-r.Context().Done():
			canceled <- struct{}{}
			return nil, r.Context().Err()
		}
	})
	first, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()
	second, cancelSecond := context.WithCancel(t.Context())
	defer cancelSecond()
	result1 := make(chan error, 1)
	result2 := make(chan error, 1)
	go func() { _, e := c.FetchCatalog(first, cred); result1 <- e }()
	waitCatalogWaiters(t, c, cred, 1)
	go func() { _, e := c.FetchCatalog(second, cred); result2 <- e }()
	waitCatalogWaiters(t, c, cred, 2)
	cancelFirst()
	if !errors.Is(<-result1, context.Canceled) {
		t.Fatal("first waiter did not cancel")
	}
	waitCatalogWaiters(t, c, cred, 1)
	select {
	case <-canceled:
		t.Fatal("one waiter canceled shared upstream")
	default:
	}
	close(release)
	if err := <-result2; err != nil {
		t.Fatal(err)
	}
	// A separate flight with no remaining waiters must release its connection.
	c.InvalidateCatalog(cred)
	release = make(chan struct{})
	last, cancelLast := context.WithCancel(t.Context())
	go func() { _, e := c.FetchCatalog(last, cred); result1 <- e }()
	waitCatalogWaiters(t, c, cred, 1)
	cancelLast()
	if !errors.Is(<-result1, context.Canceled) {
		t.Fatal("last waiter did not cancel")
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("abandoned upstream query was not canceled")
	}
}

func TestCatalogIdentityChangesBypassOldCache(t *testing.T) {
	c := New(Config{})
	var calls int
	c.http.Transport = checkinBusinessTransport(func(r *http.Request) (*http.Response, error) { calls++; return catalogResponse(200), nil })
	for _, cred := range []provider.Credential{catalogCredential("token-a", "tenant-a"), catalogCredential("token-b", "tenant-a"), catalogCredential("token-b", "tenant-b")} {
		if _, err := c.FetchCatalog(t.Context(), cred); err != nil {
			t.Fatal(err)
		}
		decoded, _ := c.decodeCredential(cred)
		key := catalogKey(decoded)
		if len(key) != 64 || strings.Contains(key, "token-") || strings.Contains(key, "same-user") {
			t.Fatal("cache key exposes credential material")
		}
	}
	if calls != 3 {
		t.Fatal("new token or tenant reused old authorization catalog")
	}
}

func TestFailedAndInvalidatedCatalogsDoNotRefillCache(t *testing.T) {
	c := New(Config{})
	cred := catalogCredential("token", "tenant")
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	c.http.Transport = checkinBusinessTransport(func(r *http.Request) (*http.Response, error) {
		n := calls.Add(1)
		if n == 1 {
			close(started)
			<-release
		} // Simulate a transport completing after cancellation.
		if n == 2 {
			return catalogResponse(503), nil
		}
		return catalogResponse(200), nil
	})
	result := make(chan error, 1)
	go func() { _, e := c.FetchCatalog(t.Context(), cred); result <- e }()
	<-started
	c.InvalidateCatalog(cred)
	if _, err := c.FetchCatalog(t.Context(), cred); err == nil {
		t.Fatal("failed upstream request treated as success")
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("invalidated flight returned a successful result")
	}
	if _, err := c.FetchCatalog(t.Context(), cred); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatal("failure or stale in-flight response was cached")
	}
}

func TestCatalogCacheEvictsExpiredAndOldestEntries(t *testing.T) {
	c := New(Config{})
	models := []provider.ModelInfo{{NativeModel: "fixture", ReasoningOptions: []string{"low"}}}
	c.catalog["expired"] = catalogEntry{models: models, fetched: time.Now().Add(-2 * catalogTTL)}
	c.storeCatalog("fresh", models)
	if _, ok := c.catalog["expired"]; ok {
		t.Fatal("expired entry retained")
	}
	models[0].ReasoningOptions[0] = "modified"
	got, _ := c.cachedCatalog("fresh")
	if got[0].ReasoningOptions[0] != "low" {
		t.Fatal("cache did not own its input")
	}
	for i := 1; i < maxCatalogEntries; i++ {
		c.catalog[fmt.Sprint(i)] = catalogEntry{fetched: time.Now()}
	}
	c.catalog["fresh"] = catalogEntry{fetched: time.Now().Add(-time.Minute)}
	c.storeCatalog("new", nil)
	if len(c.catalog) != maxCatalogEntries {
		t.Fatal("cache exceeded bound")
	}
	if _, ok := c.catalog["fresh"]; ok {
		t.Fatal("oldest entry was not evicted")
	}
}
