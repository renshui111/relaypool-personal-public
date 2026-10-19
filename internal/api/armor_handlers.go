package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/relaypool/relaypool/internal/armor"
)

// handleListArmorPresets reports the loaded system-prompt presets.
//
// The full body is NOT returned by default: a preset can be long, and the
// console only needs names and descriptions to offer a choice. A caller that
// wants the text asks for a specific preset, which makes an accidental dump of
// every preset into a log or a screenshot less likely.
func (s *Server) handleListArmorPresets(w http.ResponseWriter, r *http.Request) {
	s.armorMu.Lock()
	defer s.armorMu.Unlock()
	if s.armor == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"default": "",
			"data":    []any{},
		})
		return
	}

	presets := s.armor.List()
	out := make([]map[string]any, 0, len(presets))
	for _, preset := range presets {
		out = append(out, map[string]any{
			"name":            preset.Name,
			"description":     preset.Description,
			"file":            preset.File,
			"bytes":           len(preset.Body),
			"family":          preset.Family,
			"auto_selectable": preset.IsAutoSelectable(),
			"priority":        preset.Priority,
			"source_url":      preset.SourceURL,
			"source_sha256":   preset.SourceSHA256,
			"license":         preset.License,
			"imported":        s.armorCustom.Presets[preset.Name].Name != "",
			"coverage":        preset.Coverage,
			"selection_note":  preset.SelectionNote,
			"body_sha256":     armor.TextSHA256(preset.Body),
			"model_prefixes":  preset.ModelPrefixes,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":             len(presets) > 0,
		"default":             s.armor.Default(),
		"families":            s.armor.Families(),
		"allow_per_request":   s.cfg.Armor.AllowPerRequest,
		"data":                out,
		"model_bindings":      s.armorCustom.Bindings,
		"configuration_error": s.armorConfigError,
		"primary_source":      armor.PrimarySource(),
		"coverage_matrix":     s.armor.Coverage(),
	})
}

// handleArmorResolve reports which preset would be chosen for a model.
//
// It exists so an operator can verify the selection before relying on it: the
// mapping from model name to family to preset is otherwise invisible, and a
// mis-detected family would only surface as a subtly wrong answer.
func (s *Server) handleArmorResolve(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "the model query parameter is required")
		return
	}
	// The decision mirrors the request path exactly: same model-based selection,
	// same enable rule. It is computed with Enabled=true so the answer shows
	// what WOULD be sent if the caller turned the armor on.
	native, channel := s.modelRoute(model)
	if id := strings.TrimSpace(r.URL.Query().Get("account_id")); id != "" {
		item, ok := s.pool.ByID(id)
		if !keyFromRequest(r).Permissions.AllowsAccount(id) {
			writeError(w, 403, "key_permission_denied", "当前密钥未获准调用此账号")
			return
		}
		if !ok {
			writeError(w, 404, "account_not_found", "账号不存在，请重新选择")
			return
		}
		if channel != "" && channel != item.Provider {
			writeError(w, 400, "model_provider_mismatch", "模型渠道与所选账号不一致")
			return
		}
		channel = item.Provider
	}
	if _, ok := s.permittedRouteAccounts(w, r, native, channel); !ok {
		return
	}
	resolvedModel := armorModelForChannel(native, channel)
	decision := s.armor.Resolve(armor.ApplyOptions{
		Enabled: true,
		Model:   resolvedModel,
	})
	if s.armorConfigError != "" {
		decision = armor.Decision{Reason: s.armorConfigError}
	}
	active := s.armorDecision(r, resolvedModel)
	status, explanation := armorStatus(active)

	response := map[string]any{
		"model":             model,
		"family":            string(armor.ModelFamily(resolvedModel)),
		"applied":           decision.Applied,
		"reason":            decision.Reason,
		"resolved_model":    resolvedModel,
		"key_armor_enabled": keyFromRequest(r).Permissions.ArmorEnabled,
		"key_armor_allowed": keyFromRequest(r).Permissions.ArmorAllowed,
		"effective":         active.Applied,
		"active_status":     status,
		"active_reason":     explanation,
		"efficacy_verified": false,
	}
	if decision.Applied {
		response["preset"] = decision.Preset.Name
		response["source"] = decision.Source
		response["bytes"] = len(decision.Preset.Body)
		response["selection_kind"] = armorSelectionKind(decision)
		response["body_sha256"] = armor.TextSHA256(decision.Preset.Body)
	}
	writeJSON(w, http.StatusOK, response)
}

// handleGetArmorPreset returns one preset, including its body.
func (s *Server) handleGetArmorPreset(w http.ResponseWriter, r *http.Request) {
	if s.armor == nil {
		writeError(w, http.StatusNotFound, "not_found", "armor is not configured")
		return
	}
	name := r.PathValue("name")
	preset, err := s.armor.Lookup(name)
	if err != nil {
		if errors.Is(err, armor.ErrUnknownPreset) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":          preset.Name,
		"description":   preset.Description,
		"file":          preset.File,
		"body":          preset.Body,
		"source_url":    preset.SourceURL,
		"source_sha256": preset.SourceSHA256,
		"license":       preset.License,
		"license_text":  preset.LicenseText,
		"body_sha256":   armor.TextSHA256(preset.Body),
	})
}

// handleAccessInfo reports the current exposure configuration.
//
// It exists so a client can discover whether it is talking to a loopback, LAN or
// public instance without guessing, which matters because the appropriate
// caution differs per level.
func (s *Server) handleAccessInfo(w http.ResponseWriter, r *http.Request) {
	mode := s.cfg.Access.Mode
	if mode == "" {
		mode = "loopback"
	}
	info := map[string]any{
		"mode": mode,
		// The direct peer address is reported rather than X-Forwarded-For,
		// because no proxy is trusted by default.
		"peer": clientIP(r),
	}
	writeJSON(w, http.StatusOK, info)
}
