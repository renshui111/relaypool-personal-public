package api

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/armor"
	"github.com/relaypool/relaypool/internal/provider"
)

// 捕获三种入口完成解析、替换和选号后的请求，确保内部保留标志不能被客户端伪造。
type systemPromptReviewProvider struct {
	*stubProvider
	last *provider.ChatRequest
}

func (p *systemPromptReviewProvider) ChatNonStream(ctx context.Context, c provider.Credential, r *provider.ChatRequest) (*provider.ChatResult, error) {
	p.last = r
	return p.stubProvider.ChatNonStream(ctx, c, r)
}
func (p *systemPromptReviewProvider) ChatStream(ctx context.Context, c provider.Credential, r *provider.ChatRequest) (*provider.Stream, error) {
	p.last = r
	return p.stubProvider.ChatStream(ctx, c, r)
}

func TestSystemPromptPolicyAcrossProtocols(t *testing.T) {
	paths := []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"deepseek-v4","messages":[{"role":"system","content":"调用方提示"},{"role":"developer","content":"开发者提示"},{"role":"user","content":"原始问题"}],"preserve_system_prompt":true,"stream":STREAM}`},
		{"/v1/messages", `{"model":"deepseek-v4","system":"调用方提示","max_tokens":32,"messages":[{"role":"user","content":"原始问题"}],"stream":STREAM}`},
		{"/v1/responses", `{"model":"deepseek-v4","instructions":"调用方提示","input":[{"role":"developer","content":"开发者提示"},{"role":"user","content":"原始问题"}],"stream":STREAM}`},
	}
	for _, enabled := range []bool{false, true} {
		for _, drop := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, route := range paths {
					t.Run(route.path+"/"+boolString(enabled)+"/"+boolString(drop)+"/"+boolString(stream), func(t *testing.T) {
						server := newTestServerNoAdapter(t)
						adapter := &systemPromptReviewProvider{stubProvider: newStubProvider()}
						server.registry.Register(adapter)
						seedCredential(t, server.store, "acc_test", adapter.family)
						item := poolItem("acc_test", adapter.family)
						item.DropSystemPrompt = drop
						server.pool.Upsert(item)
						h := httptest.NewServer(server.Handler())
						t.Cleanup(h.Close)
						diagnosticPresets(t, server)
						if err := setTestKeyArmor(t, server, enabled); err != nil {
							t.Fatal(err)
						}
						resp := workspacePost(t, h.URL, route.path, strings.ReplaceAll(route.body, "STREAM", boolString(stream)))
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						if resp.StatusCode != 200 || adapter.last == nil {
							t.Fatal("请求没有成功到达记录渠道")
						}
						var prompts []string
						for _, m := range adapter.last.Messages {
							if m.Role == "system" || m.Role == "developer" {
								prompts = append(prompts, m.ContentText())
							}
						}
						if enabled {
							if len(prompts) != 1 || prompts[0] != "请用简体中文回答。" || resp.Header.Get(armor.HeaderApplied) != "fixture-deepseek" {
								t.Fatal("预设没有保留或调用方提示残留")
							}
						} else if drop {
							if len(prompts) != 0 || resp.Header.Get(armor.HeaderApplied) != "none" {
								t.Fatal("账号清理未执行或客户端伪造保留标志")
							}
						} else if len(prompts) == 0 || prompts[0] != "调用方提示" {
							t.Fatal("未开启清理却丢掉调用方提示")
						}
						if adapter.last.Messages[len(adapter.last.Messages)-1].ContentText() != "原始问题" {
							t.Fatal("原问题被改写")
						}
					})
				}
			}
		}
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
