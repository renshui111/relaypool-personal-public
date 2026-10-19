package armor

import "testing"

// The matcher must recognise the model IDs a real pool actually serves. The
// list below is taken from a live WorkBuddy catalogue, so a regression here
// means auto-selection would stop working for models that are actually used.
func TestModelFamilyRealPoolModels(t *testing.T) {
	cases := map[string]Family{
		// The live catalogue's Hunyuan models carry no recognisable prefix.
		"hy3":           FamilyHunyuan,
		"hy3-x":         FamilyHunyuan,
		"hy4-preview":   FamilyHunyuan,
		"hy4-preview-x": FamilyHunyuan,

		"deepseek-v4.1-flash": FamilyDeepSeek,
		"deepseek-v4-pro":     FamilyDeepSeek,

		"glm-5.3":       FamilyGLM,
		"glm-5.3-flash": FamilyGLM,
		"glm-5.2":       FamilyGLM,
		"glm-5.1":       FamilyGLM,
		"glm-5v-turbo":  FamilyGLM,

		"kimi-k3-1":         FamilyKimi,
		"kimi-k2.8-preview": FamilyKimi,
		"kimi-k2.7":         FamilyKimi,
		"kimi-k2.6":         FamilyKimi,

		"minimax-m3": FamilyMiniMax,

		// "auto" is the upstream's router: it has no family, and guessing one
		// would apply a preset written for a different model.
		"auto": FamilyGeneric,
	}
	for model, want := range cases {
		if got := ModelFamily(model); got != want {
			t.Errorf("ModelFamily(%q)=%q, want %q", model, got, want)
		}
	}
}

func TestModelFamilyOtherVendors(t *testing.T) {
	cases := map[string]Family{
		"claude-sonnet-4-6":        FamilyClaude,
		"claude-3-5-sonnet-latest": FamilyClaude,
		"sonnet-4.6":               FamilyClaude,
		"claude-opus-4":            FamilyClaude,
		"gpt-5":                    FamilyGPT,
		"gpt-4o":                   FamilyGPT,
		"chatgpt-4o-latest":        FamilyGPT,
		"o1-mini":                  FamilyGPT,
		"o3":                       FamilyGPT,
		"gemini-2.5-pro":           FamilyGemini,
		"gemini-3.5-flash":         FamilyGemini,
		"grok-4":                   FamilyGrok,
		"qwen3-max":                FamilyQwen,
		"qwen-max":                 FamilyQwen,
		"moonshot-v1-128k":         FamilyKimi,
		"chatglm3":                 FamilyGLM,
		"hunyuan-turbo":            FamilyHunyuan,
	}
	for model, want := range cases {
		if got := ModelFamily(model); got != want {
			t.Errorf("ModelFamily(%q)=%q, want %q", model, got, want)
		}
	}
}

// Case, separators and a vendor prefix must all fold away, because every client
// spells model IDs differently.
func TestModelFamilyFoldsSpelling(t *testing.T) {
	want := FamilyDeepSeek
	for _, variant := range []string{
		"deepseek-v4",
		"DeepSeek-V4",
		"DEEPSEEK_V4",
		"deepseek.v4",
		"deepseek v4",
		"workbuddy/deepseek-v4",
		"  deepseek-v4  ",
	} {
		if got := ModelFamily(variant); got != want {
			t.Errorf("ModelFamily(%q)=%q, want %q", variant, got, want)
		}
	}
}

// An unknown model must report generic rather than being forced into a family.
// A wrong guess silently sends a preset written for a different model.
func TestModelFamilyUnknownIsGeneric(t *testing.T) {
	for _, model := range []string{"", "some-new-model-xyz", "internal-v2", "foo"} {
		if got := ModelFamily(model); got != FamilyGeneric {
			t.Errorf("ModelFamily(%q)=%q, want %q", model, got, FamilyGeneric)
		}
	}
}

