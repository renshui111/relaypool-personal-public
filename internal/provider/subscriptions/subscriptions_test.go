package subscriptions

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

func testCredential(t *testing.T, c *Client, endpoint string, extra map[string]any) provider.Credential {
	t.Helper()
	input := map[string]any{"access_token": "dt-synthetic-access", "refresh_token": "drt-synthetic-refresh", "uid": "synthetic-user", "region": "cn", "machine_id": "original-machine", "device_id": "original-device"}
	if c.family == "trae" {
		input["access_token"] = "synthetic-trae-access"
		input["refresh_token"] = "synthetic-trae-refresh"
	}
	if endpoint != "" {
		input["base_url"] = endpoint
		input["api_host"] = endpoint
	}
	for k, v := range extra {
		input[k] = v
	}
	cred, err := c.DecodeCredential(marshal(input))
	if err != nil {
		t.Fatal(err)
	}
	cred.AccountID = "account-one"
	return cred
}

// 解码器独立从公开编码表反向推导，模拟上游验证真实发送字节。
func decodeWire(t *testing.T, raw []byte) []byte {
	t.Helper()
	const alphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!$"
	const standard = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="
	normal := make([]byte, len(raw))
	for i, char := range raw {
		index := strings.IndexByte(alphabet, char)
		if index < 0 {
			t.Fatal("编码含未知字符")
		}
		normal[i] = standard[index]
	}
	n, a := len(normal), len(normal)/3
	ordered := append(append(append([]byte{}, normal[n-a:]...), normal[a:n-a]...), normal[:a]...)
	decoded, err := base64.StdEncoding.DecodeString(string(ordered))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestCredentialImportAndRegionIsolation(t *testing.T) {
	qoder := NewQoder(Config{})
	trae := NewTrae(Config{})
	for _, test := range []struct {
		name   string
		client *Client
		raw    string
		region provider.Region
		valid  bool
	}{
		{"桌面国际", qoder, `{"auth":{"accessToken":"dt-synthetic","refreshToken":"drt-synthetic","domain":"qoder.com","expiresAt":1790000000000},"account":{"uid":"synthetic"}}`, provider.RegionGlobal, true},
		{"本人PAT", qoder, `{"personal_token":"pt-synthetic","region":"global"}`, provider.RegionGlobal, true},
		{"错误地区", qoder, `{"access_token":"dt-synthetic","uid":"synthetic","region":"cn","domain":"qoder.com"}`, "", false},
		{"其他来源", qoder, `{"provider":"claude","access_token":"dt-synthetic","uid":"synthetic"}`, "", false},
		{"错误刷新类型", qoder, `{"access_token":"dt-synthetic","refresh_token":"foreign-refresh","uid":"synthetic"}`, "", false},
		{"拒绝地区主机混用", qoder, `{"access_token":"dt-synthetic","uid":"synthetic","region":"global","api_host":"https://openapi.qoder.com.cn"}`, "", false},
		{"拒绝远程自定义地址", qoder, `{"access_token":"dt-synthetic","uid":"synthetic","base_url":"https://attacker.invalid"}`, "", false},
		{"拒绝缺少设备", trae, `{"access_token":"synthetic","uid":"synthetic"}`, "", false},
		{"拒绝国际远程代理冒充聊天", trae, `{"access_token":"synthetic","uid":"synthetic","region":"global","machine_id":"original","device_id":"original"}`, "", false},
		{"国内嵌套", trae, `{"auth":{"accessToken":"synthetic","refreshToken":"synthetic","domain":"trae.cn","machineId":"original","deviceId":"original"},"account":{"uid":"synthetic"}}`, provider.RegionCN, true},
		{"多余对象", qoder, `{"access_token":"dt-synthetic","uid":"synthetic"} {}`, "", false},
		{"非法过期时间", qoder, `{"access_token":"dt-synthetic","uid":"synthetic","expires_at":"unknown"}`, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cred, err := test.client.DecodeCredential([]byte(test.raw))
			if (err == nil) != test.valid {
				t.Fatalf("校验结果不符：%v", err)
			}
			if test.valid && cred.Region != test.region {
				t.Fatal("地区错误")
			}
		})
	}
}

