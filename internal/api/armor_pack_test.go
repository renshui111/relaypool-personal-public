package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/armor"
)

func testArmorPack() armorPack {
	return armorPack{Format: armorPackFormat, Version: 1, Presets: []armorPackPreset{
		{Name: "owned-gpt", Body: "自有系列测试正文", BodySHA256: armor.TextSHA256("自有系列测试正文"), Family: "gpt", Auto: true, Priority: 200, License: "MIT"},
		{Name: "owned-gpt-version", Body: "自有版本测试正文", BodySHA256: armor.TextSHA256("自有版本测试正文"), Family: "gpt", Auto: true, Priority: 200, ModelPrefixes: []string{"gpt-6.1", "gpt6.1"}},
	}, Bindings: map[string]string{"deepseek-chat": "off"}}
}

func submitArmorPack(t *testing.T, s *Server, method, mode, hash string, pack armorPack) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(pack)
	path := "/admin/armor/pack?mode=" + mode
	if method == "POST" {
		path = "/admin/armor/pack/preview?mode=" + mode
	}
	req := authRequest(t, method, path, string(raw))
	if hash != "" {
		req.Header.Set("If-Match", `"`+hash+`"`)
	}
	r := httptest.NewRecorder()
	s.Handler().ServeHTTP(r, req)
	return r
}

func exportedPack(t *testing.T, s *Server) armorPack {
	t.Helper()
	r := callComplete(t, s, "GET", "/admin/armor/pack", "")
	var p armorPack
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &p) != nil {
		t.Fatalf("导出失败：%s", r.Body.String())
	}
	return p
}

func TestArmorPackPreviewCommitRestartAndRestore(t *testing.T) {
	s := newTestServerNoAdapter(t)
	before := exportedPack(t, s)
	p := testArmorPack()
	if r := submitArmorPack(t, s, "POST", "merge", "", p); r.Code != 200 || !strings.Contains(r.Body.String(), before.StateSHA256) {
		t.Fatal("预览失败：", r.Body.String())
	}
	if exportedPack(t, s).StateSHA256 != before.StateSHA256 {
		t.Fatal("预览改变了配置")
	}
	if r := submitArmorPack(t, s, "PUT", "merge", before.StateSHA256, p); r.Code != 200 || !strings.Contains(r.Body.String(), `"previous"`) {
		t.Fatal("导入失败：", r.Body.String())
	}
	for model, name := range map[string]string{"gpt-4o": "owned-gpt", "gpt6.1-sol": "owned-gpt-version"} {
		v, ok := s.armor.ForModel(model)
		if !ok || v.Name != name {
			t.Fatal("导入后没有按系列及版本自动匹配")
		}
	}
	if testKeyArmor(t, s) {
		t.Fatal("导入打开了总开关")
	}
	installed := exportedPack(t, s)
	s.Close()
	restarted := New(Options{Config: testConfig(t), Store: s.store, APIKey: "rp_test"})
	defer restarted.Close()
	if exportedPack(t, restarted).StateSHA256 != installed.StateSHA256 {
		t.Fatal("重启丢失预设包规则或说明")
	}
	if v, ok := restarted.armor.ForModel("gpt-6.1-sol"); !ok || v.Name != "owned-gpt-version" {
		t.Fatal("重启丢失版本匹配")
	}
	if r := submitArmorPack(t, restarted, "PUT", "replace", installed.StateSHA256, before); r.Code != 200 {
		t.Fatal("还原失败：", r.Body.String())
	}
	if restored := exportedPack(t, restarted); restored.StateSHA256 != before.StateSHA256 {
		t.Fatal("备份没有完整还原用户预设和绑定")
	}
	if _, err := restarted.armor.Lookup("owned-gpt-version"); err == nil {
		t.Fatal("还原没有清除新增用户预设")
	}
	if armor.DistributionProfile == "research" {
		if _, err := restarted.armor.Lookup("gpt-curated"); err != nil {
			t.Fatal("还原删除了内置预设")
		}
	}
}

