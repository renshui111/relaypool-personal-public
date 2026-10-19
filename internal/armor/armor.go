// Package armor holds the optional system-prompt presets, selected by model and
// gated by one global switch.
//
// WHY THIS IS A SEPARATE PACKAGE WITH AN EXPLICIT OPT-IN:
//
// A system prompt is the most behaviour-changing thing this gateway can send.
// Substituting the caller's prompt changes what the model will and will not do,
// which is a decision that belongs to whoever operates the gateway, not to the
// gateway's default and not to a caller. So the armor is:
//
//   - OFF by default, controlled by ONE global switch (see the api package's
//     armorSwitch). With the switch off nothing is substituted, ever.
//   - With the switch on, the preset is chosen from the REQUESTED MODEL, so a
//     client needs no header at all.
//   - A request header may only NARROW the effect, never enable it: an explicit
//     "0" suppresses armor for that one request, but no header can turn armor on
//     while the operator has switched it off.
//   - Never silently applied: the response carries a header naming the preset
//     that was used, so a caller can always tell what was sent.
//
// The header asymmetry is deliberate. Armor rewrites what the model is told, so
// the operator's "off" must mean off; and "less armor" can never make the
// gateway do something the operator did not enable, so narrowing is always safe.
//
// WHY SELECTION IS BY MODEL, NOT BY ACCOUNT (changed 2026-10-01):
//
// The preset is chosen from the model the request asks for. An earlier design
// also allowed binding a preset to an ACCOUNT, which turned out to be the wrong
// axis twice over: the same account serves many models, so an account-level
// preset would send a DeepSeek prompt to a GLM model; and it made the effective
// prompt depend on which account the scheduler happened to pick, which is
// invisible to the caller and changes between requests. Selecting by model means
// the prompt always matches what is being asked, and the same request produces
// the same prompt regardless of routing.
//
// The preset text itself is operator-supplied data (see prompts/*.md), not
// compiled-in prose, so it can be reviewed and changed without a rebuild.
package armor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Errors returned by the registry.
var (
	// ErrUnknownPreset means the requested preset does not exist.
	ErrUnknownPreset = errors.New("armor: unknown preset")
	// ErrDisabled means the preset exists but the account or request did not
	// opt in.
	ErrDisabled = errors.New("armor: not enabled")
)

// Header names that control the armor.
//
// The opt-in is expressed with headers rather than a body field because the
// body shape is the CLIENT's protocol (OpenAI or Anthropic), and adding a
// vendor field there would make the request invalid for a strict upstream.
const (
	// HeaderEnable turns the armor on for one request. Any non-empty value
	// other than "0"/"false" enables it. The preset itself is then chosen from
	// the requested model.
	HeaderEnable = "X-RelayPool-Armor"
	// HeaderPreset overrides the model-based selection with a named preset.
	HeaderPreset = "X-RelayPool-Armor-Preset"
	// HeaderApplied is set on the RESPONSE to report what was actually sent.
	// It exists so a caller is never left guessing whether substitution
	// happened, which matters because the effect is invisible in the output.
	HeaderApplied = "X-RelayPool-Armor-Applied"
)

// Preset is one named system-prompt replacement.
type Preset struct {
	// Name is the identifier a caller uses in the header.
	Name string
	// Description is shown in the console so an operator can tell presets
	// apart without reading the full text.
	Description string
	// Body is the system prompt sent to the model.
	Body string
	// File records where the body was loaded from, for diagnostics.
	File string

	// Family is the model family this preset targets, e.g. "deepseek". It is
	// what makes automatic selection possible: a request for a DeepSeek model
	// receives the DeepSeek preset without the caller naming it.
	//
	// An empty family means "not family-specific", which excludes the preset
	// from automatic selection while leaving it usable by explicit name.
	Family Family

	// AutoSelectable opts a preset in or out of automatic selection. Nil means
	// "yes when a family is set", which is the useful default; an explicit
	// false is how an operator keeps a preset available by name only.
	AutoSelectable *bool

	// Priority breaks ties when several presets target one family. The higher
	// value wins. Without it, selection would follow file load order, which an
	// operator cannot reason about or control.
	Priority      int
	SourceURL     string `json:"source_url,omitempty"`
	SourceSHA256  string `json:"source_sha256,omitempty"`
	License       string `json:"license,omitempty"`
	LicenseText   string `json:"license_text,omitempty"`
	Coverage      string `json:"coverage,omitempty"`
	SelectionNote string `json:"selection_note,omitempty"`
	// ModelPrefixes 限制版本预设的适用型号；空列表表示整个系列的默认提示。
	ModelPrefixes []string `json:"model_prefixes,omitempty"`
}

