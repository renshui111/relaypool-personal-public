package api

import (
	"net/http"
	"strings"

	"github.com/relaypool/relaypool/internal/armor"
)

// 预览与实际请求共用型号解析，避免别名和固定账号的渠道信息丢失。
func (s *Server) modelRoute(model string) (string, string) {
	native, channel := resolveModel(model)
	if channel == "" {
		if prefix, value, ok := strings.Cut(strings.TrimSpace(model), "/"); ok && s.registry.Has(strings.ToLower(prefix)) && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), strings.ToLower(prefix)
		}
	}
	return native, channel
}

func armorModelForChannel(model, channel string) string {
	if channel == "qoder" {
		return "qoder/" + model
	}
	return model
}

// 状态只描述网关是否注入指令，不把它当成模型限制已被突破的证据。
func armorStatus(decision armor.Decision) (string, string) {
	if decision.Applied {
		return "injected", "预设已加入请求，实际回答效果待验证"
	}
	switch decision.Reason {
	case "armor is not allowed for this key":
		return "not_allowed", "当前密钥未获准使用破甲"
	case "armor is disabled for this key", "not enabled":
		return "disabled", "当前密钥未启用破甲，本次不会加入预设"
	case "disabled for this request by header":
		return "request_disabled", "本次请求已关闭预设"
	case "disabled for this model by binding":
		return "model_disabled", "这个型号已单独关闭预设"
	case "armor is not configured", "no presets loaded":
		return "no_presets", "没有加载预设，请检查配置或导入预设包"
	case "no preset for family generic":
		return "unknown_model", "无法识别实际模型；自动选型请改用具体型号或设置明确绑定"
	case "per-request preset selection is not allowed":
		return "selection_forbidden", "当前配置不允许请求指定预设"
	}
	if strings.HasPrefix(decision.Reason, "no preset for family ") {
		return "no_matching_preset", "已识别模型系列，但没有可用预设"
	}
	return "configuration_error", "预设配置或指定名称不可用，请检查预设页"
}

func armorSelectionKind(decision armor.Decision) string {
	if decision.Source == armor.SourceBinding {
		return "binding"
	}
	if len(decision.Preset.ModelPrefixes) > 0 {
		return "version"
	}
	return "family"
}

// 成功、失败和流式响应均报告同一份诊断，不输出提示正文或账号凭证。
func setArmorHeaders(w http.ResponseWriter, decision armor.Decision) {
	w.Header().Set(armor.HeaderApplied, armorHeaderValue(decision))
	status, _ := armorStatus(decision)
	w.Header().Set("X-RelayPool-Armor-Status", status)
	if decision.Applied {
		w.Header().Set(armor.HeaderPreset, decision.Preset.Name)
		w.Header().Set("X-RelayPool-Armor-SHA256", armor.TextSHA256(decision.Preset.Body))
		w.Header().Set("X-RelayPool-Armor-Bytes", itoa(len(decision.Preset.Body)))
	}
}
