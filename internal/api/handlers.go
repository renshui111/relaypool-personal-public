package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/store"
)

// handleChatCompletions, handleMessages and handleResponses live in
// protocol.go: each one only parses its wire shape and delegates to the
// transport, which owns routing and rendering for all three protocols.

// handleModels reports the models the pool can currently serve.
//
// The catalogue is assembled from what accounts actually advertise rather than
// from a static list, so a model appears only when some account can serve it.
// An account whose catalogue has not been fetched yet contributes nothing:
// advertising a model no account can serve would just produce a failure on the
// first request that used it.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	data := s.modelCatalog(keyFromRequest(r).Permissions)
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// 目录只展示当前密钥可用账号提供的已授权型号。
func (s *Server) modelCatalog(p store.KeyPermissions) []map[string]any {
	data := []map[string]any{}
	seen := map[string]bool{}
	for _, item := range s.pool.Items() {
		if !item.Enabled || !p.AllowsAccount(item.ID) {
			continue
		}
		for _, model := range item.Models {
			if model == "" || !s.keyAllowsModel(p, model, item.Provider) {
				continue
			}
			for _, id := range []string{model, item.Provider + "/" + model} {
				if seen[id] {
					continue
				}
				seen[id] = true
				data = append(data, map[string]any{"id": id, "object": "model", "owned_by": item.Provider})
			}
		}
		// 限定型号时不暴露可能自动选择其他模型的区域别名。
		if p.AllModels {
			id := item.Provider + "/" + item.Region
			if !seen[id] {
				seen[id] = true
				data = append(data, map[string]any{"id": id, "object": "model", "owned_by": item.Provider})
			}
		}
	}
	return data
}