func (p Preset) clone() Preset {
	p.ModelPrefixes = append([]string(nil), p.ModelPrefixes...)
	if p.AutoSelectable != nil {
		value := *p.AutoSelectable
		p.AutoSelectable = &value
	}
	return p
}

// AutoSelectable reports whether this preset may be chosen by family.
func (p Preset) IsAutoSelectable() bool {
	if p.Family == "" {
		return false
	}
	if p.AutoSelectable != nil {
		return *p.AutoSelectable
	}
	return true
}

// Registry holds the available presets.
//
// It is safe for concurrent use: presets are read on the request path.
type Registry struct {
	mu      sync.RWMutex
	presets map[string]Preset
	// order preserves a stable listing order for the console.
	order []string
	// defaultName is used when a request enables the armor without naming a
	// preset.
	defaultName string
	// byFamily indexes auto-selectable presets by model family, so automatic
	// selection is a map lookup rather than a scan of every preset on every
	// request.
	byFamily      map[Family][]Preset
	modelBindings map[string]string
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		presets:       map[string]Preset{},
		byFamily:      map[Family][]Preset{},
		modelBindings: map[string]string{},
	}
}

// Register adds or replaces a preset.
func (r *Registry) Register(preset Preset) error {
	name := normalizeName(preset.Name)
	if name == "" {
		return fmt.Errorf("armor: preset name must not be empty")
	}
	if strings.TrimSpace(preset.Body) == "" {
		return fmt.Errorf("armor: preset %q has an empty body", name)
	}
	preset.Name = name
	preset.Family = normalizeFamily(preset.Family)
	preset = preset.clone()

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.presets[name]; !exists {
		r.order = append(r.order, name)
		sort.Strings(r.order)
	}
	r.presets[name] = preset
	if r.defaultName == "" {
		r.defaultName = name
	}
	r.reindexLocked()
	return nil
}

// reindexLocked rebuilds the family index. Callers must hold the write lock.
//
// The whole index is rebuilt rather than incrementally patched because
// Register may be REPLACING a preset whose family changed, and an incremental
// update would leave the old family pointing at it.
func (r *Registry) reindexLocked() {
	byFamily := map[Family][]Preset{}
	for _, name := range r.order {
		preset, ok := r.presets[name]
		if !ok || !preset.IsAutoSelectable() {
			continue
		}
		byFamily[preset.Family] = append(byFamily[preset.Family], preset)
	}
	// Sort each family's candidates so selection is deterministic: highest
	// priority first, then by name. Relying on map iteration order would make
	// the choice vary between runs.
	for family := range byFamily {
		candidates := byFamily[family]
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].Priority != candidates[j].Priority {
				return candidates[i].Priority > candidates[j].Priority
			}
			return candidates[i].Name < candidates[j].Name
		})
		byFamily[family] = candidates
	}
	r.byFamily = byFamily
}

