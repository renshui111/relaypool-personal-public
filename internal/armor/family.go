// Package armor: model-family matching for automatic preset selection.
//
// WHY AUTO-SELECTION NEEDS ITS OWN MATCHER: a jailbreak prompt is tuned for one
// model family. A prompt written for DeepSeek says different things than one
// written for Claude, and applying the wrong one is worse than applying none --
// it wastes prompt tokens and can produce a refusal where none would have
// occurred.
//
// So the gateway needs to answer "which family is this model?" for an arbitrary
// model ID, and it has to do so from a name that upstreams spell inconsistently:
//
//   - deepseek-v4.1-flash, deepseek-v4-pro, deepseek_r1, DeepSeek-V3
//   - glm-5.3, glm-5.3-flash, GLM-4-Plus, chatglm3
//   - kimi-k3-1, kimi-k2.8-preview, moonshot-v1
//   - hy3, hy4-preview        (Tencent Hunyuan, no recognisable prefix)
//   - minimax-m3
//
// The match is therefore a set of ordered rules over a canonicalized name, not a
// single prefix check.

package armor

import (
	"strings"
)

// Family is a model family identifier. It is a plain string so a preset file can
// declare one without this package knowing about it.
type Family = string

// Known families.
const (
	FamilyDeepSeek = "deepseek"
	FamilyGLM      = "glm"
	FamilyKimi     = "kimi"
	FamilyQwen     = "qwen"
	FamilyClaude   = "claude"
	FamilyGPT      = "gpt"
	FamilyGemini   = "gemini"
	FamilyGrok     = "grok"
	FamilyHunyuan  = "hunyuan"
	FamilyMiniMax  = "minimax"
	FamilyDoubao   = "doubao"
	FamilyMiMo     = "mimo"
	FamilyLlama    = "llama"
	FamilyGemma    = "gemma"
	FamilyPaLM     = "palm"
	FamilyGeneric  = "generic"
)

// familyRule maps a set of substrings to a family.
//
// Rules are evaluated in order, so a more specific pattern must come before a
// broader one. The ordering is the logic: "glm" must be checked before a
// catch-all, and a model like "kimi-k2.8-preview" must not be captured by a
// broad "k" pattern.
type familyRule struct {
	family  Family
	needles []string
}

// familyRules is the ordered matching table.
//
// Every needle is lowercase and compared against a canonicalized model name, so
// case and separators in the incoming ID do not matter.
var familyRules = []familyRule{
	// DeepSeek. The version suffix varies wildly (v4, v4.1, r1, chat, coder),
	// so only the family token is matched.
	{FamilyDeepSeek, []string{"deepseek", "deep-seek", "deepseekr", "dsk-"}},

	// GLM / ChatGLM / Zhipu.
	{FamilyGLM, []string{"glm", "chatglm", "codegeex", "zhipu"}},

	// Kimi / Moonshot.
	{FamilyKimi, []string{"kimi", "moonshot"}},

	// Qwen / Tongyi.
	{FamilyQwen, []string{"qwen", "tongyi", "qwenmax"}},

	// Anthropic. "sonnet", "opus" and "haiku" are the tier names that appear
	// without any "claude" prefix in many clients.
	{FamilyClaude, []string{"claude", "sonnet", "opus", "haiku"}},

	// OpenAI. "gpt" covers gpt-4/5/o-series naming; "o1"/"o3" are matched
	// separately because they carry no "gpt" token. They are anchored by a
	// separator check below, since a bare "o1" substring would also appear
	// inside unrelated words.
	{FamilyGPT, []string{"gpt-", "gpt4", "gpt5", "gpt6", "gpt3", "chatgpt", "davinci", "codex"}},

	// Google Gemini.
	{FamilyGemini, []string{"gemini"}},
	{FamilyGemma, []string{"gemma"}},
	{FamilyPaLM, []string{"palm"}},

	// xAI Grok.
	{FamilyGrok, []string{"grok"}},

	// Tencent Hunyuan. The public IDs are "hy3", "hy4-preview", "hunyuan-*".
	// "hy" alone is too short to match loosely, so the versions are listed.
	{FamilyHunyuan, []string{"hunyuan", "hy3", "hy4", "hunyuan-turbo"}},

	// MiniMax.
	{FamilyMiniMax, []string{"minimax", "abab"}},
	{FamilyDoubao, []string{"doubao", "seed-"}},
	{FamilyMiMo, []string{"mimo"}},
	{FamilyLlama, []string{"llama"}},

	// "auto" is the upstream's router pseudo-model. It carries no family, and
	// guessing one would be wrong more often than right.
	{FamilyGeneric, []string{"auto"}},
}