func TestArmorPackRejectsPartialInvalidAndExecutableFields(t *testing.T) {
	s := newTestServerNoAdapter(t)
	before := exportedPack(t, s)
	cases := []struct {
		label string
		edit  func(*armorPack)
	}{
		{"正文哈希", func(p *armorPack) { p.Presets[1].BodySHA256 = "错误" }},
		{"空白正文", func(p *armorPack) { p.Presets[1].Body = " "; p.Presets[1].BodySHA256 = armor.TextSHA256(" ") }},
		{"跨系列前缀", func(p *armorPack) { p.Presets[1].ModelPrefixes = []string{"claude-opus-4"} }},
		{"重复名称", func(p *armorPack) { p.Presets[1].Name = p.Presets[0].Name }},
		{"未知引用", func(p *armorPack) { p.Bindings["gpt-4o"] = "missing" }},
		{"绑定碰撞", func(p *armorPack) { p.Bindings["GPT_4o"] = "owned-gpt"; p.Bindings["gpt-4o"] = "off" }},
		{"泛用自动匹配", func(p *armorPack) { p.Presets[0].Family = "generic" }},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			p := testArmorPack()
			tc.edit(&p)
			if r := submitArmorPack(t, s, "PUT", "merge", before.StateSHA256, p); r.Code != 400 {
				t.Fatal("错误包被接受：", r.Body.String())
			}
			if exportedPack(t, s).StateSHA256 != before.StateSHA256 {
				t.Fatal("部分无效的包改变了配置")
			}
			if _, err := s.armor.Lookup("owned-gpt"); err == nil {
				t.Fatal("有效的第一项被部分写入")
			}
		})
	}
	for _, raw := range []string{`{"format":"relaypool.armor-pack","version":1,"presets":[],"bindings":{},"install":"echo test"}`, `{"format":"relaypool.armor-pack","version":1,"presets":[],"bindings":{}} {}`} {
		if r := callComplete(t, s, "POST", "/admin/armor/pack/preview", raw); r.Code != 400 {
			t.Fatal("接受了脚本字段或多对象 JSON")
		}
	}
}

func TestArmorPackConcurrentEditAndPersistenceFailurePreserveState(t *testing.T) {
	s := newTestServerNoAdapter(t)
	before := exportedPack(t, s)
	p := testArmorPack()
	if r := submitArmorPack(t, s, "PUT", "merge", "", p); r.Code != 428 {
		t.Fatal("允许没有指纹的写入")
	}
	callComplete(t, s, "PUT", "/admin/armor/bindings", `{"model":"glm-4-plus","preset":"off"}`)
	if r := submitArmorPack(t, s, "PUT", "merge", before.StateSHA256, p); r.Code != 412 {
		t.Fatal("覆盖了另一窗口的修改")
	}
	current := exportedPack(t, s)
	s.store.Close()
	raw, _ := json.Marshal(p)
	req := authRequest(t, "PUT", "/admin/armor/pack?mode=merge", string(raw))
	req.Header.Set("If-Match", `"`+current.StateSHA256+`"`)
	r := httptest.NewRecorder()
	// 绕过已单独测试的鉴权，直接验证数据库保存失败时的状态不变。
	s.handleArmorPack(r, req)
	if r.Code != 500 || !strings.Contains(r.Body.String(), "persist_failed") {
		t.Fatal("数据库写入失败没有返回错误")
	}
	if exportArmorPack(s.customizationCopy()).StateSHA256 != current.StateSHA256 {
		t.Fatal("写入失败改变了运行状态")
	}
}

func TestArmorPackMergeDoesNotOverwriteOrExposeCredentials(t *testing.T) {
	s := newTestServerNoAdapter(t)
	before := exportedPack(t, s)
	p := testArmorPack()
	if r := submitArmorPack(t, s, "PUT", "merge", before.StateSHA256, p); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	current := exportedPack(t, s)
	p.Presets[0].Body = "试图覆盖"
	p.Presets[0].BodySHA256 = armor.TextSHA256(p.Presets[0].Body)
	if r := submitArmorPack(t, s, "PUT", "merge", current.StateSHA256, p); r.Code != 400 {
		t.Fatal("合并覆盖了同名预设")
	}
	if exportedPack(t, s).StateSHA256 != current.StateSHA256 {
		t.Fatal("合并失败改变了状态")
	}
	raw, _ := json.Marshal(current)
	for _, secretField := range []string{"rp_test", "credential", "access_token", "refresh_token", "api_key"} {
		if strings.Contains(string(raw), secretField) {
			t.Fatal("导出包含非预设字段")
		}
	}
	unauth := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauth, httptest.NewRequest("GET", "/admin/armor/pack", nil))
	if unauth.Code != 401 {
		t.Fatal("未鉴权可以导出预设包")
	}
}