// ForFamily returns the auto-selectable preset for a model family.
//
// It reports false when the family has no preset, which is the normal case for
// a model that no preset targets.
func (r *Registry) ForFamily(family Family) (Preset, bool) {
	if r == nil {
		return Preset{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	candidates := r.byFamily[normalizeFamily(family)]
	if len(candidates) == 0 {
		return Preset{}, false
	}
	// 版本专用预设不能成为其他型号的系列兜底。
	for _, preset := range candidates {
		if len(preset.ModelPrefixes) == 0 {
			return preset.clone(), true
		}
	}
	return Preset{}, false
}

// ForModel returns the auto-selectable preset for a model ID.
//
// This is the entry point the request path uses: it derives the family from the
// model name, so a caller never has to know the family taxonomy.
func (r *Registry) ForModel(model string) (Preset, bool) {
	if r == nil || r.Empty() {
		return Preset{}, false
	}
	r.mu.RLock()
	bound, exists := r.modelBindings[canonicalModelName(model)]
	if exists {
		preset, found := r.presets[bound]
		r.mu.RUnlock()
		return preset.clone(), found && bound != "off"
	}
	// 完整型号绑定优先，随后按最长版本前缀选择，最后才采用系列默认提示。
	canonical, family := canonicalModelName(model), ModelFamily(model)
	var best Preset
	bestLength := 0
	for _, preset := range r.byFamily[family] {
		for _, prefix := range preset.ModelPrefixes {
			prefix = canonicalModelName(prefix)
			if !matchesModelPrefix(canonical, prefix) {
				continue
			}
			if len(prefix) > bestLength || len(prefix) == bestLength && (preset.Priority > best.Priority || preset.Priority == best.Priority && preset.Name < best.Name) {
				best, bestLength = preset, len(prefix)
			}
		}
	}
	r.mu.RUnlock()
	if bestLength > 0 {
		return best.clone(), true
	}
	return r.ForFamily(family)
}

// 版本前缀只接受型号后缀，不把下一个小版本号当成套餐或日期。
// 例如 Opus 4 的提示可用于 4-20250514，不能据此分配给 Opus 4.6。
func matchesModelPrefix(model, prefix string) bool {
	if prefix == "" {
		return false
	}
	if model == prefix {
		return true
	}
	if !strings.HasPrefix(model, prefix+"-") {
		return false
	}
	last := prefix[strings.LastIndex(prefix, "-")+1:]
	// gpt6 这种紧凑写法也包含版本，不能把 gpt6.10 当成 gpt6 的后缀。
	last = strings.TrimPrefix(last, "gpt")
	if _, err := strconv.Atoi(last); err != nil {
		return true
	}
	next := strings.SplitN(strings.TrimPrefix(model, prefix+"-"), "-", 2)[0]
	if _, err := strconv.Atoi(next); err != nil {
		return true
	}
	// 合法日期快照是型号后缀；其他纯数字段按另一个版本处理。
	if len(next) == 8 {
		_, err := time.Parse("20060102", next)
		return err == nil
	}
	return false
}

// Families lists the families that have at least one auto-selectable preset.
func (r *Registry) Families() []Family {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Family, 0, len(r.byFamily))
	for family := range r.byFamily {
		out = append(out, family)
	}
	sort.Strings(out)
	return out
}

// normalizeFamily folds a family name for comparisons.
func normalizeFamily(family Family) Family {
	return Family(strings.ToLower(strings.TrimSpace(family)))
}

// SetDefault names the preset reported as the registry's nominal default.
//
// It is no longer a request-time fallback: selection is by model, and a model
// with no family preset gets nothing rather than this one. It is kept because
// the console shows which preset is "the" default when presets are listed, and
// because LoadDir uses it to record the first preset it loaded.
func (r *Registry) SetDefault(name string) error {
	name = normalizeName(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.presets[name]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownPreset, name)
	}
	r.defaultName = name
	return nil
}

// Default reports the nominal default preset name.
func (r *Registry) Default() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultName
}

// Lookup resolves a preset by name. An empty name resolves to the default.
func (r *Registry) Lookup(name string) (Preset, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	name = normalizeName(name)
	if name == "" {
		name = r.defaultName
	}
	if name == "" {
		return Preset{}, fmt.Errorf("%w: no presets are loaded", ErrUnknownPreset)
	}
	preset, ok := r.presets[name]
	if !ok {
		return Preset{}, fmt.Errorf("%w: %q", ErrUnknownPreset, name)
	}
	return preset.clone(), nil
}

// List returns the presets in a stable order.
func (r *Registry) List() []Preset {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Preset, 0, len(r.order))
	for _, name := range r.order {
		if preset, ok := r.presets[name]; ok {
			out = append(out, preset.clone())
		}
	}
	return out
}

