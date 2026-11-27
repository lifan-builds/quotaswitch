// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lifan-builds/quotaswitch/internal/codex"
)

type Server struct {
	Pool       *Pool
	CodexUsage *codex.UsageClient
	CodexHome  string
	APIKey     string
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
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 10 * time.Minute
	return &Server{
		Pool:        pool,
		CodexHome:   codexHome,
		APIKey:      apiKey,
		AgyEndpoint: DefaultAgyEndpoint,
		HTTPClient:  &http.Client{Transport: transport},
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.Method == http.MethodGet && (path == "/healthz" || path == "/health" || path == "/") {
		s.handleHealth(w, r)
		return
	}
	if key := strings.TrimSpace(s.APIKey); key != "" {
		scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") {
			token = ""
		}
		want := sha256.Sum256([]byte(key))
		bearer := sha256.Sum256([]byte(strings.TrimSpace(token)))
		header := sha256.Sum256([]byte(strings.TrimSpace(r.Header.Get("X-API-Key"))))
		if subtle.ConstantTimeCompare(want[:], bearer[:])|subtle.ConstantTimeCompare(want[:], header[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid or missing gateway API key")
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && (path == "/v1/credits" || path == "/credits"):
		s.handleCredits(w, r)
	case r.Method == http.MethodGet && (path == "/v1/models" || path == "/models"):
		s.handleModels(w, r)
	case r.Method == http.MethodPost && (path == "/v1/chat/completions" || path == "/chat/completions"):
		s.handleChatCompletions(w, r)
	case r.Method == http.MethodPost && (path == "/v1/responses" || path == "/responses"):
		s.handleCodexResponsesPassthrough(w, r)
	case isOpenAIPlatformRoute(r.Method, path):
		s.handleOpenAIPlatformPassthrough(w, r)
	default:
		writeOpenAIError(w, http.StatusNotFound, "not_found", fmt.Sprintf("unknown gateway route %s %s", r.Method, path))
	}
}

func isOpenAIPlatformRoute(method, path string) bool {
	norm := path
	if !strings.HasPrefix(norm, "/v1/") {
		norm = "/v1" + norm
	}
	if tail, ok := strings.CutPrefix(norm, "/v1/responses/"); ok {
		id, action, nested := strings.Cut(tail, "/")
		return id != "" && ((!nested && (method == http.MethodGet || method == http.MethodDelete)) ||
			(nested && action == "cancel" && method == http.MethodPost))
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

const maxJSONRequestBytes = 32 << 20
const maxResponseCaptureBytes = 1 << 20

func readRequestBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if requestCanceled(r.Context(), err) {
		return nil, false
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
		return nil, false
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
		return nil, false
	}
	return body, true
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	bodyBytes, ok := readRequestBody(w, r, maxJSONRequestBytes)
	if !ok {
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
	ctx := r.Context()
	bodyBytes, ok := readRequestBody(w, r, maxJSONRequestBytes)
	if !ok {
		return
	}
	var reqModel string
	var rawMap map[string]any
	oauthBodyBytes := bodyBytes
	if err := json.Unmarshal(bodyBytes, &rawMap); err == nil {
		if rawMap == nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "expected a JSON object")
			return
		}
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
		oauthBodyBytes = bodyBytes
		if _, hasTier := rawMap["service_tier"]; !hasTier {
			rawMap["service_tier"] = "priority"
			if updatedOAuth, mErr := json.Marshal(rawMap); mErr == nil {
				oauthBodyBytes = updatedOAuth
			}
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
		if ctx.Err() != nil {
			return
		}
		cand := candidates[i]
		if err := s.Pool.EnsureCodexToken(ctx, &cand, false); err != nil {
			if requestCanceled(ctx, err) {
				return
			}
			lastErr = err
			s.Pool.MarkCooldown(BackendCodex, cand.ID, defaultCooldown)
			continue
		}
		reqBody := oauthBodyBytes
		if cand.AuthMode == codex.AuthModeAPIKey {
			reqBody = bodyBytes
		}
		resetAttempted := false
	retryPassthroughAfterReset:
		resp, status, doErr := s.doCodexResponsesRequest(ctx, cand, reqBody, r.Header)
		if doErr != nil {
			if requestCanceled(ctx, doErr) {
				return
			}
			if !resetAttempted && cand.AuthMode != codex.AuthModeAPIKey && isCodexQuotaLimitError(status, doErr.Error()) {
				resetAttempted = true
				if s.tryRedeemCodexReset(ctx, &cand) {
					goto retryPassthroughAfterReset
				}
			}
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
		defer resp.Body.Close()
		if ctx.Err() != nil {
			return
		}
		for k, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		setQuotaswitchHeaders(w, BackendCodex, cand.DisplayName, cand.Account.Name)
		w.WriteHeader(resp.StatusCode)

		var usage *ChatCompletionUsage
		var complete bool
		if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			usage, complete = s.forwardResponsesStream(ctx, w, resp.Body, cand)
		} else {
			capture, err := io.ReadAll(io.LimitReader(io.TeeReader(resp.Body, w), maxResponseCaptureBytes+1))
			if err == nil && len(capture) > maxResponseCaptureBytes {
				_, _ = io.Copy(w, resp.Body)
			} else if err == nil {
				var response struct {
					Status string `json:"status"`
					Error  any    `json:"error"`
					Usage  any    `json:"usage"`
				}
				complete = json.Unmarshal(capture, &response) == nil && response.Error == nil && response.Status != "incomplete" && response.Status != "failed"
				usage = parseResponsesUsage(response.Usage)
			}
		}
		if complete {
			s.Pool.MarkHealthy(BackendCodex, cand.ID)
		}
		if cand.AuthMode == codex.AuthModeAPIKey {
			var inTok, cachedTok, outTok int
			if usage != nil {
				inTok = usage.PromptTokens
				cachedTok = usage.CachedTokens
				outTok = usage.CompletionTokens
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

func (s *Server) forwardResponsesStream(ctx context.Context, w http.ResponseWriter, body io.Reader, cand CodexCandidate) (*ChatCompletionUsage, bool) {
	reader := bufio.NewReader(body)
	var usage *ChatCompletionUsage
	var completed, failed bool
	var name string
	for {
		line, readErr := reader.ReadBytes('\n')
		if requestCanceled(ctx, readErr) {
			return usage, false
		}
		var failure string
		content := bytes.TrimSpace(line)
		field, value, _ := bytes.Cut(content, []byte(":"))
		switch string(field) {
		case "":
			if len(content) == 0 {
				name = ""
			}
		case "event":
			name = string(bytes.TrimSpace(value))
		case "data":
			data := bytes.TrimSpace(value)
			if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
				break
			}
			var event map[string]any
			if err := json.Unmarshal(data, &event); err != nil {
				failed = true
				break
			}
			eventType, _ := event["type"].(string)
			if eventType == "" {
				eventType = name
			}
			switch {
			case eventType == "error", eventType == "response.failed", eventType == "response.incomplete", event["error"] != nil:
				failed = true
				failure = codexEventFailure(event, eventType).Error()
			case eventType == "response.completed":
				completed = true
				response, _ := event["response"].(map[string]any)
				usage = parseResponsesUsage(response["usage"])
			}
		}
		if len(line) > 0 {
			if _, err := w.Write(line); err != nil {
				return usage, false
			}
			if err := flushResponse(w); err != nil {
				return usage, false
			}
		}
		if ctx.Err() != nil {
			return usage, false
		}
		if failure != "" {
			if cand.AuthMode == codex.AuthModeAPIKey && isQuotaHardExhausted(failure) {
				s.Pool.MarkAPIKeyExhausted(cand.Account)
			} else if strings.Contains(strings.ToLower(failure), "rate_limit_exceeded") {
				s.Pool.MarkCooldown(BackendCodex, cand.ID, cooldownForStatus(http.StatusTooManyRequests))
			}
		}
		if readErr != nil {
			return usage, readErr == io.EOF && completed && !failed
		}
	}
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

func requestCanceled(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled)
}

func flushResponse(w http.ResponseWriter) error {
	err := http.NewResponseController(w).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

var errDownstreamWrite = errors.New("writing response to client")

func writeSSEData(w http.ResponseWriter, data []byte) error {
	_, err := fmt.Fprintf(w, "data: %s\n\n", data)
	if err == nil {
		err = flushResponse(w)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errDownstreamWrite, err)
	}
	return nil
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
		var ok bool
		bodyBytes, ok = readRequestBody(w, r, maxPlatformBodyBytes)
		if !ok {
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
	collection := strings.TrimSuffix(upPath, "/")
	creatingJob := r.Method == http.MethodPost && (collection == "/v1/batches" || collection == "/v1/videos")
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
		if r.Context().Err() != nil {
			return
		}
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
			if lk == "authorization" || lk == "x-api-key" || lk == "host" || lk == "content-length" || strings.HasPrefix(lk, "x-quotaswitch-") {
				continue
			}
			for _, v := range vals {
				upReq.Header.Add(k, v)
			}
		}
		upReq.Header.Set("Authorization", "Bearer "+cand.APIKey)

		resp, doErr := client.Do(upReq)
		if doErr != nil {
			if requestCanceled(r.Context(), doErr) {
				return
			}
			if creatingJob || acctFilter != "" {
				setQuotaswitchHeaders(w, "openai-api-key", cand.DisplayName, cand.Account.Name)
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", doErr.Error())
				return
			}
			lastErr = doErr
			s.Pool.MarkCooldown(BackendCodex, cand.ID, defaultCooldown)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()
			if requestCanceled(r.Context(), readErr) {
				return
			}
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
			retry := shouldFailoverStatus(resp.StatusCode, errMsg)
			if creatingJob {
				retry = resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
					resp.StatusCode == http.StatusTooManyRequests ||
					(resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && isQuotaHardExhausted(errMsg))
			}
			if acctFilter == "" && retry {
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

		if r.Method == http.MethodGet && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			copyPlatformEventStream(w, resp.Body)
			return
		}
		var embedBuf bytes.Buffer
		var bodyReader io.Reader = resp.Body
		if r.Method == http.MethodPost && strings.HasPrefix(upPath, "/v1/embeddings") {
			bodyReader = io.TeeReader(resp.Body, &embedBuf)
		}
		_, _ = io.Copy(w, bodyReader)
		if r.Method == http.MethodPost {
			if delta, ok := buildPlatformCreditDelta(upPath, r.Header.Get("Content-Type"), bodyBytes, embedBuf.Bytes()); ok {
				s.Pool.RecordAPIKeyUsage(cand.Account, delta)
			}
		}
		return
	}

	writeOpenAIError(w, nonZeroStatus(lastStatus, http.StatusTooManyRequests), "quota_exhausted", fmt.Sprintf("OpenAI API key pool exhausted: %v", lastErr))
}

func copyPlatformEventStream(w http.ResponseWriter, body io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			if err := flushResponse(w); err != nil {
				return
			}
		}
		if readErr != nil {
			return
		}
	}
}

func buildPlatformCreditDelta(upPath, contentType string, reqBody, respBody []byte) (codex.CreditCallDelta, bool) {
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
		mediaType, params, _ := mime.ParseMediaType(contentType)
		if mediaType == "multipart/form-data" {
			fields, err := multipartImageFields(reqBody, params["boundary"])
			if err != nil {
				return codex.CreditCallDelta{}, false
			}
			reqObj.Model, reqObj.Quality, reqObj.Size = fields["model"], fields["quality"], fields["size"]
			if count, ok := fields["n"]; ok {
				reqObj.N, err = strconv.Atoi(count)
				if err != nil {
					return codex.CreditCallDelta{}, false
				}
			}
		} else if err := json.Unmarshal(reqBody, &reqObj); err != nil {
			return codex.CreditCallDelta{}, false
		}
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

func multipartImageFields(body []byte, boundary string) (map[string]string, error) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	fields := make(map[string]string)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return fields, nil
		}
		if err != nil {
			return nil, err
		}
		name := part.FormName()
		switch name {
		case "model", "quality", "size", "n":
			value, err := io.ReadAll(part)
			if err != nil {
				return nil, err
			}
			fields[name] = string(value)
		}
	}
}

func isQuotaHardExhausted(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	return strings.Contains(lower, "insufficient_quota") ||
		strings.Contains(lower, "credit_balance_exhausted") ||
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
