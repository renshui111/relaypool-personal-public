package api

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/relaypool/relaypool/internal/armor"
)

const armorPackFormat = "relaypool.armor-pack"

// 预设包只承载文本和匹配规则；没有安装脚本、可执行文件或远程下载入口。
type armorPackPreset struct {
	Name          string   `json:"name"`
	Body          string   `json:"body"`
	BodySHA256    string   `json:"body_sha256"`
	Family        string   `json:"family"`
	Description   string   `json:"description,omitempty"`
	Auto          bool     `json:"auto_selectable"`
	Priority      int      `json:"priority,omitempty"`
	ModelPrefixes []string `json:"model_prefixes,omitempty"`
	SourceURL     string   `json:"source_url,omitempty"`
	SourceSHA256  string   `json:"source_sha256,omitempty"`
	License       string   `json:"license,omitempty"`
	LicenseText   string   `json:"license_text,omitempty"`
	Coverage      string   `json:"coverage,omitempty"`
	SelectionNote string   `json:"selection_note,omitempty"`
}

type armorPack struct {
	Format      string            `json:"format"`
	Version     int               `json:"version"`
	Presets     []armorPackPreset `json:"presets"`
	Bindings    map[string]string `json:"bindings"`
	StateSHA256 string            `json:"state_sha256,omitempty"`
}

func exportArmorPack(state armorCustomization) armorPack {
	p := armorPack{Format: armorPackFormat, Version: 1, Presets: []armorPackPreset{}, Bindings: state.Bindings}
	for _, v := range state.Presets {
		p.Presets = append(p.Presets, armorPackPreset{v.Name, v.Body, armor.TextSHA256(v.Body), v.Family, v.Description, v.IsAutoSelectable(), v.Priority, v.ModelPrefixes, v.SourceURL, v.SourceSHA256, v.License, v.LicenseText, v.Coverage, v.SelectionNote})
	}
	sort.Slice(p.Presets, func(i, j int) bool { return p.Presets[i].Name < p.Presets[j].Name })
	raw, _ := json.Marshal(p)
	p.StateSHA256 = armor.TextSHA256(string(raw))
	return p
}

func (s *Server) handleExportArmorPack(w http.ResponseWriter, r *http.Request) {
	s.armorMu.Lock()
	defer s.armorMu.Unlock()
	if s.armorConfigError != "" {
		writeError(w, 409, "configuration_error", s.armorConfigError)
		return
	}
	p := exportArmorPack(s.customizationCopy())
	w.Header().Set("ETag", `"`+p.StateSHA256+`"`)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, p)
}

func decodeArmorPack(w http.ResponseWriter, r *http.Request) (armorPack, error) {
	var p armorPack
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, fmt.Errorf("预设包 JSON 无效、包含不支持的字段或超过 10 MB")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return p, fmt.Errorf("预设包只能包含一个 JSON 对象")
	}
	if p.Format != armorPackFormat || p.Version != 1 {
		return p, fmt.Errorf("不支持的预设包格式或版本")
	}
	if len(p.Presets) > 64 || len(p.Bindings) > 256 {
		return p, fmt.Errorf("预设包最多包含 64 个预设和 256 个绑定")
	}
	return p, nil
}

func packPreset(v armorPackPreset) (armor.Preset, error) {
	p := armor.Preset{Name: v.Name, Body: v.Body, Family: v.Family, Description: v.Description, AutoSelectable: &v.Auto, Priority: v.Priority, ModelPrefixes: v.ModelPrefixes, SourceURL: v.SourceURL, SourceSHA256: v.SourceSHA256, License: v.License, LicenseText: v.LicenseText, Coverage: v.Coverage, SelectionNote: v.SelectionNote, File: "用户预设包"}
	if err := armor.ValidateImport(p); err != nil {
		return p, err
	}
	if p.Name != armor.PresetNameKey(p.Name) || strings.TrimSpace(p.Body) == "" || v.BodySHA256 != armor.TextSHA256(p.Body) {
		return p, fmt.Errorf("预设 %s 的名称不规范、正文为空或正文哈希不一致", p.Name)
	}
	if p.Priority < -10000 || p.Priority > 10000 || len(p.ModelPrefixes) > 16 || len(p.SourceURL) > 2048 || len(p.License) > 512 || len(p.LicenseText) > 64<<10 || len(p.Coverage) > 1024 || len(p.SelectionNote) > 2048 {
		return p, fmt.Errorf("预设 %s 的规则或说明超过限制", p.Name)
	}
	if p.SourceSHA256 != "" {
		if b, err := hex.DecodeString(p.SourceSHA256); err != nil || len(b) != 32 {
			return p, fmt.Errorf("预设 %s 的来源哈希无效", p.Name)
		}
	}
	known := map[string]bool{"gpt": true, "claude": true, "deepseek": true, "gemini": true, "glm": true, "kimi": true, "qwen": true, "grok": true, "llama": true, "doubao": true, "hunyuan": true, "minimax": true, "mimo": true, "gemma": true, "palm": true}
	if p.IsAutoSelectable() && !known[p.Family] {
		return p, fmt.Errorf("自动预设 %s 必须声明已支持的模型系列", p.Name)
	}
	seen := map[string]bool{}
	for _, prefix := range p.ModelPrefixes {
		key := armor.ModelBindingKey(prefix)
		if key == "" || len(prefix) > 256 || seen[key] || armor.ModelFamily(prefix) != p.Family {
			return p, fmt.Errorf("预设 %s 的版本前缀为空、重复或不属于声明的模型系列", p.Name)
		}
		seen[key] = true
	}
	return p, nil
}

