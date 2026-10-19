package armor

// Coverage 列出分配规则的覆盖范围，分配通过不代表上游破甲成功。
func (r *Registry) Coverage() []map[string]any {
	groups := []struct {
		family, label string
		models        []string
	}{
		{FamilyGPT, "GPT / Codex", []string{"gpt-5.6-sol", "gpt6-astra", "gpt6.1-sol", "gpt-4o", "o3-mini", "codex"}},
		{FamilyClaude, "Claude", []string{"claude-opus-4.1", "claude-opus-4.6", "claude-sonnet-3.5", "claude-sonnet-3.7", "claude-sonnet-4.6", "claude-haiku-4.5"}},
		{FamilyDeepSeek, "DeepSeek", []string{"deepseek-v3.1", "deepseek-v3.2", "deepseek-r1", "deepseek-r1-lite", "deepseek-chat", "deepseek-reasoner", "deepseek-v4.1-flash"}},
		{FamilyGemini, "Gemini", []string{"gemini-2.0-flash", "gemini-2.5-flash", "gemini-2.5-flash-lite", "gemini-2.5-pro", "gemini-3-flash-preview", "gemini-3-pro", "gemini-3.8-flash"}},
		{FamilyGLM, "GLM", []string{"chatglm3", "glm-4-plus", "glm-4.5", "glm-5.1", "glm-5.3"}},
		{FamilyKimi, "Kimi / Moonshot", []string{"moonshot-v1", "kimi-k2", "kimi-k2.6", "kimi-k2.7", "kimi-k2.8-preview", "kimi-k3-1"}},
		{FamilyQwen, "Qwen", []string{"qwen-turbo", "qwen-plus", "qwen-max", "qwen2.5-72b-instruct", "qwen3-coder-plus", "qwen3.8-max"}},
		{FamilyGrok, "Grok", []string{"grok-2", "grok-3", "grok-3-mini", "grok-4", "grok-4.7"}},
		{FamilyLlama, "Llama", []string{"llama-3.1-70b-instruct", "llama-3.3-70b-instruct", "llama-4-maverick"}},
		{FamilyDoubao, "豆包 / Seed", []string{"doubao-pro", "seed-2.1"}},
		{FamilyHunyuan, "混元", []string{"hunyuan-turbo", "hy3", "hy4-preview"}},
		{FamilyMiniMax, "MiniMax", []string{"minimax-m2", "minimax-m3"}},
		{FamilyMiMo, "MiMo", []string{"mimo-v2.6"}},
		{FamilyGemma, "Gemma", []string{"gemma-3-27b"}},
		{FamilyPaLM, "PaLM", []string{"palm-2"}},
	}
	rows := make([]map[string]any, 0, len(groups))
	for _, group := range groups {
		models := make([]map[string]any, 0, len(group.models))
		for _, model := range group.models {
			preset, ok := r.ForModel(model)
			kind := "none"
			if ok {
				kind = "family"
				if len(preset.ModelPrefixes) > 0 {
					kind = "version"
				}
			}
			models = append(models, map[string]any{"model": model, "available": ok, "preset": preset.Name, "kind": kind, "efficacy_verified": false})
		}
		rows = append(rows, map[string]any{"family": group.family, "label": group.label, "models": models})
	}
	return rows
}
