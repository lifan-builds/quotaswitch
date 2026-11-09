package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Server struct {
	Pool      *Pool
	CodexHome string
	APIKey    string
	// CodexResponsesURL overrides both the OAuth and API-key upstream endpoints.
	CodexResponsesURL string
	AgyEndpoint       string
	HTTPClient        *http.Client
	// Logf receives request metadata only, never prompts, tool arguments, or tokens.
	Logf func(string, ...any)
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func NewServer(pool *Pool, codexHome, apiKey string) *Server {
	return &Server{
		Pool:        pool,
		CodexHome:   codexHome,
		APIKey:      apiKey,
		AgyEndpoint: DefaultAgyEndpoint,
		HTTPClient:  &http.Client{Timeout: 10 * time.Minute},
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && (path == "/healthz" || path == "/health" || path == "/"):
		s.handleHealth(w, r)
	case r.Method == http.MethodGet && (path == "/v1/models" || path == "/models"):
		s.handleModels(w, r)
	case r.Method == http.MethodPost && (path == "/v1/chat/completions" || path == "/chat/completions"):
		s.handleChatCompletions(w, r)
	case r.Method == http.MethodPost && (path == "/v1/responses" || path == "/responses"):
		s.handleCodexResponsesPassthrough(w, r)
	default:
		writeOpenAIError(w, http.StatusNotFound, "not_found", fmt.Sprintf("unknown gateway route %s %s", r.Method, path))
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	codexCount, agyCount := 0, 0
	if s.Pool != nil {
		codexCount, agyCount = s.Pool.Summary()
	}
	models := CombinedModelEntries(s.CodexHome)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":             true,
		"service":        "quotaswitch-gateway",
		"codex_accounts": codexCount,
		"agy_accounts":   agyCount,
		"models_count":   len(models),
	})
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	entries := CombinedModelEntries(s.CodexHome)
	data := make([]map[string]any, 0, len(entries))
	now := time.Now().Unix()
	for _, e := range entries {
		data = append(data, map[string]any{
			"id":           e.ModelID,
			"object":       "model",
			"created":      now,
			"owned_by":     e.Provider,
			"display_name": e.DisplayName,
			"description":  e.Description,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
	})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
		return
	}
	var req ChatCompletionRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("invalid JSON: %v", err))
		return
	}

	backend, upstreamModel, defaultEffort := ResolveModel(req.Model, s.CodexHome)
	s.logf("request provider=%s model=%q bytes=%d messages=%d tools=%d", backend, upstreamModel, len(bodyBytes), len(req.Messages), len(req.Tools))
	if req.Model == "" {
		req.Model = upstreamModel
	}
	switch backend {
	case BackendAgy:
		s.handleAgyChatCompletions(w, r, req, upstreamModel)
	default:
		s.handleCodexChatCompletions(w, r, req, upstreamModel, defaultEffort)
	}
}

func (s *Server) handleCodexResponsesPassthrough(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
		return
	}
	var rawMap map[string]any
	if err := json.Unmarshal(bodyBytes, &rawMap); err == nil {
		if m, ok := rawMap["model"].(string); ok {
			_, upstream, _ := ResolveModel(m, s.CodexHome)
			rawMap["model"] = upstream
		}
		if _, hasStore := rawMap["store"]; !hasStore {
			rawMap["store"] = false
		}
		if _, hasInst := rawMap["instructions"]; !hasInst {
			rawMap["instructions"] = "You are a helpful coding assistant."
		}
		if updated, mErr := json.Marshal(rawMap); mErr == nil {
			bodyBytes = updated
		}
	}

	candidates, err := s.Pool.CodexCandidates()
	if err != nil || len(candidates) == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_codex_accounts", fmt.Sprintf("no Codex accounts available: %v", err))
		return
	}

	var lastStatus int
	var lastErr error
	for i := range candidates {
		cand := candidates[i]
		if err := s.Pool.EnsureCodexToken(r.Context(), &cand, false); err != nil {
			lastErr = err
			s.Pool.MarkCooldown(BackendCodex, cand.ID, defaultCooldown)
			continue
		}
		resp, status, doErr := s.doCodexResponsesRequest(r.Context(), cand, bodyBytes)
		if doErr != nil {
			lastStatus = status
			lastErr = doErr
			if shouldFailoverStatus(status, doErr.Error()) {
				s.Pool.MarkCooldown(BackendCodex, cand.ID, cooldownForStatus(status))
				continue
			}
			writeOpenAIError(w, nonZeroStatus(status, http.StatusBadGateway), "upstream_error", doErr.Error())
			return
		}
		s.Pool.MarkHealthy(BackendCodex, cand.ID)
		defer resp.Body.Close()
		for k, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("X-Quotaswitch-Provider", BackendCodex)
		w.Header().Set("X-Quotaswitch-Account", cand.DisplayName)
		w.WriteHeader(resp.StatusCode)
		if flusher, ok := w.(http.Flusher); ok {
			buf := make([]byte, 16*1024)
			for {
				n, rErr := resp.Body.Read(buf)
				if n > 0 {
					_, _ = w.Write(bytes.Clone(buf[:n]))
					flusher.Flush()
				}
				if rErr != nil {
					break
				}
			}
		} else {
			_, _ = io.Copy(w, resp.Body)
		}
		return
	}

	writeOpenAIError(w, nonZeroStatus(lastStatus, http.StatusTooManyRequests), "quota_exhausted", fmt.Sprintf("Codex pool exhausted: %v", lastErr))
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}

func shouldFailoverStatus(status int, errMsg string) bool {
	if status == 0 || status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests || status >= 500 {
		return true
	}
	lower := strings.ToLower(errMsg)
	return strings.Contains(lower, "quota") ||
		strings.Contains(lower, "rate_limit") ||
		strings.Contains(lower, "usage_limit") ||
		strings.Contains(lower, "resource_exhausted") ||
		strings.Contains(lower, "exhausted")
}

func cooldownForStatus(status int) time.Duration {
	switch status {
	case http.StatusTooManyRequests:
		return 5 * time.Minute
	case http.StatusUnauthorized, http.StatusForbidden:
		return 10 * time.Minute
	default:
		return defaultCooldown
	}
}

func nonZeroStatus(status, fallback int) int {
	if status > 0 {
		return status
	}
	return fallback
}
