package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 使用真实 HTTP 传输检查同协议事件完整性，以及其他接口的转换与内部帧过滤。
func TestPlatformStreamsPreserveNativeEvents(t *testing.T) {
	for _, family := range []string{"claude", "openai", "codex"} {
		t.Run(family, func(t *testing.T) {
			s := newTestServerNoAdapter(t)
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if family == "claude" {
					for _, event := range []any{
						map[string]any{"type": "message_start", "message": map[string]any{"id": "native-msg", "type": "message", "role": "assistant", "content": []any{}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 0}}},
						map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": ""}},
						map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "思考"}},
						map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "signature_delta", "signature": "opaque-signature"}},
						map[string]any{"type": "content_block_stop", "index": 0},
						map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text", "text": ""}},
						map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": " 平台成功 "}},
						map[string]any{"type": "content_block_stop", "index": 1},
						map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 2}},
						map[string]any{"type": "message_stop"},
					} {
						io.WriteString(w, "data: "+string(mustJSON(event))+"\n\n")
					}
				} else {
					for _, event := range []any{
						map[string]any{"type": "response.created", "sequence_number": 0, "response": map[string]any{"id": "native-resp", "status": "in_progress", "output": []any{}}},
						map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"id": "reason-one", "type": "reasoning", "encrypted_content": "opaque-reasoning"}},
						map[string]any{"type": "response.output_text.delta", "output_index": 1, "content_index": 0, "delta": " 平台成功 "},
						map[string]any{"type": "response.completed", "sequence_number": 3, "response": map[string]any{"id": "native-resp", "status": "completed", "output": []any{map[string]any{"type": "reasoning", "encrypted_content": "opaque-reasoning"}, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": " 平台成功 "}}}}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 2}}},
					} {
						io.WriteString(w, "data: "+string(mustJSON(event))+"\n\n")
					}
				}
			}))
			defer fake.Close()
			cred := map[string]any{"base_url": fake.URL, "api_key": "synthetic"}
			if family == "codex" {
				delete(cred, "api_key")
				cred["access_token"] = "synthetic-access"
				cred["account_id"] = "synthetic-account"
			}
			createPlatformAccount(t, s, family, cred)
			for _, endpoint := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
				body := map[string]any{"model": family + "/model-test", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
				if endpoint == "/v1/responses" {
					delete(body, "messages")
					body["input"] = "hello"
				}
				response := callComplete(t, s, "POST", endpoint, string(mustJSON(body)))
				text := response.Body.String()
				if response.Code != 200 || !strings.Contains(text, "平台成功") || strings.Contains(text, "_relaypool_native_event") {
					t.Fatalf("流转换失败：%s %s", endpoint, text)
				}
				if family == "claude" && endpoint == "/v1/messages" {
					if !strings.Contains(text, "opaque-signature") || strings.Count(text, "event: message_start\n") != 1 || strings.Count(text, "event: content_block_stop\n") != 2 {
						t.Fatal("原生 Claude 签名或块生命周期丢失")
					}
				}
				if family != "claude" && endpoint == "/v1/responses" {
					if !strings.Contains(text, "opaque-reasoning") || strings.Count(text, "event: response.completed\n") != 1 || !strings.Contains(text, `"sequence_number":3`) {
						t.Fatal("原生 Responses 加密输出或事件编号丢失")
					}
				}
			}
		})
	}
}

func TestPlatformCrossProtocolImagesAndMultipleToolResults(t *testing.T) {
	s := newTestServerNoAdapter(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		raw := string(mustJSON(body))
		if !strings.Contains(raw, `"inlineData"`) || !strings.Contains(raw, `"functionResponse"`) || !strings.Contains(raw, `"name":"one"`) || !strings.Contains(raw, `"name":"two"`) {
			t.Errorf("图像或多个工具结果未到达 Gemini：%s", raw)
		}
		io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}]}`)
	}))
	defer fake.Close()
	createPlatformAccount(t, s, "gemini", map[string]any{"api_key": "synthetic", "base_url": fake.URL})
	for _, endpoint := range []string{"/v1/messages", "/v1/responses"} {
		body := `{"model":"gemini/model-test","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"one","input":{}},{"type":"tool_use","id":"c2","name":"two","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"first"},{"type":"tool_result","tool_use_id":"c2","content":"second"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}`
		if endpoint == "/v1/responses" {
			body = `{"model":"gemini/model-test","input":[{"type":"function_call","call_id":"c1","name":"one","arguments":"{}"},{"type":"function_call","call_id":"c2","name":"two","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"first"},{"type":"function_call_output","call_id":"c2","output":"second"},{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}]}`
		}
		response := callComplete(t, s, "POST", endpoint, body)
		if response.Code != 200 {
			t.Fatal(response.Body.String())
		}
	}
}