func TestQoderWireSignatureAndIdentity(t *testing.T) {
	c := NewQoder(Config{})
	cred := testCredential(t, c, "", nil)
	body := []byte(qoderEncode([]byte(`{"messages":[{"role":"user","content":"保留空格 left right"}]}`)))
	endpoint := "https://gateway.qoder.com.cn" + qoderChatPath
	h, err := c.qoderHeaders(cred, endpoint, body, "kmodel_latest")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimPrefix(h.Get("Authorization"), "Bearer COSY."), ".")
	if len(parts) != 2 {
		t.Fatal("签名格式错误")
	}
	sum := md5.Sum([]byte(parts[0] + "\n" + h.Get("Cosy-Key") + "\n" + h.Get("Cosy-Date") + "\n" + string(body) + "\n/api/v2/service/pro/sse/agent_chat_generation"))
	if parts[1] != hex.EncodeToString(sum[:]) {
		t.Fatal("签名和实际发送字节不同")
	}
	raw, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]string
	json.Unmarshal(raw, &envelope)
	session, err := c.qoderSession(cred)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, _ := base64.StdEncoding.DecodeString(envelope["info"])
	block, _ := aes.NewCipher([]byte(session.key))
	plain := make([]byte, len(encrypted))
	cipher.NewCBCDecrypter(block, []byte(session.key)).CryptBlocks(plain, encrypted)
	plain = plain[:len(plain)-int(plain[len(plain)-1])]
	var identity map[string]string
	if json.Unmarshal(plain, &identity) != nil || identity["security_oauth_token"] != cred.Field("access_token") || identity["uid"] != "synthetic-user" {
		t.Fatal("签名身份错误")
	}
	other := cred
	other.AccountID = "account-two"
	other.Raw = marshal(map[string]any{"access_token": "dt-other", "uid": "other"})
	other.Fields = map[string]string{"access_token": "dt-other", "uid": "other", "user_type": "personal_standard"}
	second, _ := c.qoderSession(other)
	if second == session || second.info == session.info {
		t.Fatal("账号会话混用")
	}
	if !strings.Contains(string(decodeWire(t, body)), "left right") {
		t.Fatal("编码丢失文字")
	}
}

