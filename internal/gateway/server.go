package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lifan-builds/quotaswitch/internal/codex"
)

type Server struct {
	Pool      *Pool
	CodexHome string
	APIKey    string
	// CodexResponsesURL overrides both the OAuth and API-key upstream endpoints.
	CodexResponsesURL string
	// OpenAIBaseURL overrides https://api.openai.com for Platform passthrough endpoints.
	OpenAIBaseURL string
	AgyEndpoint   string
	HTTPClient    *http.Client
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
	case r.Method == http.MethodGet && (path == "/v1/credits" || path == "/credits"):
		s.handleCredits(w, r)
	case r.Method == http.MethodGet && (path == "/v1/models" || path == "/models"):
		s.handleModels(w, r)
	case r.Method == http.MethodPost && (path == "/v1/chat/completions" || path == "/chat/completions"):
		s.handleChatCompletions(w, r)
	case r.Method == http.MethodPost && (path == "/v1/responses" || path == "/responses"):
		s.handleCodexResponsesPassthrough(w, r)
	case isOpenAIPlatformRoute(path):
		s.handleOpenAIPlatformPassthrough(w, r)
	default:
		writeOpenAIError(w, http.StatusNotFound, "not_found", fmt.Sprintf("unknown gateway route %s %s", r.Method, path))
	}
}

func isOpenAIPlatformRoute(path string) bool {
	norm := path
	if !strings.HasPrefix(norm, "/v1/") {
		norm = "/v1" + norm
	}
	for _, prefix := range []string{
		"/v1/images/",
		"/v1/audio/",
		"/v1/embeddings",
		"/v1/files",
		"/v1/batches",
		"/v1/moderations",
		"/v1/videos",
	} {
		if strings.HasPrefix(norm, prefix) {
			return true
		}
	}
	return false
}