// ---- admin: accounts ----

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		log.Printf("admin: list accounts: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list accounts")
		return
	}
	// The POOL is the live source for runtime health. The database copy is
	// written asynchronously to keep writes off the request path, so reading it
	// here would show a stale cooldown and last_error for a few seconds after
	// they changed. Merging the live values makes the console reflect reality
	// immediately.
	out := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		view := accountView(account)
		view["quota_supported"] = s.quotaSupported(account.Provider)
		if item, ok := s.pool.ByID(account.ID); ok {
			applyLiveRuntime(view, item)
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// applyLiveRuntime overlays the pool's live runtime state onto an account view.
//
// Only runtime fields are overlaid. Configuration (name, weight, enabled, armor
// preset) comes from the database, because that is what the operator last saved
// and the pool merely mirrors.
func applyLiveRuntime(view map[string]any, item pool.Item) {
	view["effective_max_inflight"] = item.MaxInflight
	view["last_error"] = item.LastError
	view["last_error_kind"] = item.LastKind
	view["backoff_level"] = item.BackoffLevel
	view["in_flight"] = item.InFlight
	if item.Ready != nil {
		view["ready"] = *item.Ready
	}
	// 认证失败只影响展示与既有冷却；不设置永久不可调度，保留自动刷新与重试机会。
	if item.LastKind == pool.KindAuth || item.LastKind == pool.KindNotReady {
		view["ready"] = false
	}

	now := time.Now()
	deadline := item.AccountDownUntil()
	if deadline.After(now) {
		view["cooldown_until"] = deadline
		view["status"] = "cooldown"
		view["retry_after_seconds"] = int(deadline.Sub(now).Seconds())
	} else if _, present := view["cooldown_until"]; present {
		// The cooldown expired, so the stored value must not be reported: a
		// stale deadline in the console is worse than no deadline.
		delete(view, "cooldown_until")
		view["status"] = "ready"
	}
	if !deadline.After(now) && item.Ready != nil && *item.Ready {
		view["status"] = "ready"
	}

	// Model-scoped cooldowns explain a single model failing while the account
	// is otherwise healthy, so they are surfaced explicitly.
	cooling := map[string]string{}
	for model, until := range item.ModelDownUntil {
		if until.After(now) {
			cooling[model] = until.UTC().Format(time.RFC3339)
		}
	}
	if len(cooling) > 0 {
		view["model_cooldowns"] = cooling
	}
	if len(item.Models) > 0 {
		view["models"] = item.Models
	}
}

type createAccountRequest struct {
	Name             string    `json:"name"`
	Provider         string    `json:"provider"`
	Region           string    `json:"region"`
	AuthType         string    `json:"auth_type"`
	Enabled          *bool     `json:"enabled"`
	MaxInflight      int       `json:"max_inflight"`
	QuotaScope       string    `json:"quota_scope"`
	QuotaGroup       string    `json:"quota_group"`
	QuotaExpiresAt   time.Time `json:"quota_expires_at"`
	Priority         int       `json:"priority"`
	Weight           int       `json:"weight"`
	DropSystem       *bool     `json:"drop_system_prompt"`
	AutoCheckin      *bool     `json:"auto_checkin"`
	Credential       string    `json:"credential"`
	CredentialFormat string    `json:"credential_format"`
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	var req createAccountRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	if req.MaxInflight < 0 || req.Weight < 0 || req.Weight > 1000000 || !validAccountQuotaScope(req.QuotaScope) || len(strings.TrimSpace(req.QuotaGroup)) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_request", "并发上限、权重或额度范围无效")
		return
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider == "" {
		provider = "workbuddy"
	}
	if _, err := s.registry.Lookup(provider); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "渠道尚未接入适配器")
		return
	}
	region := strings.ToLower(strings.TrimSpace(req.Region))
	if region == "" {
		region = "cn"
	}
	switch region {
	case "cn", "global":
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "region must be cn or global")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	dropSystem := provider == "workbuddy"
	if req.DropSystem != nil {
		dropSystem = *req.DropSystem
	}
	autoCheckin := false
	if req.AutoCheckin != nil {
		autoCheckin = *req.AutoCheckin
	}

	account := store.Account{
		Name:             strings.TrimSpace(req.Name),
		Provider:         provider,
		Region:           region,
		AuthType:         strings.TrimSpace(req.AuthType),
		Enabled:          enabled,
		MaxInflight:      req.MaxInflight,
		QuotaScope:       req.QuotaScope,
		QuotaGroup:       strings.TrimSpace(req.QuotaGroup),
		QuotaExpiresAt:   req.QuotaExpiresAt,
		Priority:         req.Priority,
		Weight:           req.Weight,
		DropSystemPrompt: dropSystem,
		AutoCheckin:      autoCheckin,
	}
	// 保存之前校验，避免导入失败却留下一个缺少凭证的启用账号。
	if strings.TrimSpace(req.Credential) != "" {
		adapter, _ := s.registry.Lookup(provider)
		credential, err := adapter.DecodeCredential([]byte(req.Credential))
		if err != nil || (credential.Region != "" && string(credential.Region) != region) {
			message := "凭证格式或地区不匹配"
			if err != nil && (provider == "qoder" || provider == "trae" || provider == "trae-intl") {
				message = err.Error()
			}
			writeError(w, http.StatusBadRequest, "invalid_credential", message)
			return
		}
		if err := validateAuthType(account.AuthType, credential); err != nil {
			writeError(w, 400, "invalid_credential", err.Error())
			return
		}
		account.AuthType = credentialAuthType(credential)
		account.CredentialExpiresAt = credential.ExpiresAt
	} else if account.AuthType != "" && account.AuthType != "api_key" && account.AuthType != "oauth" {
		writeError(w, 400, "invalid_request", "凭证类型须为 api_key 或 oauth")
		return
	}
	if account.AutoCheckin && !accountCheckinCapability(account).Supported {
		writeError(w, 400, "unsupported_checkin", accountCheckinCapability(account).Note)
		return
	}
	created, err := s.store.CreateAccount(r.Context(), account)
	if err != nil {
		log.Printf("admin: create account: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create account")
		return
	}

	// The credential is stored against the ID the store just assigned, which is
	// why CreateAccount returns the row: reading it back afterwards would be
	// racy under concurrent creates.
	if strings.TrimSpace(req.Credential) != "" {
		if err := s.attachCredential(r, created, req); err != nil {
			_ = s.store.DeleteAccount(r.Context(), created.ID)
			writeError(w, http.StatusInternalServerError, "internal_error", "保存凭证失败")
			return
		}
		created.HasCredential = true
	}

	// The store is only half the picture: the executor routes through the
	// in-memory pool, so an account that exists only in SQLite is invisible to
	// scheduling. Registering it here is what makes a newly added account
	// immediately usable instead of requiring a restart.
	s.pool.Upsert(s.accountPoolItem(created))

	view := accountView(created)
	if live, ok := s.pool.ByID(created.ID); ok {
		applyLiveRuntime(view, live)
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) attachCredential(r *http.Request, account store.Account, req createAccountRequest) error {
	adapter, err := s.registry.Lookup(account.Provider)
	if err != nil {
		return fmt.Errorf("渠道 %s 尚未接入适配器", account.Provider)
	}
	credential, err := adapter.DecodeCredential([]byte(req.Credential))
	if err != nil {
		return err
	}
	if !credential.Ready() {
		return fmt.Errorf("凭证信息不完整")
	}
	format := strings.TrimSpace(req.CredentialFormat)
	if format == "" {
		format = account.Provider + "-" + credentialAuthType(credential) + "-v1"
	}
	return s.store.UpsertCredential(r.Context(), store.Credential{
		AccountID: account.ID,
		Format:    format,
		Payload:   []byte(req.Credential),
		AuthType:  credentialAuthType(credential),
		ExpiresAt: credential.ExpiresAt,
	})
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	account, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to read account")
		return
	}
	view := accountView(account)
	// Overlay the live runtime state, for the same reason the list does.
	view["quota_supported"] = s.quotaSupported(account.Provider)
	if item, ok := s.pool.ByID(id); ok {
		applyLiveRuntime(view, item)
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	id := r.PathValue("id")
	if err := s.store.DeleteAccount(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to delete account")
		return
	}
	// Removing it from the pool is what actually stops traffic: the store row
	// alone would leave the scheduler still routing to a deleted account.
	s.pool.Remove(id)
	if s.stateWriter != nil {
		s.stateWriter.forget(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetAccountEnabled(w http.ResponseWriter, r *http.Request) {
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	id := r.PathValue("id")
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "enabled is required")
		return
	}
	if err := s.store.SetAccountEnabled(r.Context(), id, *req.Enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to update account")
		return
	}
	account, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to read account back")
		return
	}
	// Enabling or disabling only takes effect once the pool agrees, so the
	// in-memory entry is refreshed from the row we just wrote.
	s.pool.Upsert(s.accountPoolItem(account))
	writeJSON(w, http.StatusOK, accountView(account))
}

