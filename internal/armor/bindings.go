package armor

import "sort"

func ModelBindingKey(model string) string { return canonicalModelName(model) }

func PresetNameKey(name string) string { return normalizeName(name) }

// SetModelBindings 原子替换具体模型绑定；off 只关闭该模型的自动注入。
func (r *Registry) SetModelBindings(bindings map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.modelBindings = map[string]string{}
	for model, preset := range bindings {
		r.modelBindings[canonicalModelName(model)] = preset
	}
}

func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.presets, name)
	for i, item := range r.order {
		if item == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	if r.defaultName == name {
		r.defaultName = ""
		if len(r.order) > 0 {
			r.defaultName = r.order[0]
		}
	}
	r.reindexLocked()
}

// ApplyCustomization 一次替换已校验的用户预设与绑定，推理请求不会看见半套配置。
// 调用方须先持久化并校验名称、正文及内置预设冲突，previous 只能包含用户预设。
func (r *Registry) ApplyCustomization(previous []string, presets []Preset, bindings map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range previous {
		delete(r.presets, name)
	}
	for _, preset := range presets {
		r.presets[preset.Name] = preset.clone()
	}
	r.order = r.order[:0]
	for name := range r.presets {
		r.order = append(r.order, name)
	}
	sort.Strings(r.order)
	if _, exists := r.presets[r.defaultName]; !exists {
		r.defaultName = ""
		if len(r.order) > 0 {
			r.defaultName = r.order[0]
		}
	}
	r.modelBindings = map[string]string{}
	for model, name := range bindings {
		r.modelBindings[model] = name
	}
	r.reindexLocked()
}
