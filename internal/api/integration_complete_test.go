package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/armor"
)

func callComplete(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, authRequest(t, method, path, body))
	return response
}

func TestCompletePresetImportBindingAndRestart(t *testing.T) {
	s := newTestServerNoAdapter(t)
	files := armor.BundledResearchFiles("gpt-instruct")
	if len(files) == 0 && armor.DistributionProfile == "public" {
		for _, name := range []string{"fixture-a.md", "fixture-b.md", "fixture-c.md"} {
			files = append(files, armor.ResearchFile{SourceID: "gpt-instruct", Path: name, Body: "测试完整正文", SHA256: armor.TextSHA256("测试完整正文")})
		}
		s.researchCache = map[string][]armor.ResearchFile{"gpt-instruct": files}
	}
	if len(files) != 3 {
		t.Fatal("离线发布提示资源未齐全")
	}
	input, _ := json.Marshal(map[string]any{"source_id": "gpt-instruct", "path": files[1].Path, "family": "gpt"})
	response := callComplete(t, s, "PUT", "/admin/armor/user-gpt", string(input))
	if response.Code != 200 {
		t.Fatalf("导入失败：%s", response.Body.String())
	}
	if response := callComplete(t, s, "PUT", "/admin/armor/bindings", `{"model":"GPT-6-Astra","preset":"user-gpt"}`); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if decision := s.armor.Resolve(armor.ApplyOptions{Enabled: true, Model: "workbuddy/gpt_6_astra"}); !decision.Applied || decision.Preset.Name != "user-gpt" || decision.Preset.Body != files[1].Body {
		t.Fatal("具体模型没有绑定到原始提示文本")
	}
	if response := callComplete(t, s, "DELETE", "/admin/armor/user-gpt", ""); response.Code != 409 {
		t.Fatal("仍在使用的预设被删除")
	}
	s.Close()
	restarted := New(Options{Config: testConfig(t), Store: s.store, APIKey: "rp_test"})
	defer restarted.Close()
	if d := restarted.armor.Resolve(armor.ApplyOptions{Enabled: true, Model: "gpt-6-astra"}); !d.Applied || d.Preset.SourceSHA256 != files[1].SHA256 {
		t.Fatal("导入或模型绑定重启丢失")
	}
	if response := callComplete(t, restarted, "PUT", "/admin/armor/bindings", `{"model":"gpt_6_astra","preset":"off"}`); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if d := restarted.armor.Resolve(armor.ApplyOptions{Enabled: true, Model: "gpt-6-astra"}); d.Applied {
		t.Fatal("模型关闭被族预设覆盖")
	}
	callComplete(t, restarted, "PUT", "/admin/armor/bindings", `{"model":"gpt-6-astra","preset":""}`)
	if response := callComplete(t, restarted, "DELETE", "/admin/armor/user-gpt", ""); response.Code != 200 {
		t.Fatal("解除绑定后无法删除")
	}
}

func TestCompletePresetImportValidationAndBuiltinProtection(t *testing.T) {
	s := newTestServerNoAdapter(t)
	for _, name := range []string{"invalid.name", "off", "bad name"} {
		if r := callComplete(t, s, "PUT", "/admin/armor/"+strings.ReplaceAll(name, " ", "%20"), `{"body":"文本"}`); r.Code != 400 {
			t.Fatal("非法导入名称被接受")
		}
	}
	if len(s.armor.Names()) == 0 {
		s.armor.Register(armor.Preset{Name: "fixture-builtin", Family: "gpt", Body: "测试内置正文"})
	}
	name := s.armor.Names()[0]
	if r := callComplete(t, s, "PUT", "/admin/armor/"+name, `{"body":"覆盖"}`); r.Code != 409 {
		t.Fatal("可覆盖内置预设")
	}
	s.researchCache = map[string][]armor.ResearchFile{"calrton": {{SourceID: "calrton", Path: "fixture.md", Body: "测试正文"}}}
	if r := callComplete(t, s, "GET", "/admin/armor/sources/calrton/preview?path=../../config.json", ""); r.Code != 404 {
		t.Fatal("来源路径越界")
	}
}