// A bare "o1" substring must not match unrelated names: the o-series rule
// requires a token boundary precisely to avoid this.
func TestOSeriesRequiresTokenBoundary(t *testing.T) {
	if got := ModelFamily("proto1"); got == FamilyGPT {
		t.Errorf("proto1 must not be classified as GPT")
	}
	if got := ModelFamily("o1"); got != FamilyGPT {
		t.Errorf("o1 should be GPT, got %q", got)
	}
	if got := ModelFamily("o3-mini"); got != FamilyGPT {
		t.Errorf("o3-mini should be GPT, got %q", got)
	}
}

// A more specific family must win over a broader pattern.
func TestFamilyRuleOrdering(t *testing.T) {
	// A DeepSeek model whose name also contains a GLM token must stay DeepSeek,
	// since the DeepSeek rule is evaluated first.
	if got := ModelFamily("deepseek-v4-glm-edition"); got != FamilyDeepSeek {
		t.Errorf("got %q, want deepseek (first matching rule wins)", got)
	}
}

// ---- auto-selection ----

func boolPtr(v bool) *bool { return &v }

func TestForModelSelectsByFamily(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "ds", Family: FamilyDeepSeek, Body: "deepseek prompt"})
	mustRegister(t, r, Preset{Name: "gl", Family: FamilyGLM, Body: "glm prompt"})

	preset, ok := r.ForModel("deepseek-v4-pro")
	if !ok || preset.Name != "ds" {
		t.Fatalf("deepseek-v4-pro should select ds, got %q ok=%v", preset.Name, ok)
	}
	preset, ok = r.ForModel("glm-5.3")
	if !ok || preset.Name != "gl" {
		t.Fatalf("glm-5.3 should select gl, got %q ok=%v", preset.Name, ok)
	}
}

// A family with no preset must report not-found rather than falling back, so
// the caller can decide what to do.
func TestForModelUnknownFamily(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "ds", Family: FamilyDeepSeek, Body: "x"})

	if _, ok := r.ForModel("minimax-m3"); ok {
		t.Fatalf("minimax has no preset and must not resolve")
	}
	if _, ok := r.ForModel("auto"); ok {
		t.Fatalf("the router pseudo-model must not select a family preset")
	}
}

// With several presets for one family, the higher priority wins, and the result
// must not depend on load order.
func TestPriorityBreaksTies(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "low", Family: FamilyGLM, Body: "low", Priority: 1})
	mustRegister(t, r, Preset{Name: "high", Family: FamilyGLM, Body: "high", Priority: 99})
	mustRegister(t, r, Preset{Name: "mid", Family: FamilyGLM, Body: "mid", Priority: 50})

	preset, ok := r.ForModel("glm-5.3")
	if !ok {
		t.Fatalf("expected a preset")
	}
	if preset.Name != "high" {
		t.Errorf("selected %q, want high (priority 99)", preset.Name)
	}
}

// Equal priority falls back to name order, so selection is deterministic.
func TestEqualPriorityIsDeterministic(t *testing.T) {
	build := func() *Registry {
		r := NewRegistry()
		mustRegister(t, r, Preset{Name: "zulu", Family: FamilyGLM, Body: "z", Priority: 5})
		mustRegister(t, r, Preset{Name: "alpha", Family: FamilyGLM, Body: "a", Priority: 5})
		return r
	}
	for i := 0; i < 5; i++ {
		preset, _ := build().ForModel("glm-5.3")
		if preset.Name != "alpha" {
			t.Fatalf("run %d selected %q, want alpha (stable alphabetical tie-break)", i, preset.Name)
		}
	}
}

// A preset can opt out of auto-selection while staying usable by name.
func TestAutoSelectableOptOut(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "manual", Family: FamilyGLM, Body: "x", AutoSelectable: boolPtr(false)})

	if _, ok := r.ForModel("glm-5.3"); ok {
		t.Fatalf("an auto:false preset must not be chosen automatically")
	}
	// But it is still reachable explicitly.
	if _, err := r.Lookup("manual"); err != nil {
		t.Errorf("an auto:false preset must remain usable by name: %v", err)
	}
}