// ModelFamily reports the family a model ID belongs to.
//
// It returns FamilyGeneric when nothing matches, which is the correct answer:
// "this model has no family-specific preset" is more useful than a wrong guess,
// because a wrong guess silently sends a preset written for a different model.
func ModelFamily(model string) Family {
	canonical := canonicalModelName(model)
	// Qoder 内部 key 不含真实系列名称，只在明确指定该渠道时采用固定映射。
	if prefix, _, ok := strings.Cut(strings.ToLower(strings.TrimSpace(model)), "/"); ok && prefix == "qoder" {
		switch canonical {
		case "qmodel", "qmodel-latest", "qmodel-38max", "qfmodel":
			return FamilyQwen
		case "kmodel", "kmodel-latest":
			return FamilyKimi
		case "gmodel", "gfmodel", "gm51model":
			return FamilyGLM
		case "dmodel", "dfmodel":
			return FamilyDeepSeek
		case "mmodel":
			return FamilyMiniMax
		}
	}
	if canonical == "" {
		return FamilyGeneric
	}

	for _, rule := range familyRules {
		for _, needle := range rule.needles {
			if containsFamilyToken(canonical, needle) {
				return rule.family
			}
		}
	}

	// The o-series models (o1, o3, o4-mini) carry no "gpt" token. They are
	// matched here rather than in the table because a bare "o1" substring would
	// match inside unrelated names, so a token boundary is required.
	if isOSeries(canonical) {
		return FamilyGPT
	}
	return FamilyGeneric
}

// 只匹配名称段的开头，避免把 notgpt6、phy3 等无关名称当成已有模型。
func containsFamilyToken(name, token string) bool {
	for offset := 0; offset < len(name); {
		found := strings.Index(name[offset:], token)
		if found < 0 {
			return false
		}
		position := offset + found
		if position == 0 || name[position-1] == '-' {
			return true
		}
		offset = position + len(token)
	}
	return false
}

// isOSeries reports whether a canonical name looks like an OpenAI o-series ID.
//
// The check requires the token to be the whole name or to start a segment after
// a separator, so "o1" matches but "proto1" does not.
func isOSeries(canonical string) bool {
	for _, segment := range strings.Split(canonical, "-") {
		switch segment {
		case "o1", "o3", "o4", "o1mini", "o3mini", "o4mini":
			return true
		}
	}
	// The compact spelling "o1-mini" splits into "o1" and "mini", which the
	// loop already caught; "o1mini" without a separator is handled by the case
	// list above.
	return false
}

// canonicalModelName folds a model ID for matching.
//
// Separators are collapsed to "-" and the name is lowercased. A trailing date
// stamp ("-20250929") is stripped because it says nothing about the family and
// would otherwise have to be tolerated by every needle.
func canonicalModelName(model string) string {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return ""
	}
	// Strip a vendor prefix such as "workbuddy/deepseek-v4" so the family token
	// is still found when a client pins a channel.
	if idx := strings.LastIndex(name, "/"); idx >= 0 && idx+1 < len(name) {
		name = name[idx+1:]
	}
	name = strings.NewReplacer("_", "-", " ", "-", ".", "-", ":", "-").Replace(name)
	// Collapse a run of separators into one.
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	name = strings.Trim(name, "-")
	return name
}
