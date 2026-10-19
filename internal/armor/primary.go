package armor

// 主来源是可复现的集成选择，实际破甲效果不由星数、提示长度或作者宣传推断。
func PrimarySource() map[string]any {
	if DistributionProfile == "public" {
		return map[string]any{"id": "user", "name": "用户导入的预设包", "families": []string{}, "note": "公开核心不附带许可尚未厘清的研究正文；可导入本人有权使用的预设。", "efficacy_verified": false}
	}
	return map[string]any{
		"id":                "coldbrew",
		"name":              "冷咖啡工作台 · 公开提示文本",
		"url":               "https://github.com/3641397194-wq/gpt6-Astra/tree/cb5916819d520d6ebe4703f7e3d447253416540b",
		"revision":          "cb5916819d520d6ebe4703f7e3d447253416540b",
		"license":           "MIT",
		"families":          []string{"gpt", "claude", "gemini", "grok", "deepseek", "glm", "kimi"},
		"note":              "仅采用公开提示文本；版本专用提示优先，随后系列默认。未移植客户端补丁；豆包安装说明不作为 API 提示。",
		"efficacy_verified": false,
	}
}
