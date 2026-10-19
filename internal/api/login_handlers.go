package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/workbuddy"
	"github.com/relaypool/relaypool/internal/store"
)

// Adding an account by signing in through a browser.
//
// WHY THIS EXISTS: the only other way to add a WorkBuddy account is to paste an
// access token, which means opening developer tools and copying a secret by
// hand. That is error-prone, and the token ends up in shell history, chat logs
// and screenshots. Driving the upstream's own plugin login keeps the secret
// inside this process and produces a credential with a refresh token, so the
// account keeps working instead of expiring in an hour.
//
// The flow, as the console drives it:
//
//	POST /admin/accounts/login/start    -> {login_id, auth_url}
//	(the operator opens auth_url and signs in)
//	GET  /admin/accounts/login/{id}     -> {status: "pending"|"done"|"expired"}
//	(when done, the account exists and is routable)
//	POST /admin/accounts/login/{id}/cancel  -> abandon

// loginStatus values reported to the console.
const (
	loginPending = "pending"
	loginDone    = "done"
	loginExpired = "expired"
	loginFailed  = "failed"
)

// pendingLogin is one browser-login round held in memory.
//
// It is not persisted: a half-finished sign-in has no meaning across a restart,
// and the operator can simply start again. Keeping it in memory also means a
// state token never reaches the database.
type pendingLogin struct {
	id       string
	account  store.Account
	provider string
	global   bool
	started  time.Time
	// lastErr records why the last poll failed, so the console can show a
	// reason instead of an indefinite spinner.
	lastErr string
	flow    *browserAttempt
}

// startLoginRequest is the body of POST /admin/accounts/login/start.
type startLoginRequest struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Region   string `json:"region"`
	provider.BrowserLoginOptions
}

// handleStartAccountLogin begins a browser login and returns the URL to open.
func (s *Server) handleStartAccountLogin(w http.ResponseWriter, r *http.Request) {
	if !s.loginStartMu.TryLock() {
		writeError(w, 409, "login_start_busy", "另一个授权正在初始化，请稍后重试")
		return
	}
	defer s.loginStartMu.Unlock()
	s.loginMu.Lock()
	full := len(s.logins) >= 128
	s.loginMu.Unlock()
	if full {
		writeError(w, 429, "too_many_logins", "正在进行的授权过多，请先取消或等待过期")
		return
	}
	var req startLoginRequest
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}

	providerName := strings.ToLower(strings.TrimSpace(req.Provider))
	if providerName == "" {
		providerName = workbuddy.Family
	}
	if providerName != workbuddy.Family {
		s.startProviderLogin(w, r, req, providerName)
		return
	}

	region := strings.ToLower(strings.TrimSpace(req.Region))
	if region == "" {
		region = "cn"
	}
	if region != "cn" && region != "global" {
		writeError(w, http.StatusBadRequest, "invalid_request", "region must be cn or global")
		return
	}

	client := s.workbuddyLoginClient()
	if client == nil {
		writeError(w, http.StatusServiceUnavailable, "provider_unavailable",
			"the workbuddy adapter is not configured, so a browser login cannot be started")
		return
	}

	global := region == "global"

	// The account row is created FIRST, because the upstream state is keyed by
	// an account id and the credential must be attachable when the poll
	// succeeds. It starts disabled so a half-created account can never receive
	// traffic before its credential exists.
	account, err := s.store.CreateAccount(r.Context(), store.Account{
		Name:             defaultLoginName(req.Name, region),
		Provider:         providerName,
		Region:           region,
		Enabled:          false,
		DropSystemPrompt: true,
	})
	if err != nil {
		log.Printf("admin: start login: create account: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create the account row")
		return
	}

	authURL, _, err := client.StartLogin(r.Context(), account.ID, global)
	if err != nil {
		// The account row exists but has no credential, so it is removed rather
		// than left as a permanently broken entry the operator has to clean up.
		if delErr := s.store.DeleteAccount(r.Context(), account.ID); delErr != nil {
			log.Printf("admin: start login: rollback account %s: %v", account.ID, delErr)
		}
		writeError(w, http.StatusBadGateway, "upstream_error",
			"the upstream refused to start a login: "+err.Error())
		return
	}

	s.beginLogin(account, providerName, global)

	writeJSON(w, http.StatusCreated, map[string]any{
		"login_id":              account.ID,
		"auth_url":              authURL,
		"region":                region,
		"expires_in_seconds":    int(workbuddy.LoginTimeout / time.Second),
		"poll_interval_seconds": int(workbuddy.LoginPollInterval / time.Second),
	})
}

