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
