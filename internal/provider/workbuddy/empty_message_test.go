package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/relaypool/relaypool/internal/pool"
)

// 回归实号 Kimi 的 11151：补充非空系统消息，但保留用户消息和工具调用。
func TestPrepareBodyRepairsEmptySystemOnly(t *testing.T) {
	cases := []struct {
		name    string
		content any
	}{
		{"空字符串", ""}, {"空白", " \n\t"}, {"空值", nil},
		{"空数组", []any{}},
		{"空文本块", []any{map[string]any{"type": "text", "text": " "}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			system := map[string]any{"role": "system", "content": tc.content, "name": "original"}
			user := map[string]any{"role": "user", "content": "只回复收到"}
			tool := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_test"}}}
			input, _ := json.Marshal(map[string]any{"messages": []any{system, user, tool}})
			var prepared map[string]any
			if err := json.Unmarshal(PrepareBody(input), &prepared); err != nil {
				t.Fatal(err)
			}
			messages := prepared["messages"].([]any)
			first := messages[0].(map[string]any)
			content, ok := first["content"].(string)
			if !ok || strings.TrimSpace(content) == "" || first["name"] != "original" {
				t.Fatalf("未正确补充首条系统消息：%v", first)
			}
			if messages[1].(map[string]any)["content"] != user["content"] || messages[2].(map[string]any)["content"] != nil {
				t.Fatal("用户消息或合法工具调用被改写")
			}
		})
	}
	// 非空文本块中的原指令同样必须保留。
	content := []any{map[string]any{"type": "text", "text": "按原有规则作答"}}
	first := map[string]any{"role": "system", "content": content}
	body := map[string]any{"messages": []any{first}}
	ensureLeadingSystem(body)
	got, _ := json.Marshal(body["messages"])
	want, _ := json.Marshal([]any{first})
	if string(got) != string(want) {
		t.Fatal("非空系统正文被改写")
	}
}

// 请求错误应保留健康账号，不应换号或产生账号冷却。
func TestEmptyMessageFailureDoesNotCoolAccount(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":11151,"message":"a message has empty content, please check the conversation history and retry"}}`,
		`{"error":{"data":{"code":"11151"}}}`,
		`{"error":{"message":"a message has empty content"}}`,
	} {
		failure := classifyHTTP(400, "", body)
		classified := pool.Classify(failure.Status, failure.Body, "", failure.KindHint, pool.ClassifyOptions{})
		if classified.Kind != pool.KindInvalidRequest || classified.Failover || classified.Cooldown != 0 {
			t.Fatalf("空消息错误不应影响健康账号：%+v", classified)
		}
	}
}
