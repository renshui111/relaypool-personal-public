package armor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every shipped preset must declare a model family.
//
// WHY THIS IS A TEST AND NOT A REVIEW NOTE: a preset with no family lands in a
// "no family" bucket in the console, which reads as a broken category rather
// than as a preset nobody can auto-select. Worse, the way to create one is easy
// to do by accident: the metadata block must be the FIRST thing in the file, so
// putting a markdown heading above it silently drops the whole block and the
// preset becomes familyless.
//
// That exact mistake was made while fixing this: `infinite-gen-4.md` had its
// `<!-- armor ... -->` block placed after the `# heading`, the loader ignored it,
// and the preset still showed as "no family" after the fix looked applied.
func TestEveryShippedPresetDeclaresAFamily(t *testing.T) {
	dir := filepath.Join("..", "..", "prompts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	reg := NewRegistry()
	if _, err := reg.LoadDir(dir); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	var checked int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		checked++
		name := strings.TrimSuffix(e.Name(), ".md")

		preset, err := reg.Lookup(name)
		if err != nil {
			t.Errorf("%s: not loaded by the registry: %v", e.Name(), err)
			continue
		}

		if strings.TrimSpace(string(preset.Family)) == "" {
			raw, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			head := string(raw)
			if len(head) > 120 {
				head = head[:120]
			}
			t.Errorf("%s has NO family, so it can never be auto-selected and shows up "+
				"as a 'no family' bucket in the console.\n"+
				"  A common cause: the `<!-- armor ... -->` block is not the FIRST thing in "+
				"the file (the loader ignores it otherwise).\n"+
				"  File starts with: %q", e.Name(), head)
		}
	}

	if checked == 0 {
		if DistributionProfile == "public" {
			return
		}
		t.Fatal("no presets found; this test would pass vacuously")
	}
	t.Logf("checked %d shipped presets", checked)
}

// The metadata block must be first in the file, which is the mechanism behind
// the failure above. This pins it directly so the error message is specific.
func TestPresetMetadataMustBeFirstInTheFile(t *testing.T) {
	dir := filepath.Join("..", "..", "prompts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		text := strings.TrimLeft(string(raw), "\ufeff \t\r\n")
		if !strings.HasPrefix(text, "<!-- armor") {
			t.Errorf("%s does not start with the `<!-- armor` metadata block. The loader "+
				"only reads a block at the very start, so any heading or text above it "+
				"silently discards the whole block.", e.Name())
		}
	}
}