// prepareArmorPack 在写数据库之前完成全部校验；合并不覆盖同名用户预设。
func (s *Server) prepareArmorPack(p armorPack, mode string) (armorCustomization, error) {
	state := s.customizationCopy()
	if mode == "replace" {
		state = armorCustomization{Presets: map[string]armor.Preset{}, Bindings: map[string]string{}}
	} else if mode != "merge" {
		return state, fmt.Errorf("导入方式只能为 merge 或 replace")
	}
	seen := map[string]bool{}
	for _, entry := range p.Presets {
		preset, err := packPreset(entry)
		if err != nil {
			return state, err
		}
		if seen[preset.Name] {
			return state, fmt.Errorf("预设包包含重复名称 %s", preset.Name)
		}
		seen[preset.Name] = true
		if _, oldCustom := s.armorCustom.Presets[preset.Name]; !oldCustom {
			if _, err := s.armor.Lookup(preset.Name); err == nil {
				return state, fmt.Errorf("名称 %s 属于内置预设，请改用新名称", preset.Name)
			}
		}
		if _, exists := state.Presets[preset.Name]; exists {
			return state, fmt.Errorf("用户预设 %s 已存在；合并不会覆盖，请选择替换方式或改名", preset.Name)
		}
		state.Presets[preset.Name] = preset
	}
	seenBindings := map[string]bool{}
	for original, name := range p.Bindings {
		model := armor.ModelBindingKey(original)
		if model == "" || len(original) > 256 || seenBindings[model] || name != armor.PresetNameKey(name) || name == "" {
			return state, fmt.Errorf("预设包的模型绑定格式无效或规范化后重复")
		}
		seenBindings[model] = true
		if mode == "merge" {
			if old, exists := state.Bindings[model]; exists && old != name {
				return state, fmt.Errorf("模型 %s 已绑定其他预设；合并不会覆盖", original)
			}
		}
		state.Bindings[model] = name
	}
	for _, name := range state.Bindings {
		if name == "off" {
			continue
		}
		if _, exists := state.Presets[name]; exists {
			continue
		}
		if _, removed := s.armorCustom.Presets[name]; removed {
			return state, fmt.Errorf("绑定引用了替换后不存在的预设 %s", name)
		}
		if _, err := s.armor.Lookup(name); err != nil {
			return state, fmt.Errorf("绑定引用了不存在的预设 %s", name)
		}
	}
	if len(state.Presets) > 64 || len(state.Bindings) > 256 {
		return state, fmt.Errorf("合并后超过 64 个用户预设或 256 个绑定")
	}
	raw, err := json.Marshal(state)
	if err != nil || len(raw) > 8<<20 {
		return state, fmt.Errorf("用户预设配置总量超过 8 MB")
	}
	return state, nil
}

func (s *Server) handleArmorPack(w http.ResponseWriter, r *http.Request) {
	p, err := decodeArmorPack(w, r)
	if err != nil {
		writeError(w, 400, "invalid_pack", err.Error())
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "merge"
	}
	s.armorMu.Lock()
	defer s.armorMu.Unlock()
	if s.armorConfigError != "" || s.armor == nil {
		writeError(w, 409, "configuration_error", "当前预设配置异常，不能导入或覆盖")
		return
	}
	previous := exportArmorPack(s.customizationCopy())
	state, err := s.prepareArmorPack(p, mode)
	if err != nil {
		writeError(w, 400, "invalid_pack", err.Error())
		return
	}
	next := exportArmorPack(state)
	if r.Method == "POST" {
		names := []string{}
		for _, entry := range p.Presets {
			names = append(names, entry.Name)
		}
		writeJSON(w, 200, map[string]any{"valid": true, "mode": mode, "base_sha256": previous.StateSHA256, "next_sha256": next.StateSHA256, "presets": len(state.Presets), "bindings": len(state.Bindings), "incoming_names": names})
		return
	}
	match := r.Header.Get("If-Match")
	if match == "" {
		writeError(w, 428, "precondition_required", "请先预览或导出当前配置，再传入 If-Match 指纹")
		return
	}
	if match != `"`+previous.StateSHA256+`"` {
		writeError(w, 412, "configuration_changed", "配置已被修改，请重新预览后导入")
		return
	}
	oldNames := make([]string, 0, len(s.armorCustom.Presets))
	for name := range s.armorCustom.Presets {
		oldNames = append(oldNames, name)
	}
	if err := s.saveArmorCustomization(r.Context(), state); err != nil {
		writeError(w, 500, "persist_failed", err.Error())
		return
	}
	presets := make([]armor.Preset, 0, len(state.Presets))
	for _, preset := range state.Presets {
		presets = append(presets, preset)
	}
	s.armor.ApplyCustomization(oldNames, presets, state.Bindings)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"imported": true, "presets": len(presets), "bindings": len(state.Bindings), "state_sha256": next.StateSHA256, "previous": previous})
}
