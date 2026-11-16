package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type benefitTransport func(*http.Request) (*http.Response, error)

func TestBenefitsConcurrentTimeoutIsShared(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		f := benefitFeed{client: &http.Client{Transport: benefitTransport(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}}
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Go(func() {
				if _, _, err := f.read(t.Context(), "/api/agent/search"); !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("expected upstream timeout, got %v", err)
				}
			})
		}
		wg.Wait()
		if got := calls.Load(); got != 1 {
			t.Fatalf("concurrent timeout made %d calls, want 1", got)
		}
	})
}

func TestBenefitsCanceledLeaderDoesNotCancelWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		f := benefitFeed{client: &http.Client{Transport: benefitTransport(func(r *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return benefitResponse(`{"offers":[]}`), nil
		})}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, _, err := f.read(ctx, "/api/agent/search"); !errors.Is(err, context.Canceled) {
				t.Errorf("leader cancellation lost: %v", err)
			}
		})
		synctest.Wait()
		wg.Go(func() {
			if _, stale, err := f.read(t.Context(), "/api/agent/search"); err != nil || stale {
				t.Errorf("waiter inherited cancellation: %v, %v", stale, err)
			}
		})
		synctest.Wait()
		cancel()
		wg.Wait()
		if got := calls.Load(); got != 2 {
			t.Fatalf("calls = %d, want canceled fetch plus successful retry", got)
		}
	})
}

func TestBenefitsConcurrentFailureIsShared(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(strconv.FormatBool(cached), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				release := make(chan struct{})
				f := benefitFeed{client: &http.Client{Transport: benefitTransport(func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					<-release
					return nil, errors.New("offline")
				})}}
				const path = "/api/agent/search"
				if cached {
					f.cache = map[string]benefitCacheEntry{path: {body: json.RawMessage(`{"offers":[]}`), at: time.Now().Add(-time.Hour)}}
					f.flights = make(map[string]*benefitFlight)
				}
				var wg sync.WaitGroup
				for i := 0; i < 12; i++ {
					wg.Go(func() {
						entry, stale, err := f.read(t.Context(), path)
						if cached {
							if err != nil || !stale || len(entry.body) == 0 {
								t.Errorf("missing stale fallback: %v, %v", stale, err)
							}
						} else if err == nil {
							t.Error("upstream failure was hidden")
						}
					})
				}
				synctest.Wait()
				close(release)
				wg.Wait()
				if got := calls.Load(); got != 1 {
					t.Fatalf("concurrent failure made %d upstream calls, want 1", got)
				}
			})
		})
	}
}