// Names returns the preset names in a stable order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Empty reports whether no presets are loaded.
//
// It exists so the request path can skip the whole armor branch on a deployment
// that has none, keeping the common case free of the lookup.
func (r *Registry) Empty() bool {
	if r == nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.presets) == 0
}

// LoadDir loads every .md or .txt file in dir as a preset named after the file
// without its extension.
//
// Metadata comes from a leading front-matter block when present, so a preset can
// declare its target family without a separate registry file:
//
//	<!-- armor
//	family: deepseek
//	auto: true
//	priority: 10
//	-->
//	# Description line
//	(the prompt body)
//
// The front matter is STRIPPED from the body before it is sent, because the
// model must not receive gateway bookkeeping. A file with no front matter loads
// as a family-less preset, usable only by explicit name.
func (r *Registry) LoadDir(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// A missing preset directory is not an error: armor is optional.
			return 0, nil
		}
		return 0, fmt.Errorf("armor: read preset dir %s: %w", dir, err)
	}

	loaded := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".md" && ext != ".txt" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return loaded, fmt.Errorf("armor: read preset %s: %w", path, err)
		}

		meta, body := splitFrontMatter(string(raw))
		preset := Preset{
			Name:          name,
			Description:   firstHeading(body),
			Body:          body,
			File:          path,
			Family:        Family(meta["family"]),
			Priority:      parseIntOr(meta["priority"], 0),
			SourceURL:     meta["source_url"],
			SourceSHA256:  meta["source_sha256"],
			License:       meta["license"],
			Coverage:      meta["coverage"],
			SelectionNote: meta["selection_note"],
			ModelPrefixes: parseModelPrefixes(meta["model_prefixes"]),
		}
		if auto, ok := meta["auto"]; ok {
			value := parseBoolOr(auto, true)
			preset.AutoSelectable = &value
		}
		if description := strings.TrimSpace(meta["description"]); description != "" {
			preset.Description = description
		}
		for _, resource := range BundledResearchFiles(meta["source_id"]) {
			if resource.SHA256 == preset.SourceSHA256 {
				preset.LicenseText = resource.LicenseText
				break
			}
		}
		if err := r.Register(preset); err != nil {
			return loaded, err
		}
		loaded++
	}
	return loaded, nil
}

// splitFrontMatter separates an optional leading metadata block from the body.
//
// The block must be the FIRST thing in the file and is delimited by <!-- armor
// and -->. Anything else is treated as body, so a preset that happens to start
// with an unrelated HTML comment is not misparsed.
func splitFrontMatter(raw string) (map[string]string, string) {
	meta := map[string]string{}
	text := strings.ReplaceAll(raw, "\r\n", "\n")
	trimmed := strings.TrimLeft(text, "\n")

	const open = "<!-- armor"
	if !strings.HasPrefix(trimmed, open) {
		return meta, raw
	}
	end := strings.Index(trimmed, "-->")
	if end < 0 {
		// An unterminated block is not metadata; keep the file intact rather
		// than swallowing the whole prompt.
		return meta, raw
	}

	block := trimmed[len(open):end]
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		meta[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	// Keep a leading newline so the body starts cleanly.
	body := strings.TrimLeft(trimmed[end+len("-->"):], "\n")
	return meta, body
}

func parseModelPrefixes(raw string) []string {
	var prefixes []string
	for _, prefix := range strings.Split(raw, ",") {
		if prefix = strings.TrimSpace(prefix); prefix != "" {
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes
}

func parseIntOr(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return value
}

func parseBoolOr(raw string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "yes", "1", "on":
		return true
	case "false", "no", "0", "off":
		return false
	default:
		return fallback
	}
}

// firstHeading extracts a description from a preset body.
//
// A markdown heading or an HTML comment on the first meaningful line is used;
// otherwise the first line is taken as-is. This keeps a preset file
// self-documenting without requiring a separate metadata file.
func firstHeading(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "#")
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "<!--")
		line = strings.TrimSuffix(line, "-->")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 160 {
			line = line[:160] + "..."
		}
		return line
	}
	return ""
}

// normalizeName canonicalizes a preset name to a lowercase, hyphenated slug so
// a header value matches regardless of case or spacing.
func normalizeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, " ", "-")
	return name
}

