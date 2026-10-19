package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/relaypool/relaypool/internal/provider"
)

type presetRecordingProvider struct {
	*stubProvider
	mu       sync.Mutex
	messages []provider.Message
}

func (p *presetRecordingProvider) ChatStream(ctx context.Context, credential provider.Credential, req *provider.ChatRequest) (*provider.Stream, error) {
	p.mu.Lock()
	p.messages = append([]provider.Message{}, req.Messages...)
	p.mu.Unlock()
	return p.stubProvider.ChatStream(ctx, credential, req)
}

// 用自有普通系统指令验证两条聊天入口，不测试或宣称任何模型限制绕过效果。
func TestWorkspaceChatReportsActualSystemPreset(t *testing.T) {
	adapter := &presetRecordingProvider{stubProvider: newStubProvider()}
	server := newTestServerNoAdapter(t)
	server.registry.Register(adapter)
	seedCredential(t, server.store, "acc_test", adapter.family)
	server.pool.Upsert(poolItem("acc_test", adapter.family))
	h := httptest.NewServer(server.Handler())
	t.Cleanup(h.Close)
	dir := t.TempDir()
	const instruction = "本次测试请用简体中文作答。"
	if err := os.WriteFile(filepath.Join(dir, "fixture-glm.md"), []byte("<!-- armor\nfamily: glm\nauto: true\npriority: 100\n-->\n"+instruction), 0600); err != nil {
		t.Fatal(err)
	}
	withArmorDir(t, server, dir)
	for _, enabled := range []bool{false, true} {
		body, _ := json.Marshal(map[string]bool{"enabled": enabled})
		if status, _ := armorSwitchPut(t, h.URL, string(body)); status != 200 {
			t.Fatal("修改测试开关失败")
		}
		for _, path := range []string{"/v1/chat/completions", "/admin/accounts/acc_test/chat/completions"} {
			resp := workspacePost(t, h.URL, path, `{"model":"glm-5.2","messages":[{"role":"user","content":"测试消息"}],"stream":true}`)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatal("测试聊天入口调用失败")
			}
			header := resp.Header.Get("X-RelayPool-Armor-Applied")
			if enabled && header != "fixture-glm" {
				t.Fatal("开启后没有回报实际应用的预设")
			}
			if !enabled && header != "none" {
				t.Fatal("关闭后仍回报已应用预设")
			}
			// 原始输入中的提示字段已清理，应核查交给适配器的中立消息。
			adapter.mu.Lock()
			raw, _ := json.Marshal(adapter.messages)
			adapter.mu.Unlock()
			if strings.Contains(string(raw), instruction) != enabled {
				t.Fatal("响应标记与实际发送的系统指令不一致")
			}
		}
	}
}
