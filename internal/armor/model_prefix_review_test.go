package armor

import "testing"

// 版本边界与日期分别验证，避免只增加几个真实型号就遗漏紧凑别名。
func TestModelPrefixVersionBoundary(t *testing.T) {
	cases := []struct {
		model, prefix string
		expected      bool
	}{
		{"claude-opus-4", "claude-opus-4", true},
		{"claude-opus-4-20250514", "claude-opus-4", true},
		{"claude-opus-4-20250230", "claude-opus-4", false},
		{"claude-opus-4-6", "claude-opus-4", false},
		{"claude-opus-4-1-20261011", "claude-opus-4-1", true},
		{"gpt6-astra", "gpt6", true},
		{"gpt6-1-sol", "gpt6", false},
		{"gpt6-10-sol", "gpt6", false},
		{"gpt-6-1-2-sol", "gpt-6-1", false},
		{"deepseek-r1-0528", "deepseek-r1", true},
		{"gemini-2-5-flash-8b", "gemini-2-5-flash", true},
		{"gpt-6-astra", "", false},
	}
	for _, c := range cases {
		t.Run(c.model+"/"+c.prefix, func(t *testing.T) {
			if matchesModelPrefix(c.model, c.prefix) != c.expected {
				t.Fatal("版本或日期边界判断错误")
			}
		})
	}
}
