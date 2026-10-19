package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/armor"
	"github.com/relaypool/relaypool/internal/store"
)

const customizationSetting = "armor_customization"

type armorCustomization struct {
	Presets  map[string]armor.Preset `json:"presets"`
	Bindings map[string]string       `json:"bindings"`
}

func (s *Server) loadArmorCustomization() {
	s.armorCustom = armorCustomization{Presets: map[string]armor.Preset{}, Bindings: map[string]string{}}
	if s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := s.store.GetSetting(ctx, customizationSetting)
	if err == store.ErrSettingNotFound {
		return
	}
	var state armorCustomization
	if err != nil || len(raw) > 8<<20 || json.Unmarshal([]byte(raw), &state) != nil {
		s.armorConfigError = "自定义破甲配置无法读取；已暂停注入，请恢复原配置后重启"
		log.Printf("api: %s", s.armorConfigError)
		return
	}
	if len(state.Presets) > 64 || len(state.Bindings) > 256 {
		s.armorConfigError = "自定义破甲配置超过容量限制；已暂停注入"
		log.Printf("api: 自定义预设配置超过容量限制")
		return
	}
	for name, preset := range state.Presets {
		if name != preset.Name || name != armor.PresetNameKey(name) || armor.ValidateImport(preset) != nil {
			s.armorConfigError = "自定义破甲配置校验失败；已暂停注入"
			log.Printf("api: 自定义预设校验失败")
			return
		}
		if _, err := s.armor.Lookup(name); err == nil {
			s.armorConfigError = "自定义预设名称与内置预设冲突；已暂停注入"
			log.Printf("api: 自定义预设名称与内置预设冲突")
			return
		}
	}
	for model, name := range state.Bindings {
		if model == "" || len(model) > 256 || armor.ModelBindingKey(model) != model {
			s.armorConfigError = "模型绑定格式无效；已暂停注入"
			return
		}
		if name == "off" {
			continue
		}
		if _, ok := state.Presets[name]; !ok {
			if _, err := s.armor.Lookup(name); err != nil {
				s.armorConfigError = "模型绑定引用了不存在的预设；已暂停注入"
				return
			}
		}
	}
	for _, preset := range state.Presets {
		if err := s.armor.Register(preset); err != nil {
			log.Printf("api: 自定义预设加载失败")
			return
		}
	}
	if state.Presets == nil {
		state.Presets = map[string]armor.Preset{}
	}
	if state.Bindings == nil {
		state.Bindings = map[string]string{}
	}
	s.armorCustom = state
	s.armor.SetModelBindings(state.Bindings)
}

