package armor

import "testing"

func TestQoderKeysDoNotGuessAutomaticModelFamily(t *testing.T) {
	for model, expected := range map[string]string{"qoder/kmodel_latest": FamilyKimi, "qoder/qmodel_38max": FamilyQwen, "qoder/gmodel": FamilyGLM, "qoder/dfmodel": FamilyDeepSeek, "qoder/auto": FamilyGeneric, "qoder/ultimate": FamilyGeneric, "qoder/cmodel": FamilyGeneric, "qoder/mmodel": FamilyMiniMax, "other/kmodel_latest": FamilyGeneric, "kmodel_latest": FamilyGeneric} {
		if got := ModelFamily(model); got != expected {
			t.Errorf("%s：得到 %s，期望 %s", model, got, expected)
		}
	}
}