func setQuotaswitchHeaders(w http.ResponseWriter, provider, account, accountID string) {
	w.Header().Set("X-Quotaswitch-Provider", provider)
	w.Header().Set("X-Quotaswitch-Account", account)
	if accountID != "" {
		w.Header().Set("X-Quotaswitch-Account-ID", accountID)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	codexCount, agyCount := 0, 0
	var creditRem, creditGrant float64
	if s.Pool != nil {
		codexCount, agyCount = s.Pool.Summary()
		if cr, err := s.Pool.CreditsSummary(); err == nil && cr != nil {
			creditRem = cr.TotalRemainingUSD
			creditGrant = cr.TotalGrantUSD
		}
	}
	models := CombinedModelEntries(s.CodexHome)
	resp := map[string]any{
		"ok":             true,
		"service":        "quotaswitch-gateway",
		"codex_accounts": codexCount,
		"agy_accounts":   agyCount,
		"models_count":   len(models),
	}
	if creditGrant > 0 {
		resp["api_credits_remaining_usd"] = creditRem
		resp["api_credits_grant_usd"] = creditGrant
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleCredits(w http.ResponseWriter, _ *http.Request) {
	if s.Pool == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "pool_unavailable", "gateway pool is not configured")
		return
	}
	summary, err := s.Pool.CreditsSummary()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "credits_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(summary)
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
	var reqModel string
	var rawMap map[string]any
	if err := json.Unmarshal(bodyBytes, &rawMap); err == nil {
		if m, ok := rawMap["model"].(string); ok {
			_, upstream, _ := ResolveModel(m, s.CodexHome)
			rawMap["model"] = upstream
			reqModel = upstream
		}
		if _, hasStore := rawMap["store"]; !hasStore {
			rawMap["store"] = false
		}
		if _, hasInst := rawMap["instructions"]; !hasInst {
			rawMap["instructions"] = defaultDirectInstructions
		}
		if updated, mErr := json.Marshal(rawMap); mErr == nil {
			bodyBytes = updated
		}
	}

	acctFilter := r.Header.Get("X-Quotaswitch-Account")
	authPref := r.Header.Get("X-Quotaswitch-Auth")
	candidates, err := s.Pool.CodexCandidatesWithMode(acctFilter, authPref)
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
			if cand.AuthMode == codex.AuthModeAPIKey && isQuotaHardExhausted(doErr.Error()) {
				s.Pool.MarkAPIKeyExhausted(cand.Account)
			}
			if cand.AuthMode != codex.AuthModeAPIKey && isOAuthUnsupportedModelError(status, doErr.Error()) {
				continue
			}
			if shouldFailoverStatus(status, doErr.Error()) {
				s.Pool.MarkCooldown(BackendCodex, cand.ID, cooldownForError(status, doErr.Error()))
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
		setQuotaswitchHeaders(w, BackendCodex, cand.DisplayName, cand.Account.Name)
		w.WriteHeader(resp.StatusCode)

		var capture bytes.Buffer
		const maxUsageCapture = 4 * 1024 * 1024
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 16*1024)
		for {
			n, rErr := resp.Body.Read(buf)
			if n > 0 {
				if cand.AuthMode == codex.AuthModeAPIKey && capture.Len() < maxUsageCapture {
					rem := maxUsageCapture - capture.Len()
					if rem > n {
						rem = n
					}
					_, _ = capture.Write(buf[:rem])
				}
				_, _ = w.Write(buf[:n])
				if flusher != nil {
					flusher.Flush()
				}
			}
			if rErr != nil {
				break
			}
		}
		if cand.AuthMode == codex.AuthModeAPIKey {
			u := extractPassthroughResponsesUsage(capture.Bytes())
			var inTok, cachedTok, outTok int
			if u != nil {
				inTok = u.PromptTokens
				cachedTok = u.CachedTokens
				outTok = u.CompletionTokens
			}
			s.Pool.RecordAPIKeyUsage(cand.Account, codex.CreditCallDelta{
				Model:        reqModel,
				CostUSD:      codex.EstimateTokenCostUSD(reqModel, inTok, cachedTok, outTok, false),
				Requests:     1,
				InputTokens:  int64(inTok),
				CachedTokens: int64(cachedTok),
				OutputTokens: int64(outTok),
			})
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

func (s *Server) handleOpenAIPlatformPassthrough(w http.ResponseWriter, r *http.Request) {
	const maxPlatformBodyBytes = 128 * 1024 * 1024
	var bodyBytes []byte
	if r.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(io.LimitReader(r.Body, maxPlatformBodyBytes))
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
			return
		}
	}

	acctFilter := r.Header.Get("X-Quotaswitch-Account")
	candidates, err := s.Pool.OpenAIAPIKeyCandidates(acctFilter)
	if err != nil || len(candidates) == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_api_key_accounts", fmt.Sprintf("no OpenAI API key accounts available: %v", err))
		return
	}

	baseURL := strings.TrimRight(strings.TrimSpace(s.OpenAIBaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	upPath := r.URL.Path
	if !strings.HasPrefix(upPath, "/v1/") {
		upPath = "/v1" + upPath
	}
	targetURL := baseURL + upPath
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}

	var lastStatus int
	var lastErr error
	for i := range candidates {
		cand := candidates[i]
		var reqReader io.Reader
		if len(bodyBytes) > 0 {
			reqReader = bytes.NewReader(bodyBytes)
		}
		upReq, reqErr := http.NewRequestWithContext(r.Context(), r.Method, targetURL, reqReader)
		if reqErr != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", reqErr.Error())
			return
		}
		for k, vals := range r.Header {
			lk := strings.ToLower(k)
			if lk == "authorization" || lk == "host" || lk == "content-length" || strings.HasPrefix(lk, "x-quotaswitch-") {
				continue
			}
			for _, v := range vals {
				upReq.Header.Add(k, v)
			}
		}
		upReq.Header.Set("Authorization", "Bearer "+cand.APIKey)

		resp, doErr := client.Do(upReq)
		if doErr != nil {
			lastErr = doErr
			s.Pool.MarkCooldown(BackendCodex, cand.ID, defaultCooldown)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()
			errMsg := strings.TrimSpace(string(errBytes))
			if errMsg == "" {
				errMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
			lastStatus = resp.StatusCode
			lastErr = fmt.Errorf("OpenAI Platform HTTP %d: %s", resp.StatusCode, errMsg)
			s.logf("platform method=%s path=%q candidate=%d status=%d", r.Method, upPath, i+1, resp.StatusCode)
			if isQuotaHardExhausted(errMsg) {
				s.Pool.MarkAPIKeyExhausted(cand.Account)
			}
			if acctFilter == "" && shouldFailoverStatus(resp.StatusCode, errMsg) {
				s.Pool.MarkCooldown(BackendCodex, cand.ID, cooldownForError(resp.StatusCode, errMsg))
				continue
			}
			w.Header().Set("Content-Type", "application/json")
			setQuotaswitchHeaders(w, "openai-api-key", cand.DisplayName, cand.Account.Name)
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(errBytes)
			return
		}

		s.Pool.MarkHealthy(BackendCodex, cand.ID)
		defer resp.Body.Close()
		for k, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		setQuotaswitchHeaders(w, "openai-api-key", cand.DisplayName, cand.Account.Name)
		w.WriteHeader(resp.StatusCode)

		var embedBuf bytes.Buffer
		var bodyReader io.Reader = resp.Body
		if r.Method == http.MethodPost && strings.HasPrefix(upPath, "/v1/embeddings") {
			bodyReader = io.TeeReader(resp.Body, &limitedBuffer{buf: &embedBuf, max: 2 * 1024 * 1024})
		}
		_, _ = io.Copy(w, bodyReader)
		if r.Method == http.MethodPost {
			if delta, ok := buildPlatformCreditDelta(upPath, bodyBytes, embedBuf.Bytes()); ok {
				s.Pool.RecordAPIKeyUsage(cand.Account, delta)
			}
		}
		return
	}

	writeOpenAIError(w, nonZeroStatus(lastStatus, http.StatusTooManyRequests), "quota_exhausted", fmt.Sprintf("OpenAI API key pool exhausted: %v", lastErr))
}

type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	origLen := len(p)
	if l.buf.Len() < l.max {
		rem := l.max - l.buf.Len()
		if len(p) > rem {
			p = p[:rem]
		}
		_, _ = l.buf.Write(p)
	}
	return origLen, nil
}

