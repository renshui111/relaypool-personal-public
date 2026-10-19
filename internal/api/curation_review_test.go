package api

import (
	"context"
	"encoding/json"
	"github.com/relaypool/relaypool/internal/armor"
	"github.com/relaypool/relaypool/internal/provider"
	"net/http/httptest"
	"testing"
)

func TestCurationBindingClearActuallyRestoresAutomatic(t *testing.T) {
	s := newTestServerNoAdapter(t)
	if armor.DistributionProfile == "public" {
		s.armor.Register(armor.Preset{Name: "fixture-gpt", Family: "gpt", Body: "测试提示"})
	}
	callComplete(t, s, "PUT", "/admin/armor/bindings", `{"model":"gpt-test","preset":"off"}`)
	callComplete(t, s, "PUT", "/admin/armor/bindings", `{"model":"gpt-test","preset":""}`)
	if _, exists := s.armorCustom.Bindings["gpt-test"]; exists {
		t.Fatal("清除绑定仍保存了关闭规则")
	}
	if _, ok := s.armor.ForModel("gpt-test"); !ok {
		t.Fatal("清除绑定没有恢复系列匹配")
	}
}

func TestCurationCorruptConfigurationStopsInjectionAndOverwrite(t *testing.T) {
	s := newTestServerNoAdapter(t)
	s.store.SetSetting(context.Background(), customizationSetting, `{"broken":`)
	s.loadArmorCustomization()
	setTestKeyArmor(t, s, true)
	if d := s.armorDecision(testKeyRequest(t, s, httptest.NewRequest("POST", "/v1/chat/completions", nil)), "gpt-test"); d.Applied {
		t.Fatal("损坏配置退回默认注入")
	}
	if r := callComplete(t, s, "PUT", "/admin/armor/new-preset", `{"body":"测试"}`); r.Code != 500 {
		t.Fatal("覆盖了损坏配置")
	}
	raw, _ := s.store.GetSetting(context.Background(), customizationSetting)
	if raw != `{"broken":` {
		t.Fatal("原配置未保留")
	}
}

func TestCurationEditedSourceRetainsAttribution(t *testing.T) {
	s := newTestServerNoAdapter(t)
	source := "coldbrew"
	files := armor.BundledResearchFiles("coldbrew")
	if len(files) == 0 {
		source = "gpt-instruct"
		files = []armor.ResearchFile{{SourceID: source, Path: "fixture.md", Body: "测试提示", SHA256: armor.TextSHA256("测试提示"), License: "测试许可", LicenseText: "测试许可正文"}}
		s.researchCache = map[string][]armor.ResearchFile{source: files}
	}
	input := `{"source_id":"` + source + `","path":"` + files[0].Path + `","body":"用户编辑内容","edited":true}`
	if r := callComplete(t, s, "PUT", "/admin/armor/edited_text", input); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	p, err := s.armor.Lookup("edited-text")
	if err != nil || p.Body != "用户编辑内容" || p.SourceURL == "" || p.LicenseText == "" || p.SourceSHA256 != files[0].SHA256 {
		t.Fatal("编辑丢失来源或许可")
	}
	if r := callComplete(t, s, "DELETE", "/admin/armor/edited_text", ""); r.Code != 200 {
		t.Fatal("名称别名无法删除")
	}
}

func TestCurationKeyPermissionWriteFailureIsRejected(t *testing.T) {
	s := newTestServerNoAdapter(t)
	k, err := s.store.AuthenticateAPIKey(t.Context(), "rp_test")
	if err != nil {
		t.Fatal(err)
	}
	s.store.Close()
	p := k.Permissions
	p.ArmorAllowed = true
	p.ArmorEnabled = true
	if _, err := s.store.UpdateAPIKey(t.Context(), k.ID, k.Name, k.Scope, p); err == nil {
		t.Fatal("数据库不可写时却报告保存成功")
	}
	if k.Permissions.ArmorEnabled {
		t.Fatal("失败修改影响了已鉴权快照")
	}
}

func TestCurationArmorPreservesOtherRequestOptions(t *testing.T) {
	req := &provider.ChatRequest{Raw: json.RawMessage(`{"messages":[{"role":"system","content":"旧提示"}],"instructions":"旧提示","response_format":{"type":"json_object"},"max_completion_tokens":64}`)}
	applyArmor(req, armor.Decision{Applied: true, Preset: armor.Preset{Body: "新提示"}})
	var body map[string]any
	json.Unmarshal(req.Raw, &body)
	if _, ok := body["messages"]; ok {
		t.Fatal("旧提示残留")
	}
	if body["max_completion_tokens"] != float64(64) || body["response_format"] == nil {
		t.Fatal("破甲丢失了非提示请求选项")
	}
}
