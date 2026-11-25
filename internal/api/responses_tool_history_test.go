package api

import (
	"encoding/json"
	"testing"
)

func TestResponsesParallelToolHistory(t *testing.T) {
	input := json.RawMessage(`[
		{"type":"function_call","call_id":"a","name":"alpha","arguments":"{}"},
		{"type":"reasoning","encrypted_content":"opaque"},
		{"type":"function_call","call_id":"b","name":"beta","arguments":"{}"},
		{"type":"function_call_output","call_id":"b","output":"second"},
		{"type":"function_call_output","call_id":"a","output":"first"},
		{"type":"function_call","call_id":"c","name":"gamma","arguments":"{}"},
		{"type":"function_call_output","call_id":"c","output":"third"}
	]`)
	messages := parseResponsesInput(input)
	if len(messages) != 5 {
		t.Fatalf("got %d messages, want 5", len(messages))
	}
	for i, ids := range map[int][]string{0: {"a", "b"}, 3: {"c"}} {
		var calls []struct{ ID string }
		if messages[i].Role != "assistant" || json.Unmarshal(messages[i].ToolCalls, &calls) != nil || len(calls) != len(ids) {
			t.Fatalf("incorrect tool call group: %+v", messages[i])
		}
		for j, id := range ids {
			if calls[j].ID != id {
				t.Fatalf("wrong call ID: %v", calls)
			}
		}
	}
	for i, id := range map[int]string{1: "b", 2: "a", 4: "c"} {
		if messages[i].Role != "tool" || messages[i].ToolCallID != id {
			t.Fatalf("wrong tool result: %+v", messages[i])
		}
	}
}

func TestResponsesToolOutputContent(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"text parts", `[{"type":"input_text","text":"one "},{"type":"input_text","text":"two"}]`, `"one two"`},
		{"mixed media", `[{"type":"input_text","text":"caption"},{"type":"input_image","image_url":"https://example.test/image","detail":"low"}]`, `[{"text":"caption","type":"text"},{"image_url":{"detail":"low","url":"https://example.test/image"},"type":"image_url"}]`},
		{"string", `"ok"`, `"ok"`},
		{"opaque array", `[{"value":9876543212345678901}]`, `[{"value":9876543212345678901}]`},
		{"null", `null`, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := json.RawMessage(`[{"type":"function_call_output","call_id":"test","output":` + tc.input + `}]`)
			req, err := parseResponsesRequest([]byte(`{"model":"m","input":` + string(input) + `}`))
			if err != nil || string(req.NativeInput) != string(input) {
				t.Fatalf("original Responses input was lost: %v", err)
			}
			got := req.Messages
			if len(got) != 1 || got[0].ToolCallID != "test" || got[0].Role != "tool" || string(got[0].Content) != tc.want {
				t.Fatalf("unexpected tool result: %+v", got)
			}
		})
	}
}