func extractPassthroughResponsesUsage(raw []byte) *ChatCompletionUsage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '{' {
		var obj map[string]any
		if json.Unmarshal(trimmed, &obj) == nil {
			if u := parseResponsesUsage(obj["usage"]); u != nil {
				return u
			}
		}
	}
	for _, line := range bytes.Split(trimmed, []byte("\n")) {
		s := strings.TrimSpace(string(line))
		if !strings.HasPrefix(s, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
		if payload == "" || payload == "[DONE]" || !strings.Contains(payload, "response.completed") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(payload), &ev) == nil {
			if respObj, ok := ev["response"].(map[string]any); ok {
				if u := parseResponsesUsage(respObj["usage"]); u != nil {
					return u
				}
			}
		}
	}
	return nil
}

func buildPlatformCreditDelta(upPath string, reqBody, respBody []byte) (codex.CreditCallDelta, bool) {
	switch {
	case strings.HasPrefix(upPath, "/v1/embeddings"):
		var reqObj struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(reqBody, &reqObj)
		var respObj struct {
			Model string `json:"model"`
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
				TotalTokens  int `json:"total_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(respBody, &respObj)
		toks := respObj.Usage.TotalTokens
		if toks == 0 {
			toks = respObj.Usage.PromptTokens
		}
		m := reqObj.Model
		if m == "" {
			m = respObj.Model
		}
		return codex.CreditCallDelta{
			Model:           m,
			CostUSD:         codex.EstimateEmbeddingCostUSD(m, toks),
			Requests:        1,
			EmbeddingTokens: int64(toks),
		}, true
	case strings.HasPrefix(upPath, "/v1/images/generations"), strings.HasPrefix(upPath, "/v1/images/edits"):
		var reqObj struct {
			Model   string `json:"model"`
			Quality string `json:"quality"`
			Size    string `json:"size"`
			N       int    `json:"n"`
		}
		_ = json.Unmarshal(reqBody, &reqObj)
		n := reqObj.N
		if n <= 0 {
			n = 1
		}
		return codex.CreditCallDelta{
			Model:    reqObj.Model,
			CostUSD:  codex.EstimateImageCostUSD(reqObj.Model, reqObj.Quality, reqObj.Size, n),
			Requests: 1,
			Images:   int64(n),
		}, true
	case strings.HasPrefix(upPath, "/v1/audio/speech"):
		var reqObj struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}
		_ = json.Unmarshal(reqBody, &reqObj)
		chars := utf8.RuneCountInString(reqObj.Input)
		return codex.CreditCallDelta{
			Model:      reqObj.Model,
			CostUSD:    codex.EstimateAudioSpeechCostUSD(reqObj.Model, chars),
			Requests:   1,
			AudioChars: int64(chars),
		}, true
	default:
		return codex.CreditCallDelta{}, false
	}
}

func isQuotaHardExhausted(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	return strings.Contains(lower, "insufficient_quota") ||
		strings.Contains(lower, "billing_hard_limit_reached") ||
		strings.Contains(lower, "exceeded your current quota")
}

func isOAuthUnsupportedModelError(status int, errMsg string) bool {
	if status != http.StatusBadRequest && status != http.StatusNotFound {
		return false
	}
	lower := strings.ToLower(errMsg)
	return strings.Contains(lower, "not supported when using codex") ||
		strings.Contains(lower, "model_not_found") ||
		(strings.Contains(lower, "model") && strings.Contains(lower, "does not exist"))
}

func cooldownForError(status int, errMsg string) time.Duration {
	if isQuotaHardExhausted(errMsg) {
		return 6 * time.Hour
	}
	return cooldownForStatus(status)
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
