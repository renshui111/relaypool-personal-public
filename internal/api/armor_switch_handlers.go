package api

import (
	"net/http"
	"sort"
)

// keyArmorStateView 展示当前密钥的破甲状态及已加载的预设。
type keyArmorStateView struct {
	// Allowed 为使用许可，Enabled 为该密钥当前开关。
	Allowed bool `json:"allowed"`
	Enabled bool `json:"enabled"`
	// Presets is how many presets loaded. Zero means armor cannot do anything,
	// which the console surfaces rather than showing a switch that does nothing.
	Presets int `json:"presets"`
	// Families lists the model families that have a preset, so an operator can
	// see which models will be affected before turning the switch on.
	Families []string `json:"families"`
	// Default is the registry default preset name, shown for reference.
	Default string `json:"default,omitempty"`
}

// keyArmorStateView assembles the current switch state.
func (s *Server) keyArmorState(r *http.Request) keyArmorStateView {
	p := keyFromRequest(r).Permissions
	state := keyArmorStateView{Allowed: p.ArmorAllowed, Enabled: p.ArmorAllowed && p.ArmorEnabled}
	if s.armor == nil {
		state.Families = []string{}
		return state
	}

	presets := s.armor.List()
	state.Presets = len(presets)
	state.Default = s.armor.Default()

	// Only families that will actually be auto-selected are listed: showing a
	// family whose presets are all opt-out would promise armor for a model that
	// will never receive any.
	seen := map[string]bool{}
	for _, p := range presets {
		if !p.IsAutoSelectable() || p.Family == "" {
			continue
		}
		name := string(p.Family)
		if !seen[name] {
			seen[name] = true
			state.Families = append(state.Families, name)
		}
	}
	sort.Strings(state.Families)
	if state.Families == nil {
		state.Families = []string{}
	}
	return state
}