func TestChatRoundtripBothNativeProtocols(t *testing.T) {
	for _, family := range []string{"qoder", "trae"} {
		t.Run(family, func(t *testing.T) {
			c := newClient(family, Config{})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, _ := io.ReadAll(r.Body)
				if family == "qoder" {
					raw = decodeWire(t, raw)
					if r.Header.Get("X-Model-Key") != "actual-model" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
						t.Error("Qoder 鉴权头错误")
					}
				} else {
					if r.Header.Get("Authorization") != "Cloud-IDE-JWT synthetic-trae-access" || r.Header.Get("X-Machine-Id") != "original-machine" {
						t.Error("Trae 设备或鉴权混用")
					}
				}
				var body map[string]any
				json.Unmarshal(raw, &body)
				if !strings.Contains(string(raw), "真正的工具结果") || strings.Contains(string(raw), "stale-system") {
					t.Error("中立历史没有正确重建")
				}
				if family == "trae" {
					if body["function"] != "solo_work_lite" || body["config_name"] != "actual-model" || !strings.Contains(string(raw), "function_call") {
						t.Error("Trae 专用消息转换失败")
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if family == "qoder" {
					io.WriteString(w, "data: "+string(marshal(map[string]any{"statusCodeValue": 200, "body": string(marshal(map[string]any{"id": "qoder-test", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": " 成功 ", "reasoning_content": "思考", "tool_calls": []any{map[string]any{"index": 0, "id": "call-one", "type": "function", "function": map[string]any{"name": "echo", "arguments": "{\"text\":\"left right\"}"}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}}))}))+"\n\ndata: {\"body\":\"[DONE]\",\"statusCodeValue\":200}\n\n")
				} else {
					io.WriteString(w, "event: output\ndata: {\"response\":\" 成功 \",\"reasoning_content\":\"思考\",\"tool_calls\":[{\"id\":\"call-one\",\"function_call\":{\"name\":\"echo\",\"arguments\":{\"text\":\"left right\"}}}]}\n\nevent: token_usage\ndata: {\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}\n\nevent: done\ndata: {}\n\n")
				}
			}))
			defer server.Close()
			cred := testCredential(t, c, server.URL, nil)
			req := &provider.ChatRequest{Model: "actual-model", Raw: []byte(`{"messages":[{"role":"system","content":"stale-system"}]}`), Messages: []provider.Message{{Role: "system", Content: marshal("新的系统提示")}, {Role: "user", Content: marshal("hi")}, {Role: "assistant", Content: []byte("null"), ToolCalls: []byte(`[{"id":"old-call","type":"function","function":{"name":"echo","arguments":"{}"}}]`)}, {Role: "tool", ToolCallID: "old-call", Content: marshal("真正的工具结果")}}, Tools: []byte(`[{"type":"function","function":{"name":"echo","parameters":{"type":"object"}}}]`)}
			result, err := c.ChatNonStream(context.Background(), cred, req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Message.ContentText() != " 成功 " || result.Message.ReasoningContent != "思考" || result.Usage.TotalTokens != 5 || !strings.Contains(string(result.Message.ToolCalls), "left right") || result.FinishReason != "tool_calls" {
				t.Fatalf("转换丢失：%+v", result)
			}
			if calls.Load() != 1 {
				t.Fatal("同一次推理不应重复上游调用")
			}
		})
	}
}

func TestNativeCatalogUsesSelectedAccount(t *testing.T) {
	for _, family := range []string{"qoder", "trae"} {
		t.Run(family, func(t *testing.T) {
			c := newClient(family, Config{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if family == "qoder" {
					raw, _ := io.ReadAll(r.Body)
					if r.Method != "GET" || string(decodeWire(t, raw)) != "{}" || r.Header.Get("Cosy-User") != "synthetic-user" {
						t.Error("目录签名字节或账号不符")
					}
					io.WriteString(w, `{"chat":[{"key":"per-account","display_name":"当前账号型号","enable":true,"is_reasoning":true,"max_input_tokens":1234},{"key":"hidden","enable":false}]}`)
				} else {
					io.WriteString(w, `{"config_info_list":[{"config_name":"per-account","display_config":{"display_name":"当前账号型号"},"context_window_tokens":{"dev":1234}},{"config_name":"hidden","is_invisible_to_user":true}]}`)
				}
			}))
			defer server.Close()
			cred := testCredential(t, c, server.URL, nil)
			models, err := c.FetchCatalog(context.Background(), cred)
			if err != nil || len(models) != 1 || models[0].NativeModel != "per-account" || models[0].ContextWindow != 1234 {
				t.Fatalf("目录错误 %v %+v", err, models)
			}
		})
	}
}

