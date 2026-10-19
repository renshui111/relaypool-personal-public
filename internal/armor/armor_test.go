package armor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newRegistryWith(t *testing.T, presets ...Preset) *Registry {
	t.Helper()
	r := NewRegistry()
	for _, preset := range presets {
		if err := r.Register(preset); err != nil {
			t.Fatalf("register %s: %v", preset.Name, err)
		}
	}
	return r
}

func TestRegisterAndLookup(t *testing.T) {
	r := newRegistryWith(t, Preset{Name: "alpha", Body: "you are alpha"})

	preset, err := r.Lookup("alpha")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if preset.Body != "you are alpha" {
		t.Errorf("body=%q", preset.Body)
	}
}

// Names are slugged so a header value matches regardless of case or spacing.
func TestLookupNormalizesName(t *testing.T) {
	r := newRegistryWith(t, Preset{Name: "infinite-gen-4", Body: "x"})

	for _, variant := range []string{"infinite-gen-4", "Infinite-Gen-4", "infinite_gen_4", " INFINITE-GEN-4 "} {
		if _, err := r.Lookup(variant); err != nil {
			t.Errorf("Lookup(%q) failed: %v", variant, err)
		}
	}
}

func TestLookupUnknownFails(t *testing.T) {
	r := newRegistryWith(t, Preset{Name: "alpha", Body: "x"})
	if _, err := r.Lookup("nope"); err == nil {
		t.Fatalf("an unknown preset must fail")
	}
}

// An empty name resolves to the default, so a caller can enable the armor
// without having to know which preset is default.
func TestLookupEmptyUsesDefault(t *testing.T) {
	r := newRegistryWith(t,
		Preset{Name: "alpha", Body: "first"},
		Preset{Name: "beta", Body: "second"},
	)
	preset, err := r.Lookup("")
	if err != nil {
		t.Fatalf("lookup default: %v", err)
	}
	// The first registered preset becomes the default.
	if preset.Name != "alpha" {
		t.Errorf("default=%q, want alpha", preset.Name)
	}
}

func TestSetDefault(t *testing.T) {
	r := newRegistryWith(t,
		Preset{Name: "alpha", Body: "first"},
		Preset{Name: "beta", Body: "second"},
	)
	if err := r.SetDefault("beta"); err != nil {
		t.Fatalf("set default: %v", err)
	}
	if r.Default() != "beta" {
		t.Errorf("default=%q, want beta", r.Default())
	}
	preset, _ := r.Lookup("")
	if preset.Name != "beta" {
		t.Errorf("empty lookup resolved to %q, want beta", preset.Name)
	}
}

func TestSetDefaultRejectsUnknown(t *testing.T) {
	r := newRegistryWith(t, Preset{Name: "alpha", Body: "x"})
	if err := r.SetDefault("missing"); err == nil {
		t.Fatalf("setting an unknown default must fail")
	}
}

func TestRegisterRejectsEmptyBody(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Preset{Name: "empty", Body: "   "}); err == nil {
		t.Fatalf("an empty body must be rejected: a preset that sends nothing is worse than none")
	}
}

func TestRegisterRejectsEmptyName(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Preset{Body: "x"}); err == nil {
		t.Fatalf("an empty name must be rejected")
	}
}

// ---- disable-header parsing ----

// An absent header is NOT a disable: "no opinion" must leave the operator's
// switch in charge. This is the single most important property of the header
// logic, because treating an absent header as "off" would make every request
// unarmored whenever a client simply did not send it.
func TestIsExplicitDisableRequiresAnExplicitNo(t *testing.T) {
	for _, value := range []string{"", " ", "\t", "1", "true", "yes", "on", "infinite-gen-4", "anything"} {
		if IsExplicitDisable(value) {
			t.Errorf("IsExplicitDisable(%q) = true; only an explicit no may disable, "+
				"because an absent or unrecognised value means \"no opinion\"", value)
		}
	}
}

func TestIsExplicitDisableAcceptsTheNoSpellings(t *testing.T) {
	for _, value := range []string{"0", "false", "no", "off", "disable", "disabled", "FALSE", " No "} {
		if !IsExplicitDisable(value) {
			t.Errorf("IsExplicitDisable(%q) = false, but it is an explicit no", value)
		}
	}
}

// ---- resolve ----

func TestResolveNotEnabled(t *testing.T) {
	r := newRegistryWith(t, Preset{Name: "alpha", Body: "x"})

	d := r.Resolve(ApplyOptions{Enabled: false})
	if d.Applied {
		t.Fatalf("a request without the opt-in must not apply the armor")
	}
	if d.Reason == "" {
		t.Errorf("a non-application must carry a reason for the log")
	}
}

func TestResolveApplied(t *testing.T) {
	r := newRegistryWith(t, Preset{Name: "alpha", Body: "you are alpha"})

	d := r.Resolve(ApplyOptions{Enabled: true, PresetName: "alpha"})
	if !d.Applied {
		t.Fatalf("expected the armor to apply: %s", d.Reason)
	}
	if d.Preset.Name != "alpha" || d.Preset.Body != "you are alpha" {
		t.Errorf("wrong preset: %+v", d.Preset)
	}
}