// handlePutCredential stores a credential for an account.
//
// The credential is stored verbatim after the adapter validates that it can be
// decoded. Storing an undecodable blob would produce an account that looks
// configured but fails every request, which is harder to diagnose than an
// outright rejection here.
func (s *Server) handlePutCredential(w http.ResponseWriter, r *http.Request) {
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	id := r.PathValue("id")
	account, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to read account")
		return
	}

	var req struct {
		Format     string          `json:"format"`
		Payload    json.RawMessage `json:"payload"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	// Accept the blob under either key so an operator can paste a raw export
	// without wrapping it.
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.Credential
	}
	if len(payload) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "payload is required")
		return
	}

	adapter, err := s.registry.Lookup(account.Provider)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"no adapter for provider "+account.Provider)
		return
	}
	// Validate by decoding. This is the adapter's own contract, so it accepts
	// every export shape the adapter supports rather than one gateway-imposed
	// shape.
	decoded, err := adapter.DecodeCredential(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_credential", err.Error())
		return
	}
	if decoded.Region != "" && string(decoded.Region) != account.Region {
		writeError(w, http.StatusBadRequest, "invalid_credential", "凭证地区与账号配置不一致")
		return
	}

	format := strings.TrimSpace(req.Format)
	if format == "" {
		format = account.Provider + "-" + credentialAuthType(decoded) + "-v1"
	}
	if err := s.store.UpsertCredential(r.Context(), store.Credential{
		AccountID: id,
		Format:    format,
		Payload:   payload,
		AuthType:  credentialAuthType(decoded),
		ExpiresAt: decoded.ExpiresAt,
	}); err != nil {
		log.Printf("admin: store credential for %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to store credential")
		return
	}
	account.AuthType = credentialAuthType(decoded)
	// 新凭证替换旧凭证后允许立即恢复；更新仍不代表上游已完成真实验收。
	s.pool.ResetCredentialHealth(id)
	s.pool.Upsert(s.accountPoolItem(account))
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": id,
		"format":     format,
		"stored":     true,
	})
}

// accountView is the JSON shape returned to the console.
//
// The credential blob is never included: only a boolean saying whether one
// exists, so the console can prompt for login without ever handling secrets.
//
// There is no armor field: the preset is chosen from the requested model, so
// there is nothing per-account to configure or display.
func accountView(a store.Account) map[string]any {
	view := map[string]any{
		"id":                    a.ID,
		"name":                  a.Name,
		"remote_uid":            a.RemoteUID,
		"provider":              a.Provider,
		"region":                a.Region,
		"auth_type":             a.AuthType,
		"has_credential":        a.HasCredential,
		"credential_expires_at": a.CredentialExpiresAt,
		"enabled":               a.Enabled,
		"max_inflight":          a.MaxInflight,
		"quota_scope":           a.QuotaScope,
		"quota_group":           a.QuotaGroup,
		"quota_expires_at":      a.QuotaExpiresAt,
		"priority":              a.Priority,
		"weight":                a.Weight,
		"status":                a.Status,
		"last_error":            a.LastError,
		"last_error_kind":       a.LastErrorKind,
		"drop_system_prompt":    a.DropSystemPrompt,
		"auto_checkin":          a.AutoCheckin,
		"checkin":               accountCheckinCapability(a),
		"last_checkin_status":   a.LastCheckinStatus,
		"last_checkin_msg":      a.LastCheckinMsg,
		"created_at":            a.CreatedAt,
		"updated_at":            a.UpdatedAt,
	}
	if !a.CooldownUntil.IsZero() {
		view["cooldown_until"] = a.CooldownUntil
	}
	if !a.LastCheckinAt.IsZero() {
		view["last_checkin_at"] = a.LastCheckinAt
	}
	return view
}

// ---- admin: keys ----

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListAPIKeys(r.Context())
	if err != nil {
		log.Printf("admin: list keys: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list keys")
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyView(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req keyWriteRequest
	if err := decodeKeyWrite(w, r, &req); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	name, scope := "", store.KeyScopeInference
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if req.Scope != nil {
		scope = *req.Scope
	}
	if len(name) > 200 || (scope != store.KeyScopeAdmin && scope != store.KeyScopeInference) {
		writeError(w, 400, "invalid_request", "名称过长或权限无效")
		return
	}
	p := store.DefaultKeyPermissions()
	var err error
	if req.Permissions != nil {
		p, err = s.validateKeyPermissions(r.Context(), *req.Permissions)
		if err != nil {
			writeError(w, 400, "invalid_request", err.Error())
			return
		}
	}
	key, k, err := s.store.CreateAPIKeyWithPermissions(r.Context(), name, scope, p)
	if err != nil {
		writeError(w, 500, "internal_error", "创建密钥失败")
		return
	}
	view := keyView(k)
	view["key"] = key
	view["warning"] = "完整密钥仅显示一次，请立即保存"
	writeJSON(w, 201, view)
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteAPIKey(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "key not found")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- admin: requests ----

func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	logs, err := s.store.ListRequests(r.Context(), store.RequestLogFilter{
		AccountID: q.Get("account_id"),
		Provider:  q.Get("provider"),
		Status:    q.Get("status"),
		Limit:     atoiDefault(q.Get("limit"), 100),
		Offset:    atoiDefault(q.Get("offset"), 0),
	})
	if err != nil {
		log.Printf("admin: list requests: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list requests")
		return
	}
	out := make([]map[string]any, 0, len(logs))
	for _, l := range logs {
		out = append(out, requestView(l))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) handleListAttempts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	attempts, err := s.store.ListAttempts(r.Context(), id)
	if err != nil {
		log.Printf("admin: list attempts: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list attempts")
		return
	}
	out := make([]map[string]any, 0, len(attempts))
	for _, a := range attempts {
		out = append(out, map[string]any{
			"id":            a.ID,
			"attempt_index": a.AttemptIndex,
			"account_id":    a.AccountID,
			"provider":      a.Provider,
			"status":        a.Status,
			"error_kind":    a.ErrorKind,
			"error_code":    a.ErrorCode,
			"error_message": a.ErrorMessage,
			"latency_ms":    a.LatencyMs,
			"started_at":    a.StartedAt,
			"finished_at":   a.FinishedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func requestView(l store.RequestLog) map[string]any {
	view := map[string]any{
		"id":              l.ID,
		"created_at":      l.CreatedAt,
		"status":          l.Status,
		"stream":          l.Stream,
		"protocol":        l.Protocol,
		"requested_model": l.RequestedModel,
		"mapped_model":    l.MappedModel,
		"account_id":      l.AccountID,
		"provider":        l.Provider,
		"routing":         l.Routing,
		"error_kind":      l.ErrorKind,
		"error_code":      l.ErrorCode,
		"error_message":   l.ErrorMessage,
		"attempt_count":   l.AttemptCount,
		"armor_preset":    l.ArmorPreset,
	}
	if !l.FinishedAt.IsZero() {
		view["finished_at"] = l.FinishedAt
	}
	if l.PromptTokens != nil {
		view["prompt_tokens"] = *l.PromptTokens
	}
	if l.CompletionTokens != nil {
		view["completion_tokens"] = *l.CompletionTokens
	}
	if l.TotalTokens != nil {
		view["total_tokens"] = *l.TotalTokens
	}
	if l.Credits != nil {
		view["credits"] = *l.Credits
	}
	if l.LatencyMs != nil {
		view["latency_ms"] = *l.LatencyMs
	}
	if l.TTFBMs != nil {
		view["ttfb_ms"] = *l.TTFBMs
	}
	return view
}

// atoiDefault parses an integer query parameter, falling back when the value
// is absent or malformed. A bad filter value degrades to the default rather
// than failing the whole listing.
func atoiDefault(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}
