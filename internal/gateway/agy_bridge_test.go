package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildAgyGeneratePayloadToolRoundTrip(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "gemini-3-flash",
		Messages: []ChatMessage{
			{Role: "system", Content: "Be brief"},
			{Role: "user", Content: "list files"},
			{Role: "assistant", ToolCalls: []ChatToolCall{{ID: "call_1", Type: "function", Function: ChatFunctionCall{Name: "run_cmd", Arguments: `{"cmd":"ls"}`}}}},
			{Role: "tool", ToolCallID: "call_1", Content: "a.txt"},
		},
		Tools: []ChatTool{{Type: "function", Function: ChatToolFunction{
			Name: "run_cmd",
			Parameters: map[string]any{
				"$schema":              "http://json-schema.org/draft-07/schema#",
				"type":                 "object",
				"additionalProperties": false,
				"properties":           map[string]any{"cmd": map[string]any{"type": "string"}},
			},
		}}},
	}
	payload := buildAgyGeneratePayload(req, "proj-1", "gemini-3-flash")
	raw, _ := json.Marshal(payload)
	s := string(raw)
	for _, want := range []string{`"project":"proj-1"`, `"functionResponse"`, `"name":"run_cmd"`, `"systemInstruction"`, `"functionDeclarations"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("payload missing %s: %s", want, s)
		}
	}
	if strings.Contains(s, "additionalProperties") || strings.Contains(s, "$schema") {
		t.Fatalf("schema not sanitized: %s", s)
	}
}

func TestStreamAgyToChatCompletions(t *testing.T) {
	body := strings.Join([]string{
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"hmm","thought":true}]}}]},"traceId":"t1"}`,
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi"},{"functionCall":{"id":"c1","name":"run_cmd","args":{"cmd":"ls"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7}}}`,
		"",
	}, "\n")
	rec := httptest.NewRecorder()
	streamAgyToChatCompletions(rec, strings.NewReader(body), "gemini-3-flash")
	out := rec.Body.String()
	for _, want := range []string{`"reasoning_content":"hmm"`, `"content":"Hi"`, `"name":"run_cmd"`, `"finish_reason":"tool_calls"`, `"total_tokens":7`, "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream missing %s:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "data: {") && !strings.Contains(line, `"choices":[{`) {
			t.Fatalf("chunk without choices: %s", line)
		}
	}
}
