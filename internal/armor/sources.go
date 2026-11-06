package armor

// ResearchSource 保留报告项目的类型及集成入口；具体文本另行预览后导入。
type ResearchSource struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Repository  string   `json:"repository"`
	Branch      string   `json:"branch"`
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	Families    []string `json:"families"`
}

var researchSources = []ResearchSource{
	{"calrton", "Calrton 提示词合集", "Calrton/jailbreak-prompts", "main", "prompts", "按模型族整理的提示文本合集，保留原文件及来源署名。", []string{"gpt", "claude", "gemini", "deepseek", "grok", "llama"}},
	{"dsh-purge", "dsh-purge", "YuJunZhiXue/dsh-purge", "master", "plugin", "接入其内置默认提示文本；逐模型切换由本项目统一管理。DSH 宿主补丁只在原项目中使用。", []string{"deepseek", "qwen", "kimi", "glm"}},
	{"gpt-instruct", "gpt-instruct", "MDX-Tom/gpt-instruct", "main", "prompts", "导入发布包中的 GPT / Codex 提示文本，保留模型版本名称。", []string{"gpt"}},
	{"spiritual", "Spiritual Spell", "Goochbeater/Spiritual-Spell-Red-Teaming", "main", "prompts", "按 Claude 版本组织的参考文本，可下载选择后导入。", []string{"claude"}},
	{"coldbrew", "冷咖啡 · 常驻正文精选", "3641397194-wq/gpt6-Astra", "main", "prompts", "已核对七个模型系列的完整常驻规则；去除激活、广告、中转路由和宿主安装内容，保留固定提交与许可证。", []string{"gpt", "claude", "gemini", "deepseek", "glm", "grok", "kimi"}},
	{"libertas", "L1B3RT4S 原始合集", "elder-plinius/L1B3RT4S", "main", "prompts", "原始模型族提示合集，使用时保留其版权及许可说明。", []string{"gpt", "claude", "gemini", "deepseek", "grok", "llama"}},
	{"shadowhackrs", "ShadowHackrs 合集", "ShadowHackrs/Jailbreaks-GPT-Gemini-deepseek-", "main", "prompts", "GPT、Gemini 与 DeepSeek 的补充来源。", []string{"gpt", "gemini", "deepseek"}},
	{"langgpt", "LangGPT 合集", "langgptai/LLM-Jailbreaks", "main", "prompts", "通用案例合集，可按实际模型手动绑定。", []string{}},
	{"quincunx", "Ai jailbreak · 完整模板精选", "Quincunx33/Ai-jailbreak", "main", "prompts", "完整递归模板、辩证审计模板和 R1 研究配置；任务变量已映射当前消息，固定源提交，不把格式测试当全破证明。", []string{"qwen", "llama", "deepseek"}},
	{"deepseek-extra", "DeepSeek 补充预设", "superisuer/deepseek-jailbreak", "main", "prompts", "ByeByePass 与 bypass 系列文本。", []string{"deepseek"}},
	{"glm", "GLM 5.3f 预设", "chiina66/glm5.3f-jailbreak", "main", "prompts", "报告中的 GLM 专用文本。", []string{"glm"}},
	{"manual", "Prompt Jailbreak Manual", "Acmesec/PromptJailbreakManual", "main", "manual", "提示攻击分类及防护参考手册，作为阅读资源使用。", []string{}},
	{"easyjailbreak", "EasyJailbreak", "EasyJailbreak/EasyJailbreak", "main", "framework", "Python 评估框架，保留研究与使用入口；不作为系统提示注入。", []string{}},
	{"awesome", "Awesome Jailbreak on LLMs", "yueliu1999/Awesome-Jailbreak-on-LLMs", "main", "papers", "论文与评估资料索引，作为阅读资源使用。", []string{}},
}

func ResearchSources() []ResearchSource {
	// The console offers only reviewed, offline-available text. Legacy source
	// IDs remain resolvable for previously imported user presets.
	out := []ResearchSource{}
	for _, source := range researchSources {
		if len(BundledResearchFiles(source.ID)) > 0 {
			out = append(out, source)
		}
	}
	for i := range out {
		out[i].Families = append([]string{}, out[i].Families...)
	}
	return out
}

func FindResearchSource(id string) (ResearchSource, bool) {
	for _, source := range researchSources {
		if source.ID == id {
			return source, true
		}
	}
	return ResearchSource{}, false
}