// A preset with no family is never auto-selected, since there is no family to
// match it against.
func TestFamilylessPresetIsNotAutoSelectable(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "orphan", Body: "x"})

	if _, ok := r.ForModel("deepseek-v4"); ok {
		t.Fatalf("a family-less preset must not be auto-selected")
	}
	if _, err := r.Lookup("orphan"); err != nil {
		t.Errorf("it should still be usable by name: %v", err)
	}
}

func TestFamiliesListing(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "a", Family: FamilyDeepSeek, Body: "x"})
	mustRegister(t, r, Preset{Name: "b", Family: FamilyGLM, Body: "x"})
	mustRegister(t, r, Preset{Name: "c", Body: "x"}) // family-less, excluded

	families := r.Families()
	if len(families) != 2 {
		t.Fatalf("families=%v, want 2", families)
	}
	if families[0] != FamilyDeepSeek || families[1] != FamilyGLM {
		t.Errorf("families=%v, want sorted [deepseek glm]", families)
	}
}

// Replacing a preset whose family changed must move it in the index, not leave
// it reachable under the old family.
func TestReindexOnFamilyChange(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "p", Family: FamilyDeepSeek, Body: "x"})
	if _, ok := r.ForModel("deepseek-v4"); !ok {
		t.Fatalf("should resolve under deepseek")
	}

	mustRegister(t, r, Preset{Name: "p", Family: FamilyGLM, Body: "x"})

	if _, ok := r.ForModel("deepseek-v4"); ok {
		t.Errorf("the stale deepseek index entry was not removed")
	}
	if _, ok := r.ForModel("glm-5.3"); !ok {
		t.Errorf("the preset was not indexed under its new family")
	}
}

// ---- resolve: selection is by requested model ----

// The model the request asks for decides the preset.
func TestResolveSelectsByModel(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "ds", Family: FamilyDeepSeek, Body: "deepseek prompt"})

	d := r.Resolve(ApplyOptions{Enabled: true, Model: "deepseek-v4-pro"})
	if !d.Applied {
		t.Fatalf("expected model-based selection to apply: %s", d.Reason)
	}
	if d.Preset.Name != "ds" {
		t.Errorf("preset=%q, want ds", d.Preset.Name)
	}
	if d.Source != SourceModel {
		t.Errorf("source=%q, want %q", d.Source, SourceModel)
	}
	if d.Family != FamilyDeepSeek {
		t.Errorf("family=%q, want deepseek", d.Family)
	}
}

// Different models must receive different presets from the same registry, which
// is the whole point of selecting by model rather than by account.
func TestDifferentModelsGetDifferentPresets(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "ds", Family: FamilyDeepSeek, Body: "ds"})
	mustRegister(t, r, Preset{Name: "glm", Family: FamilyGLM, Body: "glm"})

	for model, want := range map[string]string{
		"deepseek-v4-pro": "ds",
		"deepseek-r1":     "ds",
		"glm-5.3":         "glm",
		"glm-4-plus":      "glm",
	} {
		d := r.Resolve(ApplyOptions{Enabled: true, Model: model})
		if !d.Applied || d.Preset.Name != want {
			t.Errorf("model %q -> preset %q (applied=%v), want %q", model, d.Preset.Name, d.Applied, want)
		}
	}
}

// An explicitly named preset still wins over model selection.
func TestExplicitBeatsModelSelection(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "ds", Family: FamilyDeepSeek, Body: "auto"})
	mustRegister(t, r, Preset{Name: "special", Body: "explicit"})

	d := r.Resolve(ApplyOptions{
		Enabled: true, Model: "deepseek-v4", PresetName: "special",
	})
	if d.Preset.Name != "special" {
		t.Errorf("preset=%q, want special", d.Preset.Name)
	}
	if d.Source != SourceExplicit {
		t.Errorf("source=%q, want %q", d.Source, SourceExplicit)
	}
}

