// Package modelid normalizes spelling only; it never removes variant suffixes.
package modelid

import (
	"regexp"
	"strings"
)

var separators = strings.NewReplacer("_", "-", " ", "-")
var version = regexp.MustCompile(`^(glm|gpt|gemini|deepseek-v|minimax-m|kimi-k|qwen)[-.]?([0-9]+)[.-]([0-9]{1,2})($|-.*|v$)`)
var claude = regexp.MustCompile(`^claude-(sonnet|opus|haiku)-([0-9]+)[.-]([0-9]{1,2})($|-.*)`)
var claudeReordered = regexp.MustCompile(`^claude-([0-9]+)[.-]([0-9]{1,2})-(sonnet|opus|haiku)($|-.*)`)

func Canonical(model string) string {
	id := separators.Replace(strings.ToLower(strings.TrimSpace(model)))
	if m := version.FindStringSubmatch(id); m != nil {
		prefix := m[1]
		if prefix == "glm" || prefix == "gpt" || prefix == "gemini" {
			prefix += "-"
		}
		return prefix + m[2] + "." + m[3] + m[4]
	}
	if m := claude.FindStringSubmatch(id); m != nil {
		return "claude-" + m[1] + "-" + m[2] + "." + m[3] + m[4]
	}
	if m := claudeReordered.FindStringSubmatch(id); m != nil {
		return "claude-" + m[3] + "-" + m[1] + "." + m[2] + m[4]
	}
	return id
}
