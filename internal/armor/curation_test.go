package armor

import (
	"strings"
	"testing"
)

func TestCuratedDefaultsSinglePerFamilyAndOfflineParity(t *testing.T) {
	if DistributionProfile == "public" {
		t.Skip("公开核心不分发本地研究资料，预设功能由预设包测试覆盖")
	}
	disk, embedded := NewRegistry(), NewRegistry()
	if _, err := disk.LoadDir("../../prompts"); err != nil {
		t.Fatal(err)
	}
	if _, err := embedded.LoadBundledPresets(); err != nil {
		t.Fatal(err)
	}
	if len(disk.List()) != 26 || len(embedded.List()) != 26 {
		t.Fatal("默认预设数量不符")
	}
	seen := map[string]bool{}
	for _, p := range disk.List() {
		if len(p.ModelPrefixes) == 0 && seen[p.Family] || !p.IsAutoSelectable() || p.SourceURL == "" || p.SelectionNote == "" {
			t.Fatal("默认系列重复或缺少筛选依据")
		}
		if len(p.ModelPrefixes) == 0 {
			seen[p.Family] = true
		}
		other, err := embedded.Lookup(p.Name)
		if err != nil || p.Body != other.Body || p.SourceSHA256 != other.SourceSHA256 {
			t.Fatal("离线内置资源与磁盘版本不一致")
		}
		model := "openai-compatible/" + p.Family + "-test"
		if len(p.ModelPrefixes) > 0 {
			model = "openai-compatible/" + p.ModelPrefixes[0]
		}
		selected, ok := embedded.ForModel(model)
		if !ok || selected.Name != p.Name {
			t.Fatalf("系列 %s 未自动选中", p.Family)
		}
	}
	for _, model := range []string{"unknown-model", "auto", "hy4-preview", "mimo-v2.6", "minimax-m3", "doubao-pro", "gemma-3-27b", "palm-2"} {
		if _, ok := embedded.ForModel(model); ok {
			t.Fatalf("无专属资源的 %s 被错误套用", model)
		}
	}
}

func TestVersionSelectionUsesLongestPrefixAndBindings(t *testing.T) {
	if DistributionProfile == "public" {
		t.Skip("该用例验证本地研究版本的选定正文")
	}
	r := NewRegistry()
	if _, err := r.LoadBundledPresets(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"gpt-5.6-sol": "gpt-5-6-curated", "gpt6-astra": "gpt-6-curated", "openai/gpt6.1-sol": "gpt-6-1-curated",
		"GPT_6_1_SOL_20261009": "gpt-6-1-curated", "gpt-6.10-sol": "gpt-curated", "gpt-4o": "gpt-curated",
		"claude-opus-4.1": "claude-opus-4-1-curated", "claude-sonnet-3.7-latest": "claude-sonnet-3-7-curated",
		"sonnet-4.6": "claude-curated", "deepseek-r1-lite": "deepseek-r1-curated", "deepseek-reasoner": "deepseek-r1-curated",
		"claude-opus-4.6": "claude-curated", "claude-opus-4-20250514": "claude-opus-4-curated",
		"gpt6.10-sol": "gpt-curated", "gpt-6.1.2-sol": "gpt-curated", "gpt6-20261011": "gpt-6-curated",
		"deepseek-v3.2": "deepseek-v3-2-curated", "deepseek-v4.1-flash": "deepseek-curated",
		"gemini-2.5-flash-lite-preview": "gemini-2-5-flash-lite-curated", "gemini-3-pro": "gemini-3-pro-curated",
		"gemini-3.8-flash": "gemini-curated", "glm-4.5": "glm-curated", "kimi-k2.7": "kimi-curated",
		"qwen3-coder-plus": "qwen-curated", "grok-3-mini": "grok-curated", "llama-3.3-70b": "llama-curated",
	}
	for model, expected := range cases {
		p, ok := r.ForModel(model)
		if !ok || p.Name != expected {
			t.Errorf("%s 分配 %s，期望 %s", model, p.Name, expected)
		}
	}
	r.SetModelBindings(map[string]string{"gpt-6.1-sol": "off", "gemini-2.5-pro": "gemini-curated"})
	if _, ok := r.ForModel("openai/gpt-6.1-sol"); ok {
		t.Fatal("版本规则绕过具体型号关闭")
	}
	if p, ok := r.ForModel("gemini-2.5-pro"); !ok || p.Name != "gemini-curated" {
		t.Fatal("版本规则覆盖用户绑定")
	}
	r.SetModelBindings(nil)
	if p, _ := r.ForModel("gpt-6.1-sol"); p.Name != "gpt-6-1-curated" {
		t.Fatal("清除绑定未恢复版本选择")
	}
}

func TestCleanBodiesKeepPromptAndExcludeWorkbench(t *testing.T) {
	if DistributionProfile == "public" {
		t.Skip("公开核心不附带第三方正文")
	}
	r := NewRegistry()
	if _, err := r.LoadBundledPresets(); err != nil {
		t.Fatal(err)
	}
	for _, p := range r.List() {
		if p.Name == "claude-curated" && (!strings.Contains(p.Body, "完成条件") || !strings.Contains(p.Body, "代码、偏移、命令")) {
			t.Fatal("Claude 常驻规则段再次被过度清理")
		}
		if p.Name == "gpt-curated" && (!strings.Contains(p.Body, "write atomically") || !strings.Contains(p.Body, "done-condition")) {
			t.Fatal("GPT 常驻规则段不完整")
		}
		if len(strings.TrimSpace(p.Body)) < 100 {
			t.Fatalf("%s 正文被清空", p.Name)
		}
		for _, unwanted := range []string{"coldcoffeeai.com", "QQ一群", "ida-pro-mcp", "CHA-LEAVES", "User: 4080", "print the dirtiest", "INJECTED REVISION ACTION", "ROLLBACK.sh"} {
			if strings.Contains(p.Body, unwanted) {
				t.Fatalf("%s 包含无关正文 %s", p.Name, unwanted)
			}
		}
	}
}
