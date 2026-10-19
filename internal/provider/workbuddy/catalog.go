package workbuddy

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/relaypool/relaypool/internal/provider"
)

// catalogEnvelope is the upstream catalogue response.
//
// The real shape (confirmed against the live upstream) is:
//
//	{"code":0,"msg":"OK","data":{"models":[...],"agents":[...],"endpoint":"..."}}
//
// The model list is nested under "data", NOT at the top level. A parser that
// only looks at top-level keys therefore rejects a 200 OK with "response was
// not usable" -- which is exactly what happened before this was corrected
// against a live response.
//
// The flat top-level variants are still accepted, because a gateway should
// tolerate a shape change rather than hard-fail on one.
type catalogEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`

	// Data is the wrapper the CN endpoint uses.
	Data *struct {
		Models []catalogModel `json:"models"`
		// Agents also carry model lists; they are merged in as a fallback so a
		// model only reachable through an agent is still advertised.
		Agents []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
	} `json:"data"`

	// Flat fallbacks for a shape change or a different endpoint.
	Models []catalogModel `json:"models"`
	List   []catalogModel `json:"list"`
}

// catalogModel mirrors one entry of data.models.
//
// Field names are taken from a real response. Note what is ABSENT: there is no
// "disabled" flag. A model that cannot be used is instead indicated by being
// absent, so no filtering on a disabled flag is possible or needed.
type catalogModel struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	DescriptionEn   string `json:"descriptionEn"`
	DescriptionZh   string `json:"descriptionZh"`
	MaxInputTokens  int    `json:"maxInputTokens"`
	MaxOutputTokens int    `json:"maxOutputTokens"`
	MaxAllowedSize  int    `json:"maxAllowedSize"`
	IsDefault       bool   `json:"isDefault"`

	// OnlyReasoning marks a model that always thinks and must never be told to
	// disable reasoning.
	OnlyReasoning     bool `json:"onlyReasoning"`
	SupportsReasoning bool `json:"supportsReasoning"`
	SupportsImages    bool `json:"supportsImages"`
	// SupportsToolCall is a pointer so an absent field is distinguishable from
	// an explicit false.
	SupportsToolCall *bool `json:"supportsToolCall"`

	// Reasoning carries the effort metadata. The live response has only
	// effort + summary, with no supportedEfforts list, so the option set is
	// derived rather than read.
	Reasoning struct {
		Effort             string   `json:"effort"`
		DefaultEffort      string   `json:"defaultEffort"`
		SupportedEfforts   []string `json:"supportedEfforts"`
		CanDisableThinking *bool    `json:"canDisableThinking"`
		Summary            string   `json:"summary"`
	} `json:"reasoning"`
}

// parseCatalog converts a catalogue response into model descriptors.
func parseCatalog(reader io.Reader) ([]provider.ModelInfo, error) {
	// Bound the read: the live response is ~26 KB, and an unbounded decode of a
	// remote response is a memory vector.
	limited := io.LimitReader(reader, 4<<20)

	var envelope catalogEnvelope
	if err := json.NewDecoder(limited).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("workbuddy: decode catalogue: %w", err)
	}

	// A non-zero code is an application-level failure carried on HTTP 200, so
	// it must be checked explicitly: treating it as success would yield an
	// empty catalogue and look like "this account has no models".
	if envelope.Code != 0 {
		return nil, fmt.Errorf("workbuddy: catalogue rejected with code %d: %s", envelope.Code, envelope.Msg)
	}

	entries := envelope.Models
	if len(entries) == 0 {
		entries = envelope.List
	}
	if len(entries) == 0 && envelope.Data != nil {
		entries = envelope.Data.Models
	}

	// An empty catalogue is a legitimate answer, not an error: returning a
	// non-nil empty slice lets the scheduler treat it as "known to be empty"
	// and filter, while nil would mean "unknown" and fail open.
	out := make([]provider.ModelInfo, 0, len(entries))
	seen := map[string]struct{}{}
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, catalogEntryToModel(entry))
	}

	// Merge in agent-only models. A model that appears solely under an agent is
	// still callable, so omitting it would under-advertise the account.
	if envelope.Data != nil {
		for _, agent := range envelope.Data.Agents {
			for _, name := range agent.Models {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				if _, dup := seen[name]; dup {
					continue
				}
				seen[name] = struct{}{}
				// No metadata is available for an agent-listed model, so it is
				// advertised with the capability defaults.
				out = append(out, provider.ModelInfo{
					NativeModel: name,
					PublicModel: name,
					DisplayName: name,
					Tools:       true,
				})
			}
		}
	}
	return out, nil
}

// catalogEntryToModel derives capabilities from one catalogue entry.
//
// Two things are worth spelling out because the live response differs from the
// obvious assumption:
//
//   - The live payload has NO supportedEfforts list, only a single effort and a
//     summary. So when no explicit list is present, the option set is derived
//     from the default plus the disable option.
//   - A model with onlyReasoning=true can never be told "none"; the upstream
//     errors. CanDisableThinking is therefore forced false regardless of what
//     the entry claims.
func catalogEntryToModel(entry catalogModel) provider.ModelInfo {
	options := uniqueReasoningLevels(entry.Reasoning.SupportedEfforts)
	defaultLevel := NormalizeReasoningLevel(entry.Reasoning.DefaultEffort)
	if defaultLevel == "" {
		defaultLevel = NormalizeReasoningLevel(entry.Reasoning.Effort)
	}

	// With no explicit option list, the default is the only known level.
	if len(options) == 0 && defaultLevel != "" {
		options = []string{defaultLevel}
	}
	if defaultLevel == "" && len(options) > 0 {
		defaultLevel = options[0]
	} else if defaultLevel != "" && !containsLevel(options, defaultLevel) {
		options = append([]string{defaultLevel}, options...)
	}

	canDisable := !entry.OnlyReasoning
	if entry.Reasoning.CanDisableThinking != nil {
		canDisable = *entry.Reasoning.CanDisableThinking && !entry.OnlyReasoning
	}
	// "none" is how a client says "do not think", so it belongs in the option
	// list whenever thinking is optional.
	if canDisable && (len(options) > 0 || entry.SupportsReasoning) && !containsLevel(options, "none") {
		options = append([]string{"none"}, options...)
	}

	// Tools default to true: every WorkBuddy model accepts them, and
	// under-claiming would make the gateway reject valid tool calls.
	tools := true
	if entry.SupportsToolCall != nil {
		tools = *entry.SupportsToolCall
	}

	contextWindow := entry.MaxInputTokens
	if contextWindow == 0 {
		contextWindow = entry.MaxAllowedSize
	}

	return provider.ModelInfo{
		NativeModel:        entry.ID,
		PublicModel:        entry.ID,
		DisplayName:        firstNonEmpty(entry.Name, entry.ID),
		ContextWindow:      contextWindow,
		MaxOutput:          entry.MaxOutputTokens,
		Tools:              tools,
		Images:             entry.SupportsImages,
		Reasoning:          entry.SupportsReasoning || len(options) > 0 || defaultLevel != "",
		ReasoningOptions:   options,
		ReasoningDefault:   defaultLevel,
		CanDisableThinking: canDisable,
	}
}

// uniqueReasoningLevels normalizes and deduplicates an effort list.
func uniqueReasoningLevels(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, item := range raw {
		level := NormalizeReasoningLevel(item)
		if level == "" {
			continue
		}
		if _, dup := seen[level]; dup {
			continue
		}
		seen[level] = struct{}{}
		out = append(out, level)
	}
	return out
}

// canonicalModelID folds a model name for comparison, matching the scheduler's
// canonicalization so catalogue lookups agree with routing.
func canonicalModelID(model string) string {
	key := strings.ToLower(strings.TrimSpace(model))
	key = strings.NewReplacer("_", "-", " ", "-").Replace(key)
	return key
}

// alreadyCheckedInMessage is the check-in result text for an account that
// already claimed today's reward.
//
// It is a constant rather than an inline literal so the ONLY copy of this text
// lives in one place, and so the surrounding Go source stays pure ASCII. That
// second point is not cosmetic: an editing tool mishandling the encoding once
// truncated a Chinese literal mid-character and destroyed the closing quote,
// breaking the whole file. Keeping non-ASCII out of the source removes that
// class of failure entirely.
//
// The bytes are the UTF-8 spelling of "already checked in today, come back
// tomorrow", which is what the upstream returns.
const alreadyCheckedInMessage = "\u4eca\u5929\u5df2\u7b7e\u5230\uff0c\u8bf7\u660e\u5929\u518d\u6765"

// alreadyCheckedIn reports whether a check-in rejection means "already done
// today".
//
// This must be distinguished from a real failure: treating it as an error would
// make the scheduler retry all day, and treating a genuine error as "already
// done" would silently skip the day's reward.
func alreadyCheckedIn(body string) bool {
	lower := strings.ToLower(body)
	for _, needle := range []string{
		"already checked in", "already checked-in", "already signed in today", "already signed-in today",
		"已签到", "已完成签到", "已经签到", "重复签到",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}
