package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/relaypool/relaypool/internal/armor"
	"github.com/relaypool/relaypool/internal/provider"
)

// armorDecision 按已鉴权密钥决定是否注入；客户端请求头只能关闭，不能扩大权限。
func (s *Server) armorDecision(r *http.Request, model string) armor.Decision {
	if s.armorConfigError != "" {
		return armor.Decision{Reason: s.armorConfigError}
	}
	if s.armor == nil {
		return armor.Decision{Reason: "armor is not configured"}
	}

	// 许可和实际开关都必须开启，请求参数只能缩小授权。
	if !keyFromRequest(r).Permissions.ArmorAllowed {
		return armor.Decision{Reason: "armor is not allowed for this key"}
	}
	if !keyFromRequest(r).Permissions.ArmorEnabled {
		return armor.Decision{Reason: "armor is disabled for this key"}
	}

	enableHeader := r.Header.Get(armor.HeaderEnable)
	presetName := strings.TrimSpace(r.Header.Get(armor.HeaderPreset))

	// A caller may turn armor OFF for one request with an explicit disable, but
	// never on: the header is a narrowing control, not an enabling one.
	if armor.IsExplicitDisable(enableHeader) {
		return armor.Decision{Reason: "disabled for this request by header"}
	}

	// Naming a preset is only honoured when the operator permits per-request
	// selection, and only this key's permission can enable it.
	if presetName != "" && !s.cfg.Armor.AllowPerRequest {
		return armor.Decision{Reason: "per-request preset selection is not allowed"}
	}

	return s.armor.Resolve(armor.ApplyOptions{
		Enabled:    true,
		PresetName: presetName,
		Model:      model,
	})
}

// applyArmor substitutes the system prompt when the decision says to.
//
// The transformation is deliberately narrow: every existing system message is
// removed and exactly one carrying the preset is placed first. Doing anything
// cleverer (appending, merging) would leave the caller's instructions in play
// alongside the preset, which defeats the point of a substitution and makes the
// resulting behaviour unpredictable.
//
// It reports whether a substitution happened so the caller can advertise it in
// a response header.
func applyArmor(req *provider.ChatRequest, decision armor.Decision) bool {
	if req == nil || !decision.Applied {
		return false
	}

	// system 与 developer 都是上层指令，替换时一并清除，保留对话和工具轮次顺序。
	kept := make([]provider.Message, 0, len(req.Messages)+1)
	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role == "system" || role == "developer" {
			continue
		}
		kept = append(kept, msg)
	}

	content, err := json.Marshal(decision.Preset.Body)
	if err != nil {
		return false
	}
	// The preset goes FIRST, because both the upstream's requirement (messages[0]
	// must be a system message) and the model's attention both favour a leading
	// instruction.
	req.Messages = append([]provider.Message{{
		Role:    "system",
		Content: content,
	}}, kept...)

	// 只清理提示相关原始字段，保留格式约束、输出上限等其他请求选项。
	// 适配器再从上面的中立消息重建 messages，避免旧提示重新出现。
	var remaining map[string]json.RawMessage
	if json.Unmarshal(req.Raw, &remaining) == nil {
		for _, field := range []string{"messages", "system", "instructions", "input"} {
			delete(remaining, field)
		}
		req.Raw, _ = json.Marshal(remaining)
	} else {
		req.Raw = nil
	}
	return true
}

// armorHeaderValue renders the response header value.
//
// An empty string means "no substitution", which is reported explicitly rather
// than by omitting the header: a caller that asked for the armor needs to be
// able to tell "not applied" from "the header was dropped by a proxy".
func armorHeaderValue(decision armor.Decision) string {
	if decision.Applied {
		return decision.Preset.Name
	}
	return "none"
}
