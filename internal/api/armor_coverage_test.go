package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/relaypool/relaypool/internal/armor"
	"github.com/relaypool/relaypool/internal/provider"
)

func TestBundledVersionsActuallyInjectFullCleanBody(t *testing.T) {
	s := newTestServerNoAdapter(t)
	s.armor = armor.NewRegistry()
	if _, err := s.armor.LoadBundledPresets(); err != nil {
		t.Fatal(err)
	}
	if err := setTestKeyArmor(t, s, true); err != nil {
		t.Fatal(err)
	}
	for _, row := range s.armor.Coverage() {
		for _, value := range row["models"].([]map[string]any) {
			model := value["model"].(string)
			d := s.armorDecision(testKeyRequest(t, s, httptest.NewRequest("POST", "/v1/chat/completions", nil)), model)
			if d.Applied != value["available"] {
				t.Fatalf("%s 分配与注入不同", model)
			}
			req := &provider.ChatRequest{Raw: json.RawMessage(`{"temperature":0.2}`)}
			applyArmor(req, d)
			if d.Applied {
				var body string
				if len(req.Messages) == 0 || req.Messages[0].Role != "system" {
					t.Fatalf("%s 缺少系统提示", model)
				}
				if err := json.Unmarshal(req.Messages[0].Content, &body); err != nil || body != d.Preset.Body {
					t.Fatalf("%s 未注入完整正文", model)
				}
			}
		}
	}
}
