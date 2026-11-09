package gateway

import (
	"strings"
	"testing"
)

func TestCodexInterruptedToolHistory(t *testing.T) {
	req := ChatCompletionRequest{Messages: []ChatMessage{
		{Role: "user", Content: "Read status"},
		{Role: "assistant", ToolCalls: []ChatToolCall{
			{ID: "call_missing", Function: ChatFunctionCall{Name: "check_status", Arguments: `{}`}},
			{ID: "call_complete", Function: ChatFunctionCall{Name: "read_file", Arguments: `{}`}},
		}},
		{Role: "tool", ToolCallID: "call_complete", Content: "real result"},
		{Role: "user", Content: "Is it fixed now?"},
	}}
	payload := buildCodexResponsesPayload(req, "gpt-6-astra", "low")
	results := make(map[string]string)
	for i, raw := range payload.Input {
		item := raw.(map[string]any)
		if item["type"] == "function_call_output" {
			id := item["call_id"].(string)
			if _, duplicate := results[id]; duplicate {
				t.Fatalf("duplicate result for %s", id)
			}
			results[id] = item["output"].(string)
			if id == "call_missing" && payload.Input[i-1].(map[string]any)["call_id"] != id {
				t.Fatal("missing result must follow the original call")
			}
		}
	}
	if len(payload.Input) != 6 || len(results) != 2 || results["call_complete"] != "real result" {
		t.Fatalf("history changed unexpectedly: %#v", payload.Input)
	}
	if !strings.Contains(results["call_missing"], "outcome are unknown") || !strings.Contains(results["call_missing"], "not evidence of success") {
		t.Fatalf("missing result must not imply completion: %q", results["call_missing"])
	}
}

func TestCodexEmptyRecordedToolResultIsPreserved(t *testing.T) {
	req := ChatCompletionRequest{Messages: []ChatMessage{
		{Role: "assistant", ToolCalls: []ChatToolCall{{ID: "call_empty", Function: ChatFunctionCall{Name: "check"}}}},
		{Role: "tool", ToolCallID: "call_empty", Content: ""},
	}}
	payload := buildCodexResponsesPayload(req, "gpt-6-astra", "low")
	if len(payload.Input) != 2 || payload.Input[1].(map[string]any)["output"] != "" {
		t.Fatalf("an empty recorded result is valid: %#v", payload.Input)
	}
}
