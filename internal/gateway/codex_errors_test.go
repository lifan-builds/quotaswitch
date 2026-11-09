package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lifan-builds/quotaswitch/internal/codex"
)

const completedCodexTestStream = `data: {"type":"response.output_text.delta","delta":"healthy reply"}

data: {"type":"response.completed","response":{"usage":{"input_tokens":2,"output_tokens":3}}}

`

func TestCodexToolHistoryErrorIsNotMaskedByQuota(t *testing.T) {
	for _, stream := range []bool{false, true} {
		failed := "data: " + `{"type":"error","message":"No tool output found for function call call_history."}` + "\n\n"
		quota := "data: " + `{"type":"error","code":"usage_limit_reached","message":"usage limit reached"}` + "\n\n"
		srv, calls := codexErrorTestServer(t, failed, quota)
		rec := codexErrorTestRequest(srv, stream)
		if calls.Load() != 1 || rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "No tool output found") {
			t.Fatalf("history error masked: stream=%t calls=%d status=%d body=%s", stream, calls.Load(), rec.Code, rec.Body.String())
		}
	}
}

func codexErrorTestServer(t *testing.T, first, second string) (*Server, *atomic.Int32) {
	t.Helper()
	tmp := t.TempDir()
	store := codex.NewStore(filepath.Join(tmp, "switcher"), filepath.Join(tmp, "codex"))
	active := "acc-1"
	if err := store.Save(&codex.AccountsStore{
		ActiveAccountID: &active,
		Accounts: []codex.StoredAccount{
			{ID: active, Name: "account 1", AuthMode: codex.AuthModeChatGPT, AuthData: codex.AuthData{AccessToken: "test-token-1"}},
			{ID: "acc-2", Name: "account 2", AuthMode: codex.AuthModeChatGPT, AuthData: codex.AuthData{AccessToken: "test-token-2"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		body := second
		if calls.Add(1) == 1 {
			body = first
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)
	srv := NewServer(NewPool(store, nil, nil, nil), tmp, "test")
	srv.CodexResponsesURL = upstream.URL
	return srv, calls
}

func codexErrorTestRequest(srv *Server, stream bool) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"model":"gpt-6.1-sol","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

func TestCodexStreamFailureBeforeOutputFailsOver(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			failed := `data: {"type":"response.created","response":{"id":"failed_response"}}

data: {"type":"response.failed","response":{"error":{"code":"usage_limit_reached","message":"quota unavailable"}}}

`
			srv, calls := codexErrorTestServer(t, failed, completedCodexTestStream)
			rec := codexErrorTestRequest(srv, stream)
			if calls.Load() != 2 || rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "healthy reply") {
				t.Fatalf("expected healthy failover: calls=%d status=%d body=%s", calls.Load(), rec.Code, rec.Body.String())
			}
			if rec.Header().Get("X-Quotaswitch-Account") != "account 2" || strings.Contains(rec.Body.String(), "failed_response") {
				t.Fatalf("failed account leaked into success: headers=%v body=%s", rec.Header(), rec.Body.String())
			}
		})
	}
}

func TestCodexFailureAfterOutputIsNotReplayed(t *testing.T) {
	for name, partial := range map[string]string{
		"text":      `{"type":"response.output_text.delta","delta":"partial reply"}`,
		"reasoning": `{"type":"response.reasoning_summary_text.delta","delta":"partial reasoning"}`,
		"tool":      `{"type":"response.output_item.added","item":{"id":"fc_1","call_id":"call_1","type":"function_call","name":"run_cmd"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			failed := "data: " + partial + "\n\ndata: " + `{"type":"response.failed","response":{"error":{"code":"server_error","message":"upstream crashed"}}}` + "\n\n"
			srv, calls := codexErrorTestServer(t, failed, completedCodexTestStream)
			rec := codexErrorTestRequest(srv, true)
			out := rec.Body.String()
			if calls.Load() != 1 || !strings.Contains(out, `"error":`) || !strings.Contains(out, "upstream crashed") {
				t.Fatalf("expected unreplayed visible error: calls=%d body=%s", calls.Load(), out)
			}
			if strings.Contains(out, "[DONE]") || strings.Contains(out, `"finish_reason":"stop"`) || strings.Contains(out, `"finish_reason":"tool_calls"`) || strings.Contains(out, "healthy reply") {
				t.Fatalf("failed stream advertised successful completion: %s", out)
			}
		})
	}
}

func TestCodexFailedStreamsNeverBecomeEmptySuccess(t *testing.T) {
	for name, body := range map[string]string{
		"empty":        "",
		"done only":    "data: [DONE]\n\n",
		"malformed":    "data: {broken JSON}\n\n",
		"missing type": "data: {}\n\n",
		"error":        "data: " + `{"type":"error","code":"server_error","message":"upstream crashed"}` + "\n\n",
		"nested error": "data: " + `{"error":{"code":"server_error","message":"upstream crashed"}}` + "\n\n",
		"failed":       "data: " + `{"type":"response.failed","response":{"error":{"code":"server_error","message":"upstream crashed"}}}` + "\n\n",
		"incomplete":   "data: " + `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}` + "\n\n",
		"truncated":    "data: " + `{"type":"response.output_text.delta","delta":"partial reply"}` + "\n\n",
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", name, stream), func(t *testing.T) {
				srv, _ := codexErrorTestServer(t, body, body)
				rec := codexErrorTestRequest(srv, stream)
				out := rec.Body.String()
				if !strings.Contains(out, `"error":`) {
					t.Fatalf("missing visible failure: status=%d body=%s", rec.Code, out)
				}
				if strings.Contains(out, "[DONE]") || strings.Contains(out, `"finish_reason":"stop"`) {
					t.Fatalf("failed stream advertised success: %s", out)
				}
				if !(stream && name == "truncated") && rec.Code != http.StatusBadGateway {
					t.Fatalf("expected 502 before response output, got %d: %s", rec.Code, out)
				}
			})
		}
	}
}

func TestCodexNonRetryableStreamFailure(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body := "data: " + `{"type":"error","code":"invalid_request_error","message":"bad tool schema"}` + "\n\n"
			srv, calls := codexErrorTestServer(t, body, completedCodexTestStream)
			rec := codexErrorTestRequest(srv, stream)
			if calls.Load() != 1 || rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad tool schema") {
				t.Fatalf("invalid request was retried or hidden: calls=%d status=%d body=%s", calls.Load(), rec.Code, rec.Body.String())
			}
		})
	}
}

type brokenCodexReader struct{}

func (brokenCodexReader) Read([]byte) (int, error) { return 0, errors.New("broken transport") }

func TestCodexScannerErrorIsReported(t *testing.T) {
	for _, stream := range []bool{false, true} {
		rec := httptest.NewRecorder()
		var committed bool
		var err error
		if stream {
			committed, err = streamCodexToChatCompletions(rec, brokenCodexReader{}, "gpt-test")
		} else {
			committed, err = collectCodexToChatCompletion(rec, brokenCodexReader{}, "gpt-test")
		}
		if committed || err == nil || !strings.Contains(err.Error(), "broken transport") || rec.Body.Len() != 0 {
			t.Fatalf("transport error hidden: stream=%t committed=%t err=%v body=%s", stream, committed, err, rec.Body.String())
		}
	}
}

type codexEndpointTransport func(*http.Request) (*http.Response, error)

func (f codexEndpointTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCodexDefaultEndpointDependsOnAccountAuth(t *testing.T) {
	for _, tc := range []struct {
		name, authMode, endpoint, override string
	}{
		{"oauth", codex.AuthModeChatGPT, DefaultCodexResponsesURL, ""},
		{"api key", codex.AuthModeAPIKey, DefaultOpenAIResponsesURL, ""},
		{"api key override", codex.AuthModeAPIKey, "http://127.0.0.1/test", "http://127.0.0.1/test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(nil, t.TempDir(), "test")
			if tc.override != "" {
				srv.CodexResponsesURL = tc.override
			}
			srv.HTTPClient = &http.Client{Transport: codexEndpointTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != tc.endpoint {
					t.Errorf("wrong endpoint for %s: got %s want %s", tc.name, r.URL, tc.endpoint)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
			})}
			resp, _, err := srv.doCodexResponsesRequest(context.Background(), CodexCandidate{AuthMode: tc.authMode, AccessToken: "oauth-test", APIKey: "api-test"}, []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		})
	}
}