func TestCompleteRemoteArchiveIsDataOnly(t *testing.T) {
	var raw bytes.Buffer
	writer := zip.NewWriter(&raw)
	for name, body := range map[string]string{"repo-main/prompt.mkd": "提示正文", "repo-main/README.md": "安装说明", "repo-main/install.sh": "应不执行", "repo-main/LICENSE": "原许可"} {
		entry, _ := writer.Create(name)
		io.WriteString(entry, body)
	}
	writer.Close()
	files, err := researchArchive(raw.Bytes(), "spiritual")
	if err != nil || len(files) != 1 || files[0].Path != "prompt.mkd" || files[0].LicenseText != "原许可" {
		t.Fatalf("来源解析错误：%v", err)
	}
}

func TestCompleteCompatibleGatewayAndGroupedFailover(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer quota-key" {
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"type":"insufficient_quota"},"retry_after":120}`)
			return
		}
		var input map[string]any
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			t.Error("上游请求不是 JSON")
		}
		for _, key := range []string{"input", "instructions", "system", "stop_sequences"} {
			if _, ok := input[key]; ok {
				t.Errorf("专有字段 %s 泄漏到兼容接口", key)
			}
		}
		if input["model"] != "gpt-test" {
			t.Error("渠道前缀未移除")
		}
		messages := input["messages"].([]any)
		if messages[0].(map[string]any)["content"] != "测试提示" {
			t.Error("自定义提示没有传到上游")
		}
		if input["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"id\":\"c\",\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"完成\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		io.WriteString(w, `{"id":"c","model":"gpt-test","choices":[{"message":{"role":"assistant","content":"完成"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	s := newTestServerNoAdapter(t)
	callComplete(t, s, "PUT", "/admin/armor/local-test", `{"body":"测试提示","family":"gpt"}`)
	callComplete(t, s, "PUT", "/admin/armor/bindings", `{"model":"gpt-test","preset":"local-test"}`)
	if err := setTestKeyArmor(t, s, true); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, spec := range []struct{ name, group, key string }{{"a", "项目一", "quota-key"}, {"b", "项目一", "quota-key"}, {"c", "项目二", "usable-key"}} {
		credential, _ := json.Marshal(map[string]any{"api_key": spec.key, "base_url": upstream.URL + "/v1", "models": []string{"gpt-test"}})
		body, _ := json.Marshal(map[string]any{"name": spec.name, "provider": "openai-compatible", "quota_scope": "shared", "quota_group": spec.group, "credential": string(credential), "drop_system_prompt": false})
		response := callComplete(t, s, "POST", "/admin/accounts", string(body))
		if response.Code != 201 {
			t.Fatal(response.Body.String())
		}
		var created map[string]any
		json.Unmarshal(response.Body.Bytes(), &created)
		ids[spec.name] = created["id"].(string)
	}
	for _, stream := range []bool{false, true} {
		for path, body := range map[string]map[string]any{
			"/v1/chat/completions": {"messages": []any{map[string]any{"role": "user", "content": "问候"}}},
			"/v1/messages":         {"system": "客户端提示", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "问候"}}},
			"/v1/responses":        {"instructions": "客户端提示", "input": "问候"},
		} {
			body["model"] = "openai-compatible/gpt-test"
			body["stream"] = stream
			encoded, _ := json.Marshal(body)
			request := authRequest(t, "POST", path, string(encoded))
			request.Header.Set("X-RelayPool-Account", ids["a"])
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, request)
			if response.Code != 200 || !strings.Contains(response.Body.String(), "完成") {
				t.Fatalf("协议 %s 流=%v 失败：%s", path, stream, response.Body.String())
			}
			if response.Header().Get("X-RelayPool-Armor-Preset") != "local-test" {
				t.Fatal("预设应用信息缺失")
			}
		}
	}
	for _, name := range []string{"a", "b"} {
		item, _ := s.pool.ByID(ids[name])
		if item.GroupDownUntil.IsZero() {
			t.Fatal("额度故障未隔离共享组")
		}
	}
	if item, _ := s.pool.ByID(ids["c"]); !item.GroupDownUntil.IsZero() {
		t.Fatal("独立额度组被错误冷却")
	}
}
