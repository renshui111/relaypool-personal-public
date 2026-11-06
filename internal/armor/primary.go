package armor

// 主来源是可复现的集成选择，实际破甲效果不由星数、提示长度或作者宣传推断。
func PrimarySource() map[string]any {
	if DistributionProfile == "public" {
		return map[string]any{"id": "user", "name": "用户导入的预设包", "families": []string{}, "note": "公开核心不附带许可尚未厘清的研究正文；可导入本人有权使用的预设。", "efficacy_verified": false}
	}
	return map[string]any{
		"id":                "quincunx",
		"name":              "11 月精选 · 冷咖啡常驻正文 + 完整研究模板",
		"url":               "https://github.com/Quincunx33/Ai-jailbreak/tree/b01c2f446550ac310c9bda9d9606621fe341a791",
		"revision":          "b01c2f446550ac310c9bda9d9606621fe341a791 / fd9551aaeb325af1894eaa385234889f3a9c097a",
		"license":           "MIT",
		"families":          []string{"gpt", "claude", "gemini", "grok", "deepseek", "glm", "kimi", "qwen", "llama"},
		"note":              "11 个预设：9 个系列默认、1 个 R1 专用、1 个完整递归模板手动备选。仅有正文和小样本兼容性验证，未证实全破；宿主补丁和权重不属于提示词。",
		"efficacy_verified": false,
	}
}