// The enable switch is the whole on/off control: off means nothing is sent, even
// when a matching preset exists.
func TestDisabledAppliesNothing(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "ds", Family: FamilyDeepSeek, Body: "x"})

	d := r.Resolve(ApplyOptions{Enabled: false, Model: "deepseek-v4"})
	if d.Applied {
		t.Errorf("armor must not apply when it is switched off: %+v", d)
	}
	if d.Reason != "not enabled" {
		t.Errorf("reason=%q, want %q", d.Reason, "not enabled")
	}
}

// A model with no family preset gets NOTHING.
//
// This is the deliberate difference from the old fallback behaviour: falling
// back to an unrelated preset would send a prompt written for a different model,
// which burns tokens and can turn a working request into a refusal.
func TestUnknownModelAppliesNothing(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "ds", Family: FamilyDeepSeek, Body: "x"})
	mustRegister(t, r, Preset{Name: "fallback", Body: "y"})
	if err := r.SetDefault("fallback"); err != nil {
		t.Fatalf("set default: %v", err)
	}

	d := r.Resolve(ApplyOptions{Enabled: true, Model: "minimax-m3"})
	if d.Applied {
		t.Errorf("an unrecognised model must not receive an unrelated preset, got %q", d.Preset.Name)
	}
	// minimax resolves to its OWN family, which simply has no preset. That is
	// the meaningful part: the negative answer is family-specific, not a
	// catch-all failure.
	if d.Family != FamilyMiniMax {
		t.Errorf("family=%q, want minimax", d.Family)
	}
	if d.Reason == "" {
		t.Errorf("a non-application must carry a reason for the log")
	}
}

// A model the taxonomy cannot place at all resolves to generic, and gets nothing.
func TestUnclassifiableModelAppliesNothing(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "ds", Family: FamilyDeepSeek, Body: "x"})

	d := r.Resolve(ApplyOptions{Enabled: true, Model: "some-unknown-model-xyz"})
	if d.Applied {
		t.Errorf("an unclassifiable model must not receive a preset, got %q", d.Preset.Name)
	}
	if d.Family != FamilyGeneric {
		t.Errorf("family=%q, want generic", d.Family)
	}
}

// An empty model cannot select anything, and must say so rather than picking
// some arbitrary preset.
func TestEmptyModelAppliesNothing(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "ds", Family: FamilyDeepSeek, Body: "x"})

	d := r.Resolve(ApplyOptions{Enabled: true, Model: "  "})
	if d.Applied {
		t.Errorf("an empty model must not select a preset, got %q", d.Preset.Name)
	}
}

// A model whose family has no preset must not borrow another family's.
func TestFamilyWithoutPresetDoesNotBorrow(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "glm", Family: FamilyGLM, Body: "glm"})

	// minimax resolves to its own family, which has no preset.
	d := r.Resolve(ApplyOptions{Enabled: true, Model: "minimax-m3"})
	if d.Applied {
		t.Errorf("minimax must not receive the glm preset, got %q", d.Preset.Name)
	}
}

// A vendor-prefixed model ID must still select by family.
func TestVendorPrefixedModelSelectsByFamily(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, Preset{Name: "ds", Family: FamilyDeepSeek, Body: "x"})

	d := r.Resolve(ApplyOptions{Enabled: true, Model: "workbuddy/deepseek-v4-pro"})
	if !d.Applied || d.Preset.Name != "ds" {
		t.Errorf("a prefixed model should still select its family preset: applied=%v preset=%q",
			d.Applied, d.Preset.Name)
	}
}

func mustRegister(t *testing.T, r *Registry, preset Preset) {
	t.Helper()
	if err := r.Register(preset); err != nil {
		t.Fatalf("register %s: %v", preset.Name, err)
	}
}
