package api

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/relaypool/relaypool/internal/store"
)

// A credential supplied in the create request must be stored against the ID the
// store actually assigned.
//
// REGRESSION: the store's CreateAccount took its argument by value, so the
// generated ID never reached the caller. The API layer then re-read "the newest
// row for this provider and region" to learn the ID, and the credential was
// written against an empty account ID in the meantime. The credential would
// silently not exist, and the account would fail every request with a confusing
// "credential not found".
func TestCreateAccountStoresCredentialAgainstTheRealID(t *testing.T) {
	adapter := newStubProvider()
	srv, server := newTestServer(t, adapter)

	body := `{
		"name":"with-credential",
		"provider":"workbuddy",
		"region":"cn",
		"credential":"{\"access_token\":\"at_inline\",\"uid\":\"u_inline\",\"domain\":\"codebuddy.cn\"}"
	}`
	req := authRequest(t, http.MethodPost, srv.URL+"/admin/accounts", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("the response carried no account id")
	}

	// The credential must be resolvable under that ID.
	cred, err := server.store.GetCredential(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("no credential stored for %s: %v; the inline credential was "+
			"written against the wrong (likely empty) id", created.ID, err)
	}
	if len(cred.Payload) == 0 {
		t.Errorf("the stored credential is empty")
	}

	// And nothing may be filed under an empty account id.
	if _, err := server.store.GetCredential(t.Context(), ""); err == nil {
		t.Errorf("a credential was stored against an empty account id")
	}
}

// An account whose name is omitted must still be creatable: the id becomes the
// name, which is the documented fallback.
func TestCreateAccountWithoutNameUsesTheID(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)

	req := authRequest(t, http.MethodPost, srv.URL+"/admin/accounts", `{"provider":"workbuddy","region":"cn"}`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	if created.Name == "" {
		t.Errorf("an unnamed account should take its id as the name")
	}
	if created.Name != created.ID {
		t.Errorf("name=%q id=%q; the name should default to the id", created.Name, created.ID)
	}
}

// Concurrent creates must each get their OWN id.
//
// REGRESSION: the previous "read the newest row" lookup could hand two
// concurrent creators the same account, so one of them would attach a credential
// to the other's account.
func TestConcurrentCreatesGetDistinctAccounts(t *testing.T) {
	adapter := newStubProvider()
	srv, server := newTestServer(t, adapter)

	const workers = 8
	ids := make([]string, workers)
	var wg sync.WaitGroup
	errs := make([]error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Every worker asks for the SAME provider and region, which is what
			// made the old lookup ambiguous.
			req := authRequest(t, http.MethodPost, srv.URL+"/admin/accounts",
				`{"name":"concurrent","provider":"workbuddy","region":"cn"}`)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs[idx] = err
				return
			}
			defer resp.Body.Close()
			var created struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&created)
			ids[idx] = created.ID
		}(i)
	}
	wg.Wait()

	seen := map[string]int{}
	for i, id := range ids {
		if errs[i] != nil {
			t.Fatalf("worker %d failed: %v", i, errs[i])
		}
		if id == "" {
			t.Fatalf("worker %d got no id", i)
		}
		seen[id]++
	}
	if len(seen) != workers {
		t.Errorf("got %d distinct ids across %d concurrent creates; a shared id "+
			"means credentials would land on the wrong account", len(seen), workers)
	}

	// Every created account must actually exist.
	accounts, err := server.store.ListAccounts(t.Context())
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	stored := map[string]bool{}
	for _, a := range accounts {
		stored[a.ID] = true
	}
	for id := range seen {
		if !stored[id] {
			t.Errorf("account %s was reported created but is not stored", id)
		}
	}
}

// Store defaults must be applied AND reported, so the console shows the same
// values the scheduler will use.
func TestCreateAccountReportsAppliedDefaults(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)

	// Deliberately omit every optional field.
	req := authRequest(t, http.MethodPost, srv.URL+"/admin/accounts", `{"provider":"workbuddy","region":"cn"}`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	var created map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if w, _ := created["weight"].(float64); w != 50 {
		t.Errorf("weight=%v, want the store default 50", created["weight"])
	}
	if m, _ := created["max_inflight"].(float64); m != 0 {
		t.Errorf("max_inflight=%v，省略配置应保存为零并继承全局值", created["max_inflight"])
	}
	if m, _ := created["effective_max_inflight"].(float64); m != 4 {
		t.Errorf("实际并发上限=%v，期望默认值 4", created["effective_max_inflight"])
	}
}

// Deleting an account must remove it from the POOL, not only from the store, or
// the scheduler keeps routing to a deleted account.
func TestDeleteRemovesFromPool(t *testing.T) {
	adapter := newStubProvider()
	srv, server := newTestServer(t, adapter)

	req := authRequest(t, http.MethodPost, srv.URL+"/admin/accounts", `{"name":"doomed","provider":"workbuddy","region":"cn"}`)
	resp, _ := http.DefaultClient.Do(req)
	var created struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	if _, ok := server.pool.ByID(created.ID); !ok {
		t.Fatalf("the created account is not in the pool")
	}

	delReq := authRequest(t, http.MethodDelete, srv.URL+"/admin/accounts/"+created.ID, "")
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	delResp.Body.Close()

	if _, ok := server.pool.ByID(created.ID); ok {
		t.Errorf("the account is still routable after deletion")
	}
	if _, err := server.store.GetAccount(t.Context(), created.ID); err == nil {
		t.Errorf("the account is still in the store after deletion")
	}
}

// Disabling an account must take effect on the pool immediately, so a disabled
// account stops serving without a restart.
func TestDisableTakesEffectImmediately(t *testing.T) {
	adapter := newStubProvider()
	srv, server := newTestServer(t, adapter)
	addAccount(t, server, "acc_disable", adapter.family)

	body := `{"enabled":false}`
	req := authRequest(t, http.MethodPost, srv.URL+"/admin/accounts/acc_disable/enabled", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	item, ok := server.pool.ByID("acc_disable")
	if !ok {
		t.Fatalf("the account vanished from the pool")
	}
	if item.Enabled {
		t.Errorf("the pool still reports the account as enabled")
	}

	// And the pool must report it disabled, which is what the scheduler reads.
	item, ok = server.pool.ByID("acc_disable")
	if !ok || item.Enabled {
		t.Errorf("the pool still reports the account as enabled")
	}
}

// A delete of an unknown account must be a clean 404, not a 500.
func TestDeleteUnknownAccountIs404(t *testing.T) {
	adapter := newStubProvider()
	srv, _ := newTestServer(t, adapter)

	req := authRequest(t, http.MethodDelete, srv.URL+"/admin/accounts/acc_does_not_exist", "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404", resp.StatusCode)
	}
}

// The store must reject a duplicate account id rather than silently overwriting.
func TestDuplicateAccountIDIsRejected(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()

	if _, err := st.CreateAccount(ctx, store.Account{ID: "acc_dup", Provider: "workbuddy", Region: "cn"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := st.CreateAccount(ctx, store.Account{ID: "acc_dup", Provider: "workbuddy", Region: "cn"}); err == nil {
		t.Errorf("a duplicate account id must be rejected, not silently overwritten")
	}
}