func TestStreamErrorsNeverBecomeSuccess(t *testing.T) {
	for _, family := range []string{"qoder", "trae"} {
		for _, scenario := range []string{"auth", "truncated", "empty", "late-error"} {
			t.Run(family+"/"+scenario, func(t *testing.T) {
				c := newClient(family, Config{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					if family == "qoder" {
						switch scenario {
						case "auth":
							io.WriteString(w, `data: {"statusCodeValue":401,"body":"dt-synthetic-access"}`+"\n\n")
						case "empty":
							io.WriteString(w, "data: [DONE]\n\n")
						default:
							io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"前半句\"}}]}\n\n")
							if scenario == "late-error" {
								io.WriteString(w, "event: error\ndata: {\"error\":\"dt-synthetic-access\"}\n\n")
							}
						}
					} else {
						switch scenario {
						case "auth":
							io.WriteString(w, "event: error\ndata: {\"code\":401,\"message\":\"synthetic-trae-access\"}\n\n")
						case "empty":
							io.WriteString(w, "event: done\ndata: {}\n\n")
						default:
							io.WriteString(w, "event: output\ndata: {\"response\":\"前半句\"}\n\n")
							if scenario == "late-error" {
								io.WriteString(w, "event: error\ndata: {\"code\":500,\"message\":\"synthetic-trae-access\"}\n\n")
							}
						}
					}
				}))
				defer server.Close()
				cred := testCredential(t, c, server.URL, nil)
				_, err := c.ChatNonStream(context.Background(), cred, &provider.ChatRequest{Model: "test", Messages: []provider.Message{{Role: "user", Content: marshal("hi")}}})
				if err == nil {
					t.Fatal("残缺或错误流不能报告成功")
				}
				if strings.Contains(err.Error(), cred.Field("access_token")) {
					t.Fatal("错误回显泄漏凭证")
				}
				if scenario == "auth" {
					f, ok := provider.AsFailure(err)
					if !ok || f.Status != 401 || f.KindHint != "auth" {
						t.Fatal("首帧认证错误未分类")
					}
				}
				if scenario == "truncated" && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("残缺流错误不符 %v", err)
				}
			})
		}
	}
}

func TestRefreshCoalescesAndPreservesConfiguration(t *testing.T) {
	for _, family := range []string{"qoder", "trae"} {
		t.Run(family, func(t *testing.T) {
			c := newClient(family, Config{})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				time.Sleep(40 * time.Millisecond)
				if family == "qoder" {
					if r.URL.Path != "/api/v1/deviceToken/refresh" {
						t.Error("刷新类型混用")
					}
					io.WriteString(w, `{"device_token":"dt-refreshed","refresh_token":"drt-refreshed","expires_in":3600000}`)
				} else {
					var body map[string]any
					json.NewDecoder(r.Body).Decode(&body)
					if body["ClientID"] != "en1oxy7wnw8j9n" {
						t.Error("Solo 刷新客户端错误")
					}
					io.WriteString(w, `{"Result":{"Token":"trae-refreshed","RefreshToken":"trae-refresh-new","TokenExpireDuration":3600}}`)
				}
			}))
			defer server.Close()
			cred := testCredential(t, c, server.URL, map[string]any{"models": []string{"custom-model"}, "expires_at": time.Now().Add(-time.Minute).Unix()})
			// 使用真实导入支持的 camelCase，验证刷新写回不会留下同名旧字段。
			var imported map[string]any
			json.Unmarshal(cred.Raw, &imported)
			for old, alias := range map[string]string{"access_token": "accessToken", "refresh_token": "refreshToken", "expires_at": "expiresAt", "machine_id": "machineId", "device_id": "deviceId"} {
				imported[alias] = imported[old]
				delete(imported, old)
			}
			accountID := cred.AccountID
			cred, err := c.DecodeCredential(marshal(imported))
			if err != nil {
				t.Fatal(err)
			}
			cred.AccountID = accountID
			var wg sync.WaitGroup
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					updated, err := c.Refresh(context.Background(), cred)
					if err != nil {
						t.Error(err)
						return
					}
					remaining := time.Until(updated.ExpiresAt)
					if remaining < 59*time.Minute || remaining > 61*time.Minute {
						t.Error("刷新时间单位错误")
					}
					if !strings.Contains(updated.Field("models"), "custom-model") || updated.Field("machine_id") != "original-machine" || updated.AccountID != cred.AccountID || string(updated.PreviousRaw) != string(cred.Raw) {
						t.Error("刷新丢失账号设置或 CAS 旧值")
					}
					var saved map[string]any
					json.Unmarshal(updated.Raw, &saved)
					if !strings.Contains(updated.Field("access_token"), "refreshed") || saved["accessToken"] != nil || saved["refreshToken"] != nil || saved["expiresAt"] != nil {
						t.Error("刷新写回保留旧别名或未读取新令牌")
					}
				}()
			}
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatalf("并发刷新重复调用 %d", calls.Load())
			}
		})
	}
}