// handleAccountLoginStatus polls one login in progress.
func (s *Server) handleAccountLoginStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pending, ok := s.getLogin(id)
	if !ok {
		// Claude 的回调会先保存账号；随后到达的轮询仍能获知完成结果。
		if s.replyCompletedClaudeLogin(w, r.Context(), id) {
			return
		}
		writeError(w, http.StatusNotFound, "not_found",
			"no login is in progress for that id; it may have expired or been cancelled")
		return
	}
	if pending.flow != nil {
		s.pollProviderLogin(w, r, pending)
		return
	}
	if time.Since(pending.started) > workbuddy.LoginTimeout {
		// Discard the whole attempt, not just the map entry: this branch drops
		// the login from the map, and the sweeper's only source of candidates is
		// that map, so leaving the row here orphans it permanently. The console
		// makes this path likely rather than exotic -- on "expired" it stops
		// polling and does NOT call cancel.
		s.discardLogin(r.Context(), id)
		writeJSON(w, http.StatusOK, map[string]any{"status": loginExpired})
		return
	}

	client := s.workbuddyLoginClient()
	if client == nil {
		writeError(w, http.StatusServiceUnavailable, "provider_unavailable", "the workbuddy adapter is not configured")
		return
	}

	cred, done, err := client.PollLogin(r.Context(), id, pending.global)
	if err != nil {
		// A transient upstream error must not lose the login: record it and
		// keep the poll alive, because the user is still in their browser.
		s.setLoginError(id, err.Error())
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     loginPending,
			"message":    "waiting for authorization",
			"last_error": err.Error(),
		})
		return
	}
	if !done {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  loginPending,
			"message": "waiting for authorization",
		})
		return
	}

	// A credential arrived. Store it, then enable the account: enabling last is
	// what makes "the account exists and routes" a single observable event, so a
	// request can never be routed to an account with no credential.
	if err := s.storeBrowserCredential(r, pending, cred); err != nil {
		s.setLoginError(id, err.Error())
		log.Printf("admin: login %s: store credential: %v", id, err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"the login succeeded but the credential could not be stored: "+err.Error())
		return
	}

	if err := s.store.SetAccountEnabled(r.Context(), id, true); err != nil {
		log.Printf("admin: login %s: enable: %v", id, err)
	}
	account, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "the account could not be read back")
		return
	}
	s.pool.Upsert(s.accountPoolItem(account))
	s.reloadAccounts()

	s.finishLogin(id, true)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  loginDone,
		"account": accountView(account),
	})
}

// handleCancelAccountLogin abandons a login in progress.
func (s *Server) handleCancelAccountLogin(w http.ResponseWriter, r *http.Request) {
	// discardLogin removes the row ONLY if this call actually owned the login.
	//
	// Deleting unconditionally was a real defect: a cancel arriving just after a
	// successful poll would destroy the freshly created account and its
	// credential. The operator pressing "cancel" a moment too late must not lose
	// the account they just signed into.
	if s.discardLogin(r.Context(), r.PathValue("id")) {
		s.reloadAccounts()
	}
	w.WriteHeader(http.StatusNoContent)
}

// storeBrowserCredential writes the credential the login produced.
func (s *Server) storeBrowserCredential(r *http.Request, pending pendingLogin, cred workbuddy.Credential) error {
	payload, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	return s.store.UpsertCredential(r.Context(), store.Credential{
		AccountID: pending.account.ID,
		Format:    workbuddy.CredentialFormat,
		Payload:   payload,
	})
}

// workbuddyLoginClient returns the workbuddy adapter, or nil when it is not
// registered. It is looked up by name so this file does not need to know how
// the registry is built.
func (s *Server) workbuddyLoginClient() *workbuddy.Client {
	if s.registry == nil {
		return nil
	}
	adapter, err := s.registry.Lookup(workbuddy.Family)
	if err != nil {
		return nil
	}
	client, ok := adapter.(*workbuddy.Client)
	if !ok {
		return nil
	}
	return client
}

// beginLogin records a login in progress.
//
// It refuses to start a second login for an account that already has one, so a
// double-click or a second browser tab replaces the pending attempt instead of
// leaving two states alive for one account.
func (s *Server) beginLogin(account store.Account, providerName string, global bool) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.logins == nil {
		s.logins = map[string]pendingLogin{}
	}
	s.logins[account.ID] = pendingLogin{
		id:       account.ID,
		account:  account,
		provider: providerName,
		global:   global,
		started:  time.Now(),
	}
}

// loginSweepInterval is how often abandoned logins are reaped. It is much
// shorter than the login timeout so an abandoned attempt is cleaned up within a
// couple of minutes of expiring.
const loginSweepInterval = 2 * time.Minute

// reapLogins removes logins that expired without anyone polling them, and
// deletes the account rows they created.
//
// WHY THIS EXISTS: expiry was originally checked only INSIDE the poll handler,
// so a login was reaped only if a client kept polling. An operator who opened
// the dialog and closed the tab -- the single most likely way to abandon a login
// -- left an entry in the map forever, plus a disabled account row that no
// longer had any way to be completed. Both grew without bound, and each
// abandoned row showed up in the console as a broken account.
//
// Deleting the row is the part that matters: the map entry is a few bytes, but
// the account row is user-visible.
func (s *Server) reapLogins(ctx context.Context) {
	type stale struct {
		id string
	}
	var expired []stale
	removed := 0

	s.loginMu.Lock()
	for id, pending := range s.logins {
		if time.Since(pending.started) > workbuddy.LoginTimeout && (pending.flow == nil || time.Now().After(pending.flow.session.ExpiresAt)) {
			expired = append(expired, stale{id: id})
		}
	}
	s.loginMu.Unlock()

	for _, item := range expired {
		// discardLogin re-checks ownership: if a poll completed this login after
		// the snapshot above, the row belongs to a working account and must NOT
		// be deleted.
		log.Printf("admin: login %s expired without completing; discarding it", item.id)
		if s.discardLogin(ctx, item.id) {
			removed++
		}
	}

	// Reload ONCE for the whole sweep, not once per discarded account. Each
	// reload is a full account listing, so a sweep that cleaned up a hundred
	// abandoned logins previously did a hundred sequential full scans.
	if removed > 0 {
		s.reloadAccounts()
	}
}

