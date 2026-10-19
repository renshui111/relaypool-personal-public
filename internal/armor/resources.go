package armor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"
	"unicode/utf8"
)

var libraryOnce sync.Once
var researchLibrary []ResearchFile

type ResearchFile struct {
	SourceID       string `json:"source_id"`
	Path           string `json:"path"`
	Family         string `json:"family"`
	Body           string `json:"body,omitempty"`
	SHA256         string `json:"sha256"`
	License        string `json:"license"`
	LicenseText    string `json:"license_text,omitempty"`
	ArchiveSHA256  string `json:"archive_sha256,omitempty"`
	SourceRevision string `json:"source_revision,omitempty"`
}

func BundledResearchFiles(source string) []ResearchFile {
	libraryOnce.Do(func() { _ = json.Unmarshal(mustBundledRead("assets/library.json"), &researchLibrary) })
	out := make([]ResearchFile, 0)
	for _, file := range researchLibrary {
		if file.SourceID == source {
			out = append(out, file)
		}
	}
	return out
}
func mustBundledRead(name string) []byte { raw, _ := bundledResources.ReadFile(name); return raw }

func (r *Registry) LoadBundledPresets() (int, error) {
	if bundledPresetPattern == "" {
		return 0, nil
	}
	files, err := fs.Glob(bundledResources, bundledPresetPattern)
	if err != nil {
		return 0, err
	}
	for i, file := range files {
		raw, err := bundledResources.ReadFile(file)
		if err != nil {
			return i, err
		}
		meta, body := splitFrontMatter(string(raw))
		selectable := parseBoolOr(meta["auto"], true)
		preset := Preset{Name: path.Base(file[:len(file)-3]), Body: body, File: "内置/" + path.Base(file), Description: meta["description"], Family: meta["family"], Priority: parseIntOr(meta["priority"], 0), AutoSelectable: &selectable}
		preset.SourceURL, preset.SourceSHA256, preset.License = meta["source_url"], meta["source_sha256"], meta["license"]
		preset.Coverage, preset.SelectionNote = meta["coverage"], meta["selection_note"]
		preset.ModelPrefixes = parseModelPrefixes(meta["model_prefixes"])
		for _, resource := range BundledResearchFiles(meta["source_id"]) {
			if resource.SHA256 == preset.SourceSHA256 {
				preset.LicenseText = resource.LicenseText
				break
			}
		}
		if err := r.Register(preset); err != nil {
			return i, err
		}
	}
	return len(files), nil
}

func TextSHA256(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func ValidateImport(preset Preset) error {
	if len(preset.Name) > 80 || preset.Name == "off" || preset.Name == "" {
		return fmt.Errorf("预设名称须为 1 至 80 字节，不能使用 off")
	}
	for _, c := range preset.Name {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return fmt.Errorf("预设名称只允许小写字母、数字、连字符与下划线")
		}
	}
	if len(preset.Body) > 1<<20 || len(preset.Body) == 0 {
		return fmt.Errorf("预设正文不能为空，且不能超过 1 MB")
	}
	if !utf8.ValidString(preset.Body) || strings.ContainsRune(preset.Body, 0) {
		return fmt.Errorf("预设须为有效文本，不能包含空字节")
	}
	if len(preset.Family) > 32 || len(preset.Description) > 512 {
		return fmt.Errorf("预设模型族或说明过长")
	}
	return nil
}