// There is no account-level opt-in any more; the enable switch plus the model
// is the whole configuration. This guards the removal.
func TestResolveHasNoAccountOptInPath(t *testing.T) {
	r := newRegistryWith(t, Preset{Name: "alpha", Body: "x"})

	// Disabled means disabled, regardless of any other field on the options.
	d := r.Resolve(ApplyOptions{Enabled: false, PresetName: "alpha", Model: "glm-5.3"})
	if d.Applied {
		t.Errorf("a disabled request must not apply a preset: %+v", d)
	}

	// Enabled selects by model, so a preset with no family applies nothing.
	d2 := r.Resolve(ApplyOptions{Enabled: true, PresetName: "", Model: "glm-5.3"})
	if d2.Applied {
		t.Errorf("a family-less preset must not be auto-applied, got %q", d2.Preset.Name)
	}
}

// A request naming an unknown preset must not silently fall back to the
// default: the caller asked for something specific and deserves to know it is
// unavailable.
func TestResolveUnknownPresetDoesNotFallBack(t *testing.T) {
	r := newRegistryWith(t, Preset{Name: "alpha", Body: "x"})

	d := r.Resolve(ApplyOptions{Enabled: true, PresetName: "typo"})
	if d.Applied {
		t.Fatalf("an unknown preset must not fall back to another one")
	}
	if !strings.Contains(d.Reason, "typo") {
		t.Errorf("the reason should name the missing preset: %q", d.Reason)
	}
}

// An empty registry short-circuits, so a deployment without presets pays
// nothing on the request path.
func TestResolveWithEmptyRegistry(t *testing.T) {
	r := NewRegistry()
	if !r.Empty() {
		t.Fatalf("a fresh registry should be empty")
	}
	d := r.Resolve(ApplyOptions{Enabled: true})
	if d.Applied {
		t.Fatalf("an empty registry cannot apply anything")
	}
}

// ---- directory loading ----

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "alpha.md"), "# Alpha preset\n\nYou are alpha.")
	writeFile(t, filepath.Join(dir, "beta.txt"), "Beta preset\n\nYou are beta.")
	// Non-preset files must be ignored.
	writeFile(t, filepath.Join(dir, "notes.json"), `{"ignored":true}`)

	r := NewRegistry()
	loaded, err := r.LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded != 2 {
		t.Fatalf("loaded=%d, want 2 (the .json must be ignored)", loaded)
	}

	alpha, err := r.Lookup("alpha")
	if err != nil {
		t.Fatalf("lookup alpha: %v", err)
	}
	// The first heading becomes the description, so a preset file documents
	// itself for the console.
	if alpha.Description != "Alpha preset" {
		t.Errorf("description=%q, want %q", alpha.Description, "Alpha preset")
	}
	if !strings.Contains(alpha.Body, "You are alpha.") {
		t.Errorf("body=%q", alpha.Body)
	}
	if alpha.File == "" {
		t.Errorf("the source file should be recorded for diagnostics")
	}

	beta, err := r.Lookup("beta")
	if err != nil {
		t.Fatalf("lookup beta: %v", err)
	}
	if beta.Description != "Beta preset" {
		t.Errorf("beta description=%q", beta.Description)
	}
}

// A missing directory is not an error: armor is optional, and refusing to start
// because a preset directory is absent would take the pool offline for a
// cosmetic reason.
func TestLoadDirMissingIsNotAnError(t *testing.T) {
	r := NewRegistry()
	loaded, err := r.LoadDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("a missing directory must not be an error: %v", err)
	}
	if loaded != 0 {
		t.Errorf("loaded=%d, want 0", loaded)
	}
}

func TestLoadDirEmptyIsNotAnError(t *testing.T) {
	r := NewRegistry()
	loaded, err := r.LoadDir(t.TempDir())
	if err != nil {
		t.Fatalf("an empty directory must not be an error: %v", err)
	}
	if loaded != 0 {
		t.Errorf("loaded=%d, want 0", loaded)
	}
}

// The bundled preset must actually load, since a deployment ships it.
func TestLoadBundledPreset(t *testing.T) {
	if DistributionProfile == "public" {
		r := NewRegistry()
		if n, err := r.LoadBundledPresets(); err != nil || n != 0 || len(BundledResearchFiles("coldbrew")) != 0 {
			t.Fatal("公开核心含有未批准的研究正文")
		}
		return
	}
	// The preset lives at the repository root, two levels up from this package.
	dir := filepath.Join("..", "..", "prompts")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skipf("no bundled preset directory at %s", dir)
	}
	r := NewRegistry()
	loaded, err := r.LoadDir(dir)
	if err != nil {
		t.Fatalf("load bundled presets: %v", err)
	}
	if loaded == 0 {
		t.Fatalf("the bundled prompts directory has no loadable presets")
	}

	preset, err := r.Lookup("deepseek-curated")
	if err != nil {
		t.Fatalf("精选 DeepSeek 预设未加载：%v", err)
	}
	// A few strings that must survive the file round trip intact, so a botched
	// encoding edit is caught here rather than in production.
	for _, want := range []string{
		"DeepSeek V4.1",
		"思考槽",
		"不要把票根换成别的题目",
	} {
		if !strings.Contains(preset.Body, want) {
			t.Errorf("the preset body is missing %q", want)
		}
	}
}

func TestNamesAreSorted(t *testing.T) {
	r := newRegistryWith(t,
		Preset{Name: "zulu", Body: "z"},
		Preset{Name: "alpha", Body: "a"},
		Preset{Name: "mike", Body: "m"},
	)
	names := r.Names()
	want := []string{"alpha", "mike", "zulu"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names=%v, want %v", names, want)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
