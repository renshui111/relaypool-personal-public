package modelid

import "testing"

func TestCanonicalSpellingAndDistinctVariants(t *testing.T) {
	for input, want := range map[string]string{
		"GLM_5_2": "glm-5.2", "GLM 5.2": "glm-5.2", "glm.5.2": "glm-5.2", "glm5.2": "glm-5.2",
		"claude-4-5-sonnet": "claude-sonnet-4.5", "Claude_Sonnet_4_5": "claude-sonnet-4.5",
		"DeepSeek-V3-2": "deepseek-v3.2", "Gemini-2-5-pro": "gemini-2.5-pro", "gpt-5-2": "gpt-5.2",
		"qwen3-32b": "qwen3-32b", "qwen3-235b-a22b": "qwen3-235b-a22b",
		"glm-5.2-harmony": "glm-5.2-harmony", "glm-5.2-sft-harmony": "glm-5.2-sft-harmony",
		"deepseek-v4-flash-1017": "deepseek-v4-flash-1017", "deepseek-v4-flash": "deepseek-v4-flash",
		"kimi-k3.1-pro": "kimi-k3.1-pro", "kimi-k3-1": "kimi-k3.1",
		"vendor/Some_Model": "vendor/some-model", "gpt-4o": "gpt-4o",
	} {
		if got := Canonical(input); got != want {
			t.Errorf("%q: got %q want %q", input, got, want)
		}
	}
	for _, pair := range [][2]string{{"glm-5.2", "glm-5.2-harmony"}, {"glm-5.3", "glm-5.3-test"}, {"qwen3-32b", "qwen3-235b-a22b"}, {"kimi-k3.1", "kimi-k3.1-pro"}, {"deepseek-v4.1-flash-0930", "deepseek-v4.1-flash-1017"}, {"claude-opus-4.5", "claude-sonnet-4.5"}} {
		if Canonical(pair[0]) == Canonical(pair[1]) {
			t.Fatalf("merged distinct variants: %v", pair)
		}
	}
}