func TestPATExchangeIdentityAndRefreshPolicy(t *testing.T) {
	c := NewQoder(Config{})
	var exchange, userCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobToken/exchange":
			exchange.Add(1)
			io.WriteString(w, `{"token":"jt-exchanged","expires_in":86400000}`)
		case "/api/v1/userinfo":
			userCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer jt-exchanged" {
				t.Error("用户信息令牌错误")
			}
			io.WriteString(w, `{"id":"pat-user","name":"本人账号"}`)
		default:
			t.Error("意外接口")
		}
	}))
	defer server.Close()
	cred, err := c.DecodeCredential(marshal(map[string]any{"personal_token": "pt-synthetic", "base_url": server.URL, "api_host": server.URL, "region": "global"}))
	if err != nil {
		t.Fatal(err)
	}
	cred.AccountID = "pat-account"
	if !c.NeedsRefresh(cred) {
		t.Fatal("PAT 首次使用没有交换策略")
	}
	updated, err := c.Refresh(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Field("uid") != "pat-user" || updated.Field("personal_token") != "pt-synthetic" || updated.Region != provider.RegionGlobal || exchange.Load() != 1 || userCalls.Load() != 1 || c.NeedsRefresh(updated) {
		t.Fatal("PAT 交换、账号归属或地区失效")
	}
	// PAT 交换可能不返回刷新令牌，到期后仍应直接交换，不能发送空 refresh_token。
	var expiredRaw map[string]any
	json.Unmarshal(updated.Raw, &expiredRaw)
	expiredRaw["expires_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	expired, err := c.DecodeCredential(marshal(expiredRaw))
	if err != nil {
		t.Fatal(err)
	}
	expired.AccountID = cred.AccountID
	if !c.NeedsRefresh(expired) {
		t.Fatal("仅 PAT 的访问令牌到期后没有续期策略")
	}
	renewed, err := c.Refresh(context.Background(), expired)
	if err != nil || c.NeedsRefresh(renewed) || renewed.Field("uid") != "pat-user" || exchange.Load() != 2 || userCalls.Load() != 2 {
		t.Fatalf("仅 PAT 的登录态没有成功重新交换：%v", err)
	}
}

func TestUnsupportedOptionsAndImagesFailBeforeUpstream(t *testing.T) {
	for _, family := range []string{"qoder", "trae"} {
		t.Run(family, func(t *testing.T) {
			c := newClient(family, Config{})
			cred := testCredential(t, c, "", nil)
			for _, raw := range []string{`{"tools":[{"type":"web_search"}]}`, `{"previous_response_id":"server-state"}`, `{"response_format":{"type":"json_schema"}}`} {
				var value struct {
					Tools json.RawMessage `json:"tools"`
				}
				json.Unmarshal([]byte(raw), &value)
				_, err := c.chatBody(cred, &provider.ChatRequest{Model: "test", Raw: []byte(raw), Tools: value.Tools, Messages: []provider.Message{{Role: "user", Content: marshal("hi")}}})
				f, ok := provider.AsFailure(err)
				if !ok || f.Status != 400 {
					t.Fatalf("无法转换的参数没有拒绝 %s %v", raw, err)
				}
			}
			request := &provider.ChatRequest{Model: "test", Messages: []provider.Message{{Role: "user", Content: []byte(`[{"type":"text","text":"图片问题"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]`)}}}
			body, err := c.chatBody(cred, request)
			if family == "trae" && err == nil {
				t.Fatal("Trae 图片被静默丢弃")
			}
			if family == "qoder" && (err != nil || !strings.Contains(string(body), `"is_vl":true`)) {
				t.Fatal("Qoder 图片能力未转换")
			}
		})
	}
}
