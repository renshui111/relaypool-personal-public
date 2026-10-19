package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/relaypool/relaypool/internal/armor"
	"github.com/relaypool/relaypool/internal/store"
)

// 使用自有普通指令核对真实请求字段，不测试任何限制绕过效果。
func diagnosticPresets(t *testing.T, server *Server) {
	t.Helper()
	dir := t.TempDir()
	for _, family := range []string{"qwen", "deepseek"} {
		body := "<!-- armor\nfamily: " + family + "\nauto: true\n-->\n请用简体中文回答。"
		if err := os.WriteFile(filepath.Join(dir, "fixture-"+family+".md"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	withArmorDir(t, server, dir)
}

func diagnosticResolve(t *testing.T, base, query string) map[string]any {
	t.Helper()
	req := authRequest(t, http.MethodGet, base+"/admin/armor/resolve?"+query, "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result map[string]any
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&result) != nil {
		t.Fatal("诊断查询失败")
	}
	return result
}

func TestArmorDiagnosticsSeparatesCandidateFromEffectiveState(t *testing.T) {
	h, server := newTestServer(t, newStubProvider())
	diagnosticPresets(t, server)
	result := diagnosticResolve(t, h.URL, "model=deepseek-v4")
	if result["applied"] != true || result["effective"] != false || result["active_status"] != "not_allowed" {
		t.Fatal("关闭状态被误报为已经生效")
	}
	enableArmor(t, h.URL)
	result = diagnosticResolve(t, h.URL, "model=auto")
	if result["effective"] != false || result["active_status"] != "unknown_model" {
		t.Fatal("自动选型没有明确说明无法匹配")
	}
	server.armor.SetModelBindings(map[string]string{"deepseek-v4": "off"})
	result = diagnosticResolve(t, h.URL, "model=deepseek-v4")
	if result["active_status"] != "model_disabled" || result["effective"] != false {
		t.Fatal("型号单独关闭被误报为没有预设")
	}
	server.armor.SetModelBindings(map[string]string{"auto": "fixture-deepseek"})
	result = diagnosticResolve(t, h.URL, "model=auto")
	if result["selection_kind"] != "binding" || result["effective"] != true || result["body_sha256"] == "" {
		t.Fatal("明确绑定或正文指纹没有正确报告")
	}
	server.armor = nil
	result = diagnosticResolve(t, h.URL, "model=deepseek-v4")
	if result["active_status"] != "no_presets" || result["effective"] != false {
		t.Fatal("未加载预设的诊断不完整")
	}
}

func TestPinnedQoderPreservesModelFamilyAndRequestEvidence(t *testing.T) {
	adapter := &presetRecordingProvider{stubProvider: newStubProvider()}
	adapter.family = "qoder"
	server := newTestServerNoAdapter(t)
	server.registry.Register(adapter)
	seedCredential(t, server.store, "acc_test", adapter.family)
	server.pool.Upsert(poolItem("acc_test", adapter.family))
	h := httptest.NewServer(server.Handler())
	t.Cleanup(h.Close)
	diagnosticPresets(t, server)
	enableArmor(t, h.URL)
	preview := diagnosticResolve(t, h.URL, "model=qmodel&account_id=acc_test")
	if preview["family"] != "qwen" || preview["preset"] != "fixture-qwen" || preview["effective"] != true {
		t.Fatal("固定账号的内部型号丢失了渠道匹配")
	}
	for _, path := range []string{"/v1/chat/completions", "/admin/accounts/acc_test/chat/completions"} {
		model := "qmodel"
		if path == "/v1/chat/completions" {
			model = "qoder/qmodel"
		}
		resp := workspacePost(t, h.URL, path, `{"model":"`+model+`","messages":[{"role":"user","content":"你好"}],"stream":true}`)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get(armor.HeaderApplied) != "fixture-qwen" || resp.Header.Get("X-RelayPool-Armor-Status") != "injected" {
			t.Fatal("预览与固定账号实际注入不一致")
		}
		if resp.Header.Get("X-RelayPool-Armor-SHA256") != armor.TextSHA256("请用简体中文回答。") {
			t.Fatal("实际请求的预设指纹不一致")
		}
		adapter.mu.Lock()
		messages := adapter.messages
		adapter.mu.Unlock()
		if len(messages) != 2 || messages[0].Role != "system" || messages[0].ContentText() != "请用简体中文回答。" {
			t.Fatal("标记有预设但适配器没有收到正确正文")
		}
		logs, err := server.store.ListRequests(context.Background(), store.RequestLogFilter{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, log := range logs {
			if log.ID == resp.Header.Get("X-Request-ID") && log.ArmorPreset == "fixture-qwen" {
				found = true
			}
		}
		if !found {
			t.Fatal("请求日志没有保留实际预设")
		}
	}
}

func TestArmorFailureHeadersDoNotClaimUpstreamExecution(t *testing.T) {
	server := newTestServerNoAdapter(t)
	diagnosticPresets(t, server)
	h := httptest.NewServer(server.Handler())
	defer h.Close()
	enableArmor(t, h.URL)
	resp := workspacePost(t, h.URL, "/v1/chat/completions", `{"model":"deepseek-v4","messages":[{"role":"user","content":"你好"}]}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 || resp.Header.Get("X-RelayPool-Armor-Status") != "injected" || resp.Header.Get(armor.HeaderApplied) != "fixture-deepseek" {
		t.Fatal("失败响应没有保留网关注入诊断")
	}
}