// startLoginSweeper runs reapLogins on a ticker until the context is cancelled.
func (s *Server) startLoginSweeper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(loginSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.reapLogins(ctx)
			}
		}
	}()
}

func (s *Server) getLogin(id string) (pendingLogin, bool) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	pending, ok := s.logins[id]
	return pending, ok
}

func (s *Server) setLoginError(id, message string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	pending, ok := s.logins[id]
	if !ok {
		return
	}
	pending.lastErr = message
	s.logins[id] = pending
}

// finishLogin drops a login, and cancels it upstream when the attempt failed.
//
// It reports whether THIS call actually removed the entry.
//
// WHY THE RESULT MATTERS: the sweeper snapshots candidate ids, releases the
// lock, and then acts. Between those steps a poll can complete the login, so by
// the time the sweeper acts the entry may belong to a SUCCESSFUL login. Callers
// that delete the account row must therefore act only when they won the removal,
// or they destroy a working account -- and its credential, which cascades.
func (s *Server) finishLogin(id string, succeeded bool) bool {
	s.loginMu.Lock()
	pending, ok := s.logins[id]
	delete(s.logins, id)
	s.loginMu.Unlock()
	if !ok {
		return false
	}
	if !succeeded && pending.provider == workbuddy.Family {
		if client := s.workbuddyLoginClient(); client != nil {
			client.CancelLogin(id)
		}
	}
	return true
}

// discardLogin removes an abandoned login AND the account row it created.
//
// It is the single place that deletes an incomplete login's account, so the
// decision "is this row still abandoned?" is made once and consistently.
//
// TWO guards. The second is the one that prevents data loss; the first is a
// cheap early-out that overlaps it. The overlap was verified by mutation:
// removing guard 1 alone does NOT make any test fail, because guard 2 covers the
// same inputs. Both are kept because guard 1 also avoids clearing a local login
// state that a just-succeeded login may still be using.
//
// A note on wording, because a previous comment and commit message got this
// wrong: "cancel" here is LOCAL ONLY. CancelLogin drops the in-memory state; it
// makes no upstream call, because the upstream exposes no revocation endpoint.
//
//  1. It must have won the removal of the map entry. Otherwise another caller
//     already decided this login's outcome.
//
//  2. The account must still look incomplete: no credential, and disabled.
//     THIS is the guard that matters. A successful poll stores the credential
//     and enables the account BEFORE it removes the map entry, so there is a real
//     window in which the entry is still present and the account is already live.
//     Guard 1 alone would let the sweeper win that window and delete a working
//     account -- and because credentials cascade on account delete, its
//     credential would vanish with it. Checking the row itself makes the outcome
//     depend on the account's state rather than on who reached the map first.
//
// It reports whether it actually removed the account, so a caller cleaning up
// many logins can reload the pool once at the end instead of after each one.
func (s *Server) discardLogin(ctx context.Context, id string) bool {
	if pending, ok := s.getLogin(id); ok && pending.flow != nil {
		pending.flow.mu.Lock()
		defer pending.flow.mu.Unlock()
	}
	if !s.finishLogin(id, false) {
		return false
	}
	return s.discardIncompleteAccount(ctx, id)
}

func (s *Server) discardIncompleteAccount(ctx context.Context, id string) bool {

	// Guard 2: never delete an account that has a credential or is enabled.
	account, err := s.store.GetAccount(ctx, id)
	if err != nil {
		// Already gone, so there is nothing to clean up.
		return false
	}
	if account.Enabled {
		log.Printf("admin: abandoned login %s: account is enabled, leaving it alone", id)
		return false
	}
	if _, credErr := s.store.GetCredential(ctx, id); credErr == nil {
		log.Printf("admin: abandoned login %s: a credential exists, so the login "+
			"succeeded; leaving the account alone", id)
		return false
	}

	if err := s.store.DeleteAccount(ctx, id); err != nil {
		log.Printf("admin: abandoned login %s: delete the account row: %v", id, err)
		return false
	}
	s.pool.Remove(id)
	return true
}

// defaultLoginName names an account created by a browser login, so the console
// shows something recognisable before the operator renames it.
func defaultLoginName(requested, region string) string {
	if name := strings.TrimSpace(requested); name != "" {
		return name
	}
	stamp := time.Now().Format("01-02 15:04")
	return "browser login " + region + " " + stamp
}