func (f benefitTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func benefitResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestBenefitsQueryBoundary(t *testing.T) {
	for _, raw := range []string{"url=http://localhost", "status=active", "modality=unknown", "offset=-1", "offset=10001", "q=a&q=b", "q=" + strings.Repeat("字", 121)} {
		q, _ := url.ParseQuery(raw)
		if _, err := benefitSearchQuery(q); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	q, err := benefitSearchQuery(url.Values{"q": {"华为 & Qoder"}, "modality": {"code"}, "offset": {"12"}})
	if err != nil || q.Get("limit") != "12" || q.Get("status") != "all" || q.Get("q") != "华为 & Qoder" {
		t.Fatal(q, err)
	}
	for _, id := range []string{"../search", "https://example.com", "a?b", "a/b"} {
		if benefitID.MatchString(id) {
			t.Fatal(id)
		}
	}
}

func TestBenefitsAuthAndNoCredentialForwarding(t *testing.T) {
	s := newTestServerNoAdapter(t)
	var calls atomic.Int32
	s.benefits.client = &http.Client{Transport: benefitTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Host != "ai-yangmao.com" || r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Body != nil {
			t.Error("private data or uncontrolled destination", r.URL, r.Header)
		}
		return benefitResponse(`{"offers":[],"total":0,"nextOffset":null}`), nil
	})}
	for _, path := range []string{"/admin/benefits", "/admin/benefits/identity-test", "/admin/announcements"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	created := callComplete(t, s, "POST", "/admin/keys", `{"name":"benefit inference"}`)
	var key struct{ Key string }
	if json.Unmarshal(created.Body.Bytes(), &key) != nil || key.Key == "" {
		t.Fatal(created.Body.String())
	}
	r := httptest.NewRequest("GET", "/admin/benefits", nil)
	r.Header.Set("Authorization", "Bearer "+key.Key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 || calls.Load() != 0 {
		t.Fatal("inference key reached catalogue")
	}
	w = callComplete(t, s, "GET", "/admin/benefits?q=test", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if calls.Load() != 1 || !strings.Contains(w.Body.String(), `"offers":[]`) {
		t.Fatal(w.Body.String(), calls.Load())
	}
	if callComplete(t, s, "GET", "/admin/benefits?url=http://localhost", "").Code != 400 || calls.Load() != 1 {
		t.Fatal("unvalidated destination reached upstream")
	}
}

func TestBenefitsCacheCoalescingAndStaleFallback(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	f := benefitFeed{client: &http.Client{Transport: benefitTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if fail.Load() {
			return nil, errors.New("offline")
		}
		return benefitResponse(`{"offers":[],"total":0}`), nil
	})}}
	const path = "/api/agent/search?status=all"
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			if _, stale, err := f.read(t.Context(), path); err != nil || stale {
				t.Error(err, stale)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("concurrent requests not coalesced", calls.Load())
	}
	f.mu.Lock()
	old := f.cache[path]
	old.at = time.Now().Add(-time.Hour)
	f.cache[path] = old
	f.mu.Unlock()
	fail.Store(true)
	entry, stale, err := f.read(t.Context(), path)
	if err != nil || !stale || !entry.at.Equal(old.at) {
		t.Fatal("stale data not labelled", stale, err)
	}
	f.mu.Lock()
	old.at = time.Now().Add(-25 * time.Hour)
	f.cache[path] = old
	f.mu.Unlock()
	if _, _, err := f.read(t.Context(), path); err == nil {
		t.Fatal("expired fallback served")
	}
}

func TestBenefitsRejectsInvalidAndOversizeResponses(t *testing.T) {
	for _, body := range []string{`not json`, `{"error":"unavailable"}`, strings.Repeat("x", (1<<20)+1)} {
		f := benefitFeed{client: &http.Client{Transport: benefitTransport(func(*http.Request) (*http.Response, error) { return benefitResponse(body), nil })}}
		if _, _, err := f.read(t.Context(), "/api/agent/search"); err == nil {
			t.Fatal("invalid data accepted")
		}
	}
}

func TestBenefitsValidatesResponseBeforeCaching(t *testing.T) {
	for _, tc := range []struct{ path, invalid, valid string }{
		{"/api/agent/search", `{"offers":null}`, `{"offers":[]}`},
		{"/api/agent/search", `{"offers":{}}`, `{"offers":[]}`},
		{"/api/agent/search", `{"offer":{"id":"a"}}`, `{"offers":[]}`},
		{"/api/agent/search", `{"offers":[null]}`, `{"offers":[]}`},
		{"/api/agent/search", `{"offers":[{"title":"missing id"}]}`, `{"offers":[]}`},
		{"/api/agent/offers/a", `{"offer":null}`, `{"offer":{"id":"a"}}`},
		{"/api/agent/offers/a", `{"offer":[]}`, `{"offer":{"id":"a"}}`},
		{"/api/agent/offers/a", `{"offer":{"id":"b"}}`, `{"offer":{"id":"a"}}`},
	} {
		t.Run(tc.invalid+tc.path, func(t *testing.T) {
			calls := 0
			f := benefitFeed{client: &http.Client{Transport: benefitTransport(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return benefitResponse(tc.invalid), nil
				}
				return benefitResponse(tc.valid), nil
			})}}
			if _, _, err := f.read(t.Context(), tc.path); err == nil {
				t.Fatal("malformed response accepted")
			}
			if _, stale, err := f.read(t.Context(), tc.path); err != nil || stale || calls != 2 {
				t.Fatalf("bad response poisoned cache: calls=%d stale=%v err=%v", calls, stale, err)
			}
			// Invalid refreshes must preserve and label the last valid snapshot.
			old := f.cache[tc.path]
			old.at = time.Now().Add(-time.Hour)
			f.cache[tc.path] = old
			calls = 0
			entry, stale, err := f.read(t.Context(), tc.path)
			if err != nil || !stale || string(entry.body) != tc.valid || !entry.at.Equal(old.at) {
				t.Fatalf("valid snapshot lost: stale=%v err=%v", stale, err)
			}
		})
	}
}

func TestBenefitsEquivalentQueriesShareCacheKey(t *testing.T) {
	want, err := benefitSearchQuery(url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []string{"0", "00", "+0", "-0"} {
		got, err := benefitSearchQuery(url.Values{"offset": {offset}, "q": {""}, "modality": {""}, "status": {"all"}})
		if err != nil || got.Encode() != want.Encode() {
			t.Errorf("offset %q: got %v, want %v: %v", offset, got, want, err)
		}
	}
}