// 先持久化整个定制状态，再更新运行时；保存失败不改变正在使用的预设。
func (s *Server) saveArmorCustomization(ctx context.Context, state armorCustomization) error {
	if s.armorConfigError != "" {
		return fmt.Errorf("%s", s.armorConfigError)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(raw) > 8<<20 {
		return fmt.Errorf("自定义预设总量不能超过 8 MB")
	}
	if s.store == nil {
		return fmt.Errorf("没有可用的持久化存储")
	}
	if err := s.store.SetSetting(ctx, customizationSetting, string(raw)); err != nil {
		return fmt.Errorf("保存预设配置失败")
	}
	s.armorCustom = state
	return nil
}

func (s *Server) customizationCopy() armorCustomization {
	state := armorCustomization{Presets: map[string]armor.Preset{}, Bindings: map[string]string{}}
	for k, v := range s.armorCustom.Presets {
		state.Presets[k] = v
	}
	for k, v := range s.armorCustom.Bindings {
		state.Bindings[k] = v
	}
	return state
}

func (s *Server) handleImportArmor(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Body        string `json:"body"`
		Family      string `json:"family"`
		Description string `json:"description"`
		SourceID    string `json:"source_id"`
		Path        string `json:"path"`
		Edited      bool   `json:"edited"`
	}
	if decodeJSONBody(w, r, &input, 2<<20) != nil {
		writeError(w, 400, "invalid_request", "预设 JSON 无效或正文过大")
		return
	}
	selectable := false
	preset := armor.Preset{Name: armor.PresetNameKey(r.PathValue("name")), Body: input.Body, Family: strings.ToLower(strings.TrimSpace(input.Family)), Description: input.Description, AutoSelectable: &selectable, File: "用户导入"}
	if input.SourceID != "" {
		files, err := s.researchFiles(r.Context(), input.SourceID)
		if err != nil {
			writeError(w, 400, "source_unavailable", err.Error())
			return
		}
		found := false
		for _, file := range files {
			if file.Path == input.Path {
				if !input.Edited {
					preset.Body = file.Body
				}
				preset.SourceSHA256 = file.SHA256
				preset.License = file.License
				preset.LicenseText = file.LicenseText
				source, _ := armor.FindResearchSource(input.SourceID)
				preset.SourceURL = "https://github.com/" + source.Repository
				preset.File = file.Path
				if preset.Family == "" {
					preset.Family = file.Family
				}
				found = true
				break
			}
		}
		if !found {
			writeError(w, 404, "not_found", "来源文件不存在")
			return
		}
	}
	if err := armor.ValidateImport(preset); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	// 名称先校验原始字符，再统一下划线别名，防止静默接受空格等非法字符。
	original := preset
	original.Name = r.PathValue("name")
	if err := armor.ValidateImport(original); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if strings.TrimSpace(preset.Body) == "" {
		writeError(w, 400, "invalid_request", "预设正文不能为空")
		return
	}
	if preset.SourceSHA256 == "" {
		preset.SourceSHA256 = armor.TextSHA256(preset.Body)
	}
	s.armorMu.Lock()
	defer s.armorMu.Unlock()
	state := s.customizationCopy()
	if _, custom := state.Presets[preset.Name]; !custom {
		if _, err := s.armor.Lookup(preset.Name); err == nil {
			writeError(w, 409, "name_conflict", "该名称属于内置预设，请使用新名称")
			return
		}
		if len(state.Presets) >= 64 {
			writeError(w, 400, "capacity_reached", "最多导入 64 个自定义预设")
			return
		}
	}
	state.Presets[preset.Name] = preset
	if err := s.saveArmorCustomization(r.Context(), state); err != nil {
		writeError(w, 500, "persist_failed", err.Error())
		return
	}
	_ = s.armor.Register(preset)
	writeJSON(w, 200, map[string]any{"name": preset.Name, "bytes": len(preset.Body), "sha256": preset.SourceSHA256, "imported": true})
}

func (s *Server) handleArmorBinding(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Model  string `json:"model"`
		Preset string `json:"preset"`
	}
	if decodeJSONBody(w, r, &input, 8<<10) != nil {
		writeError(w, 400, "invalid_request", "模型绑定 JSON 无效")
		return
	}
	model := armor.ModelBindingKey(input.Model)
	input.Preset = armor.PresetNameKey(input.Preset)
	if model == "" || len(model) > 256 {
		writeError(w, 400, "invalid_request", "模型名称须为 1 至 256 字节")
		return
	}
	s.armorMu.Lock()
	defer s.armorMu.Unlock()
	if input.Preset != "" && input.Preset != "off" {
		if _, err := s.armor.Lookup(input.Preset); err != nil {
			writeError(w, 400, "unknown_preset", "预设不存在")
			return
		}
	}
	state := s.customizationCopy()
	// 使用同一规范化键，防止大小写、前缀或分隔符不同产生互相覆盖的绑定。
	if input.Preset != "" {
		state.Bindings[model] = input.Preset
	} else {
		delete(state.Bindings, model)
	}
	if len(state.Bindings) > 256 {
		writeError(w, 400, "capacity_reached", "最多设置 256 个模型绑定")
		return
	}
	if err := s.saveArmorCustomization(r.Context(), state); err != nil {
		writeError(w, 500, "persist_failed", err.Error())
		return
	}
	s.armor.SetModelBindings(state.Bindings)
	writeJSON(w, 200, map[string]any{"model": model, "preset": input.Preset})
}

func (s *Server) handleDeleteArmor(w http.ResponseWriter, r *http.Request) {
	s.armorMu.Lock()
	defer s.armorMu.Unlock()
	state := s.customizationCopy()
	name := armor.PresetNameKey(r.PathValue("name"))
	if _, ok := state.Presets[name]; !ok {
		writeError(w, 400, "builtin_preset", "仅能删除用户导入的预设")
		return
	}
	for _, bound := range state.Bindings {
		if bound == name {
			writeError(w, 409, "preset_in_use", "请先解除模型绑定再删除预设")
			return
		}
	}
	delete(state.Presets, name)
	if err := s.saveArmorCustomization(r.Context(), state); err != nil {
		writeError(w, 500, "persist_failed", err.Error())
		return
	}
	s.armor.Remove(name)
	writeJSON(w, 200, map[string]any{"deleted": true})
}