// IsExplicitDisable reports whether the header is a deliberate "turn it off for
// this request" rather than an absent or unrecognised value.
//
// It exists so the request header can be a NARROWING control only. The global
// switch is a master switch: armor rewrites the system prompt, so a caller must
// not be able to force it on while the operator has it off. Turning it off for
// one request is always safe, because less armor cannot do something the
// operator did not enable.
//
// Note the asymmetry with the old ParseEnable, which this replaced: an ABSENT
// header is NOT a disable. It is "no opinion", so it leaves the operator's
// switch in charge. Treating it as a disable would unarmor every request from a
// client that simply did not send the header.
func IsExplicitDisable(raw string) bool {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "0", "false", "no", "off", "disable", "disabled":
		return true
	default:
		return false
	}
}

// ApplyOptions describes one substitution decision.
type ApplyOptions struct {
	// Enabled is the resolved opt-in. This is the whole on/off switch: when it
	// is false nothing is substituted, and when it is true the preset is chosen
	// from the model.
	Enabled bool
	// PresetName is an explicitly requested preset. Empty means "pick by model".
	// It exists so a caller can override the selection, not as the normal path.
	PresetName string
	// Model is the model the request asks for. It is what selects the preset:
	// a glm model gets the glm preset, a deepseek model the deepseek one.
	Model string
}

// Decision is the outcome of resolving the armor for a request.
type Decision struct {
	// Applied reports whether a substitution happened.
	Applied bool
	// Preset is the preset that was applied.
	Preset Preset
	// Source records how the preset was chosen: "explicit" or "model".
	Source string
	// Family is the family the model resolved to.
	Family Family
	// Reason explains a non-application, for logs.
	Reason string
}

// Preset selection sources.
const (
	// SourceExplicit means the caller named the preset.
	SourceExplicit = "explicit"
	// SourceModel means the preset was chosen from the requested model.
	SourceModel = "model"
	// SourceBinding 表示操作者为实际型号保存了明确的预设选择。
	SourceBinding = "binding"
)

// Resolve decides whether to substitute and with what.
//
// Selection order, most specific first:
//
//  1. An explicitly named preset. A caller who names one gets exactly it, and
//     an unknown name is an error rather than a silent fallback.
//  2. The preset for the requested model's family.
//
// When the model has no family preset, NOTHING is applied. Falling back to an
// arbitrary preset would send a prompt written for a different model, which is
// worse than sending none: it burns prompt tokens and can turn a working request
// into a refusal.
//
// Returning a Decision rather than an error for "not enabled" is deliberate: a
// caller that did not opt in is the normal path, not a failure.
func (r *Registry) Resolve(opts ApplyOptions) Decision {
	if !opts.Enabled {
		return Decision{Reason: "not enabled"}
	}
	if r == nil || r.Empty() {
		return Decision{Reason: "no presets loaded"}
	}

	// 1. An explicitly named preset wins outright.
	if name := strings.TrimSpace(opts.PresetName); name != "" {
		preset, err := r.Lookup(name)
		if err != nil {
			// No fallback: the caller asked for something specific, and
			// silently substituting a different prompt would be worse than
			// applying none.
			return Decision{Reason: err.Error()}
		}
		return Decision{Applied: true, Preset: preset, Source: SourceExplicit, Reason: "explicit preset"}
	}

	// 2. Selection by the requested model.
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		return Decision{Reason: "no model to select a preset for"}
	}
	family := ModelFamily(model)
	r.mu.RLock()
	bound, boundExists := r.modelBindings[canonicalModelName(model)]
	r.mu.RUnlock()
	if boundExists && bound == "off" {
		return Decision{Family: family, Reason: "disabled for this model by binding"}
	}
	preset, ok := r.ForModel(model)
	if !ok {
		// Report the family so an operator can see why nothing was applied.
		return Decision{Family: family, Reason: "no preset for family " + string(family)}
	}
	source := SourceModel
	if boundExists {
		source = SourceBinding
	}
	return Decision{
		Applied: true, Preset: preset,
		Source: source, Family: family,
		Reason: "selected for family " + string(family),
	}
}
