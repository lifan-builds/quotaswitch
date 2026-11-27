// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 lifan-builds and contributors, including MIT-licensed agy-auth code by ahmad fikril (THIRD_PARTY_NOTICES.md). QuotaSwitch; Section 7 attribution terms in ATTRIBUTION.md.

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/codex"
)

const (
	DefaultCodexResponsesURL  = "https://chatgpt.com/backend-api/codex/responses"
	DefaultOpenAIResponsesURL = "https://api.openai.com/v1/responses"
	codexCLIUserAgent         = "codex_cli_rs/0.167.0-alpha.3.2 (Mac OS 26.0; arm64)"
	defaultDirectInstructions = "Answer or execute the user's request directly. Never use defensive language, hedging, disclaimers, or throat-clearing; when coding, never add defensive boilerplate, speculative guards, fallback shims, or unrequested abstractions."
)

type ChatCompletionRequest struct {
	Model               string          `json:"model"`
	Messages            []ChatMessage   `json:"messages"`
	Tools               []ChatTool      `json:"tools,omitempty"`
	ToolChoice          any             `json:"tool_choice,omitempty"`
	ResponseFormat      any             `json:"response_format,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	PromptCacheKey      string          `json:"prompt_cache_key,omitempty"`
	StreamOptions       json.RawMessage `json:"stream_options,omitempty"`
}

type ChatMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	Name             string         `json:"name,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type ChatTool struct {
	Type     string           `json:"type"`
	Function ChatToolFunction `json:"function"`
	// Support already-flattened Responses-style tools if passed
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type ChatToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type ChatToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ChatFunctionCall `json:"function"`
}

type ChatFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type ChatCompletionChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []ChatChunkChoice    `json:"choices"`
	Usage   *ChatCompletionUsage `json:"usage,omitempty"`
}

type ChatChunkChoice struct {
	Index        int            `json:"index"`
	Delta        ChatChunkDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type ChatChunkDelta struct {
	Role             string         `json:"role,omitempty"`
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
}

type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   *ChatCompletionUsage   `json:"usage,omitempty"`
}

type ChatCompletionChoice struct {
	Index        int                 `json:"index"`
	Message      ChatResponseMessage `json:"message"`
	FinishReason string              `json:"finish_reason"`
}

type ChatResponseMessage struct {
	Role             string         `json:"role"`
	Content          *string        `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
}

type ChatCompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"-"`
}

type codexResponsesRequest struct {
	Model          string              `json:"model"`
	Instructions   string              `json:"instructions"`
	Input          []any               `json:"input"`
	Tools          []codexResponseTool `json:"tools,omitempty"`
	ToolChoice     any                 `json:"tool_choice,omitempty"`
	Text           map[string]any      `json:"text,omitempty"`
	Reasoning      *codexReasoning     `json:"reasoning,omitempty"`
	PromptCacheKey string              `json:"prompt_cache_key,omitempty"`
	ServiceTier    string              `json:"service_tier,omitempty"`
	Stream         bool                `json:"stream"`
	Store          bool                `json:"store"`
}

type codexReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type codexResponseTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

func extractMessageParts(content any, isAssistant bool) (textParts []string, contentItems []map[string]any) {
	if content == nil {
		return nil, nil
	}
	textType := "input_text"
	if isAssistant {
		textType = "output_text"
	}
	switch v := content.(type) {
	case string:
		if v != "" {
			textParts = append(textParts, v)
			contentItems = append(contentItems, map[string]any{
				"type": textType,
				"text": v,
			})
		}
	case []any:
		for _, rawPart := range v {
			partMap, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			pType, _ := partMap["type"].(string)
			switch pType {
			case "text", "input_text", "output_text":
				if txt, ok := partMap["text"].(string); ok && txt != "" {
					textParts = append(textParts, txt)
					contentItems = append(contentItems, map[string]any{
						"type": textType,
						"text": txt,
					})
				}
			case "image_url", "input_image":
				if isAssistant {
					continue
				}
				var imgURL string
				if u, ok := partMap["image_url"].(string); ok {
					imgURL = u
				} else if uMap, ok := partMap["image_url"].(map[string]any); ok {
					imgURL, _ = uMap["url"].(string)
				}
				if imgURL != "" {
					contentItems = append(contentItems, map[string]any{
						"type":      "input_image",
						"image_url": imgURL,
					})
				}
			}
		}
	}
	return textParts, contentItems
}

func normalizeCallID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "call_1"
	}
	return id
}

func buildCodexResponsesPayload(req ChatCompletionRequest, upstreamModel, defaultEffort string) codexResponsesRequest {
	var instructions []string
	var input []any

	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "system", "developer":
			texts, _ := extractMessageParts(msg.Content, false)
			if len(texts) > 0 {
				instructions = append(instructions, strings.Join(texts, "\n"))
			}
		case "user":
			_, items := extractMessageParts(msg.Content, false)
			if len(items) > 0 {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "user",
					"content": items,
				})
			}
		case "assistant":
			_, items := extractMessageParts(msg.Content, true)
			if len(items) > 0 {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": items,
				})
			}
			for _, tc := range msg.ToolCalls {
				name := strings.TrimSpace(tc.Function.Name)
				if name == "" {
					continue
				}
				args := tc.Function.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   normalizeCallID(tc.ID),
					"name":      name,
					"arguments": args,
				})
			}
		case "tool", "function":
			texts, _ := extractMessageParts(msg.Content, false)
			outStr := strings.Join(texts, "\n")
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": normalizeCallID(msg.ToolCallID),
				"output":  outStr,
			})
		}
	}

	instStr := strings.TrimSpace(strings.Join(instructions, "\n\n"))
	if instStr == "" {
		instStr = defaultDirectInstructions
	}
	if len(input) == 0 {
		input = append(input, map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": "Hello"},
			},
		})
	}

	var tools []codexResponseTool
	for _, t := range req.Tools {
		name := strings.TrimSpace(t.Function.Name)
		desc := t.Function.Description
		params := t.Function.Parameters
		if name == "" {
			name = strings.TrimSpace(t.Name)
			desc = t.Description
			params = t.Parameters
		}
		if name == "" {
			continue
		}
		if params == nil {
			params = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}
		tools = append(tools, codexResponseTool{
			Type:        "function",
			Name:        name,
			Description: desc,
			Parameters:  params,
		})
	}

	var toolChoice any
	if req.ToolChoice != nil {
		switch tc := req.ToolChoice.(type) {
		case string:
			toolChoice = tc
		case map[string]any:
			if fn, ok := tc["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" {
					toolChoice = map[string]any{
						"type": "function",
						"name": name,
					}
				}
			} else {
				toolChoice = tc
			}
		}
	}

	effort := strings.TrimSpace(req.ReasoningEffort)
	if effort == "" {
		effort = defaultEffort
	}
	if effort == "xhigh" {
		effort = "high"
	}
	var reasoning *codexReasoning
	if effort != "" && effort != "none" {
		reasoning = &codexReasoning{
			Effort:  effort,
			Summary: "auto",
		}
	}

	var textCfg map[string]any
	if rfMap, ok := req.ResponseFormat.(map[string]any); ok {
		rfType, _ := rfMap["type"].(string)
		switch rfType {
		case "json_object", "text":
			textCfg = map[string]any{"format": map[string]any{"type": rfType}}
		case "json_schema":
			if js, ok := rfMap["json_schema"].(map[string]any); ok {
				fmtObj := map[string]any{"type": "json_schema"}
				for _, k := range []string{"name", "description", "schema", "strict"} {
					if v, exists := js[k]; exists {
						fmtObj[k] = v
					}
				}
				textCfg = map[string]any{"format": fmtObj}
			}
		}
	}

	return codexResponsesRequest{
		Model:          upstreamModel,
		Instructions:   instStr,
		Input:          completeInterruptedToolCalls(input),
		Tools:          tools,
		ToolChoice:     toolChoice,
		Text:           textCfg,
		Reasoning:      reasoning,
		PromptCacheKey: req.PromptCacheKey,
		Stream:         true,
		Store:          false,
	}
}

// Antigravity may retain a call without its result after interruption or history
// compaction. Responses requires a result for every call. Mark the missing result
// explicitly rather than inventing success, dropping the call, or executing it.
func completeInterruptedToolCalls(input []any) []any {
	outputs := make(map[string]bool)
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if ok && item["type"] == "function_call_output" {
			id, _ := item["call_id"].(string)
			outputs[id] = true
		}
	}
	out := make([]any, 0, len(input))
	for _, raw := range input {
		out = append(out, raw)
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "function_call" {
			continue
		}
		id, _ := item["call_id"].(string)
		if outputs[id] {
			continue
		}
		out = append(out, map[string]any{
			"type":    "function_call_output",
			"call_id": id,
			"output":  "[Gateway history repair: no result was recorded for this tool call in the supplied Antigravity history. Its execution and outcome are unknown; this is not evidence of success. Do not assume it completed.]",
		})
	}
	return out
}

func isCodexQuotaLimitError(status int, errMsg string) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	lower := strings.ToLower(errMsg)
	for _, tok := range []string{"usage_limit", "rate_limit", "out of credits", "quota"} {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

func (s *Server) tryRedeemCodexReset(ctx context.Context, cand *CodexCandidate) bool {
	if s == nil || s.CodexUsage == nil || cand == nil || cand.AuthMode == codex.AuthModeAPIKey {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	var store *codex.Store
	activeRunning := false
	if s.Pool != nil {
		store = s.Pool.CodexStore
		if s.Pool.CodexRunning != nil {
			activeRunning = s.Pool.CodexRunning()
		}
	}
	rc, err := s.CodexUsage.FetchResetCredits(ctx, cand.Account)
	if err != nil || !rc.HasAvailable(time.Now()) {
		return false
	}
	creditID := ""
	if next := rc.NextExpiringCredit(time.Now()); next != nil {
		creditID = next.ID
	}
	res, err := s.CodexUsage.ConsumeResetCredit(ctx, store, cand.Account, creditID, activeRunning)
	if err != nil || !res.Redeemed() {
		return false
	}
	s.logf("reset_credit_redeemed account=%q credit_id=%q outcome=%q", cand.DisplayName, res.CreditID, res.Outcome)
	return true
}

func (s *Server) handleCodexChatCompletions(w http.ResponseWriter, r *http.Request, req ChatCompletionRequest, upstreamModel, defaultEffort string) {
	ctx := r.Context()
	acctFilter := r.Header.Get("X-Quotaswitch-Account")
	authPref := r.Header.Get("X-Quotaswitch-Auth")
	candidates, err := s.Pool.CodexCandidatesWithMode(acctFilter, authPref)
	if err != nil || len(candidates) == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_codex_accounts", fmt.Sprintf("no Codex accounts available: %v", err))
		return
	}

	oauthPayload := buildCodexResponsesPayload(req, upstreamModel, defaultEffort)
	oauthPayload.ServiceTier = "priority"
	rawOAuthBody, err := json.Marshal(oauthPayload)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	apiPayload := oauthPayload
	apiPayload.ServiceTier = ""
	rawAPIBody, err := json.Marshal(apiPayload)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
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
			s.logf("attempt model=%q candidate=%d status=token_unavailable", upstreamModel, i+1)
			lastErr = err
			s.Pool.MarkCooldown(BackendCodex, cand.ID, defaultCooldown)
			continue
		}

		rawBody := rawOAuthBody
		if cand.AuthMode == codex.AuthModeAPIKey {
			rawBody = rawAPIBody
		}
		resetAttempted := false
	retryAfterReset:
		resp, status, doErr := s.doCodexResponsesRequest(ctx, cand, rawBody, r.Header)
		s.logf("attempt model=%q candidate=%d status=%d payload_bytes=%d", upstreamModel, i+1, status, len(rawBody))
		if status == http.StatusUnauthorized && cand.AuthMode != codex.AuthModeAPIKey && !requestCanceled(ctx, doErr) {
			refreshErr := s.Pool.EnsureCodexToken(ctx, &cand, true)
			if requestCanceled(ctx, refreshErr) {
				return
			}
			if refreshErr == nil {
				resp, status, doErr = s.doCodexResponsesRequest(ctx, cand, rawBody, r.Header)
			}
		}
		if doErr != nil {
			if requestCanceled(ctx, doErr) {
				return
			}
			if !resetAttempted && cand.AuthMode != codex.AuthModeAPIKey && isCodexQuotaLimitError(status, doErr.Error()) {
				resetAttempted = true
				if s.tryRedeemCodexReset(ctx, &cand) {
					goto retryAfterReset
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
		if ctx.Err() != nil {
			_ = resp.Body.Close()
			return
		}

		setQuotaswitchHeaders(w, BackendCodex, cand.DisplayName, cand.Account.Name)

		var committed bool
		var usage *ChatCompletionUsage
		var streamErr error
		if req.Stream {
			committed, usage, streamErr = streamCodexToChatCompletions(w, resp.Body, req.Model)
		} else {
			committed, usage, streamErr = collectCodexToChatCompletion(w, resp.Body, req.Model)
		}
		_ = resp.Body.Close()
		if streamErr != nil {
			if requestCanceled(ctx, streamErr) || errors.Is(streamErr, errDownstreamWrite) {
				return
			}
			var retryable bool
			lastStatus, retryable = codexStreamFailureStatus(streamErr)
			s.logf("stream model=%q candidate=%d status=%d committed=%t error=%q", upstreamModel, i+1, lastStatus, committed, streamErr.Error())
			lastErr = streamErr
			if !committed && !resetAttempted && cand.AuthMode != codex.AuthModeAPIKey && isCodexQuotaLimitError(lastStatus, streamErr.Error()) {
				resetAttempted = true
				if s.tryRedeemCodexReset(ctx, &cand) {
					goto retryAfterReset
				}
			}
			if cand.AuthMode == codex.AuthModeAPIKey && isQuotaHardExhausted(streamErr.Error()) {
				s.Pool.MarkAPIKeyExhausted(cand.Account)
			}
			if retryable {
				s.Pool.MarkCooldown(BackendCodex, cand.ID, cooldownForError(lastStatus, streamErr.Error()))
			}
			if committed {
				// Replaying after text or a tool call was sent could duplicate work.
				if req.Stream {
					writeChatStreamError(w, streamErr)
				}
				return
			}
			if retryable {
				continue
			}
			writeOpenAIError(w, lastStatus, "upstream_error", streamErr.Error())
			return
		}
		s.Pool.MarkHealthy(BackendCodex, cand.ID)
		if cand.AuthMode == codex.AuthModeAPIKey {
			var inTok, cachedTok, outTok int
			if usage != nil {
				inTok = usage.PromptTokens
				cachedTok = usage.CachedTokens
				outTok = usage.CompletionTokens
			}
			s.Pool.RecordAPIKeyUsage(cand.Account, codex.CreditCallDelta{
				Model:        upstreamModel,
				CostUSD:      codex.EstimateTokenCostUSD(upstreamModel, inTok, cachedTok, outTok, false),
				Requests:     1,
				InputTokens:  int64(inTok),
				CachedTokens: int64(cachedTok),
				OutputTokens: int64(outTok),
			})
		}
		return
	}

	if lastErr == nil {
		lastErr = errors.New("all Codex accounts exhausted or unavailable")
	}
	writeOpenAIError(w, nonZeroStatus(lastStatus, http.StatusTooManyRequests), "quota_exhausted", fmt.Sprintf("Codex pool exhausted: %v", lastErr))
}

func (s *Server) doCodexResponsesRequest(ctx context.Context, cand CodexCandidate, rawBody []byte, incoming http.Header) (*http.Response, int, error) {
	endpoint := strings.TrimSpace(s.CodexResponsesURL)
	if endpoint == "" {
		if cand.AuthMode == codex.AuthModeAPIKey {
			endpoint = DefaultOpenAIResponsesURL
		} else {
			endpoint = DefaultCodexResponsesURL
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawBody))
	if err != nil {
		return nil, 0, err
	}
	token := cand.AccessToken
	if cand.AuthMode == codex.AuthModeAPIKey {
		token = cand.APIKey
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if cand.AuthMode == codex.AuthModeAPIKey {
		req.Header.Set("Accept", "application/json, text/event-stream")
	} else {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("originator", "codex_cli_rs")
		for _, name := range []string{"session_id", "conversation_id"} {
			if value := strings.TrimSpace(incoming.Get(name)); value != "" {
				req.Header.Set(name, value)
			}
		}
	}
	req.Header.Set("User-Agent", codexCLIUserAgent)
	if cand.AccountID != "" && cand.AuthMode != codex.AuthModeAPIKey {
		req.Header.Set("ChatGPT-Account-Id", cand.AccountID)
	}

	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
		if errors.Is(readErr, context.Canceled) {
			return nil, resp.StatusCode, readErr
		}
		msg := strings.TrimSpace(string(bodyBytes))
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, resp.StatusCode, fmt.Errorf("Codex upstream HTTP %d: %s", resp.StatusCode, msg)
	}
	return resp, resp.StatusCode, nil
}

type codexStreamFailure struct {
	status    int
	retryable bool
	message   string
}

func (e *codexStreamFailure) Error() string { return e.message }

func codexStreamFailureStatus(err error) (int, bool) {
	var failure *codexStreamFailure
	if errors.As(err, &failure) {
		return failure.status, failure.retryable
	}
	return http.StatusBadGateway, true
}

// nextCodexEvent requires an explicit response.completed event. A [DONE] marker
// or EOF by itself must never turn a failed or truncated response into success.
func nextCodexEvent(scanner *bufio.Scanner) (map[string]any, error) {
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil, fmt.Errorf("invalid Codex stream event JSON: %w", err)
		}
		evType, _ := ev["type"].(string)
		if evType == "error" || evType == "response.failed" || evType == "response.incomplete" || ev["error"] != nil {
			return nil, codexEventFailure(ev, evType)
		}
		if evType == "" {
			return nil, errors.New("invalid Codex stream event: missing type")
		}
		return ev, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading Codex stream: %w", err)
	}
	return nil, errors.New("Codex stream ended before response.completed")
}

func codexEventFailure(ev map[string]any, evType string) error {
	details := ev
	if response, ok := ev["response"].(map[string]any); ok {
		details = response
	}
	if nested, ok := details["error"].(map[string]any); ok {
		details = nested
	} else if incomplete, ok := details["incomplete_details"].(map[string]any); ok {
		details = incomplete
	}
	code, _ := details["code"].(string)
	message, _ := details["message"].(string)
	if code == "" {
		code, _ = details["reason"].(string)
	}
	if message == "" {
		message = "Codex upstream " + evType
	}
	if code != "" {
		message = code + ": " + message
	}
	status := http.StatusBadGateway
	lower := strings.ToLower(code)
	lowerMessage := strings.ToLower(message)
	historyError := strings.HasPrefix(lowerMessage, "no tool output found for function call") ||
		strings.HasPrefix(lowerMessage, "no tool call found for function call output")
	switch {
	case historyError:
		status = http.StatusBadRequest
	case strings.Contains(lower, "quota"), strings.Contains(lower, "rate_limit"), strings.Contains(lower, "usage_limit"), strings.Contains(lower, "usage_unavailable"):
		status = http.StatusTooManyRequests
	case strings.Contains(lower, "unauthorized"), strings.Contains(lower, "authentication"), strings.Contains(lower, "invalid_api_key"):
		status = http.StatusUnauthorized
	case strings.Contains(lower, "forbidden"), strings.Contains(lower, "permission"):
		status = http.StatusForbidden
	case strings.Contains(lower, "invalid_request"), strings.Contains(lower, "model_not_found"), strings.Contains(lower, "unsupported"):
		status = http.StatusBadRequest
	}
	return &codexStreamFailure{
		status:    status,
		retryable: !historyError && evType != "response.incomplete" && shouldFailoverStatus(status, message),
		message:   message,
	}
}

func writeChatStreamError(w http.ResponseWriter, err error) {
	raw, _ := json.Marshal(map[string]any{"error": map[string]string{
		"type": "upstream_error", "code": "upstream_error", "message": err.Error(),
	}})
	_ = writeSSEData(w, raw)
}

func streamCodexToChatCompletions(w http.ResponseWriter, body io.Reader, model string) (bool, *ChatCompletionUsage, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	created := time.Now().Unix()
	chunkID := fmt.Sprintf("chatcmpl-qs-%d", time.Now().UnixNano())
	sentRole := false
	sentText := false
	hasToolCalls := false
	nextToolIdx := 0
	toolIdxByItemID := make(map[string]int)
	argsEmittedByItemID := make(map[string]bool)
	committed := false
	var writeErr error

	emitDelta := func(delta ChatChunkDelta, finishReason *string, usage *ChatCompletionUsage) {
		if !sentRole && finishReason == nil && delta.Role == "" {
			delta.Role = "assistant"
			sentRole = true
		}
		chunk := ChatCompletionChunk{
			ID:      chunkID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []ChatChunkChoice{
				{
					Index:        0,
					Delta:        delta,
					FinishReason: finishReason,
				},
			},
			Usage: usage,
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return
		}
		committed = true
		writeErr = writeSSEData(w, raw)
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for {
		if writeErr != nil {
			return committed, nil, writeErr
		}
		ev, err := nextCodexEvent(scanner)
		if err != nil {
			return committed, nil, err
		}
		evType, _ := ev["type"].(string)
		switch evType {
		case "response.created":
			if respObj, ok := ev["response"].(map[string]any); ok {
				if id, ok := respObj["id"].(string); ok && id != "" {
					chunkID = id
				}
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if d, ok := ev["delta"].(string); ok && d != "" {
				emitDelta(ChatChunkDelta{ReasoningContent: d}, nil, nil)
			}
		case "response.output_text.delta":
			if d, ok := ev["delta"].(string); ok && d != "" {
				sentText = true
				emitDelta(ChatChunkDelta{Content: d}, nil, nil)
			}
		case "response.output_item.added":
			item, ok := ev["item"].(map[string]any)
			if !ok {
				continue
			}
			if itemType, _ := item["type"].(string); itemType == "function_call" {
				hasToolCalls = true
				itemID, _ := item["id"].(string)
				callID, _ := item["call_id"].(string)
				if callID == "" {
					callID = itemID
				}
				name, _ := item["name"].(string)
				idx := nextToolIdx
				nextToolIdx++
				if itemID != "" {
					toolIdxByItemID[itemID] = idx
				}
				emitDelta(ChatChunkDelta{
					ToolCalls: []ChatToolCall{
						{
							Index: &idx,
							ID:    normalizeCallID(callID),
							Type:  "function",
							Function: ChatFunctionCall{
								Name:      name,
								Arguments: "",
							},
						},
					},
				}, nil, nil)
			}
		case "response.function_call_arguments.delta":
			d, _ := ev["delta"].(string)
			if d == "" {
				continue
			}
			itemID, _ := ev["item_id"].(string)
			idx := 0
			if mapped, ok := toolIdxByItemID[itemID]; ok {
				idx = mapped
			}
			if itemID != "" {
				argsEmittedByItemID[itemID] = true
			}
			emitDelta(ChatChunkDelta{
				ToolCalls: []ChatToolCall{
					{
						Index: &idx,
						Function: ChatFunctionCall{
							Arguments: d,
						},
					},
				},
			}, nil, nil)
		case "response.output_item.done":
			item, ok := ev["item"].(map[string]any)
			if !ok {
				continue
			}
			itemType, _ := item["type"].(string)
			if itemType == "function_call" {
				itemID, _ := item["id"].(string)
				args, _ := item["arguments"].(string)
				if args != "" && !argsEmittedByItemID[itemID] {
					idx := 0
					if mapped, ok := toolIdxByItemID[itemID]; ok {
						idx = mapped
					}
					argsEmittedByItemID[itemID] = true
					emitDelta(ChatChunkDelta{
						ToolCalls: []ChatToolCall{
							{
								Index: &idx,
								Function: ChatFunctionCall{
									Arguments: args,
								},
							},
						},
					}, nil, nil)
				}
			} else if itemType == "message" && !sentText {
				if contentArr, ok := item["content"].([]any); ok {
					for _, rawPart := range contentArr {
						if pMap, ok := rawPart.(map[string]any); ok {
							if txt, ok := pMap["text"].(string); ok && txt != "" {
								sentText = true
								emitDelta(ChatChunkDelta{Content: txt}, nil, nil)
								if writeErr != nil {
									break
								}
							}
						}
					}
				}
			}
		case "response.completed":
			finish := "stop"
			if hasToolCalls {
				finish = "tool_calls"
			}
			var usage *ChatCompletionUsage
			if respObj, ok := ev["response"].(map[string]any); ok {
				usage = parseResponsesUsage(respObj["usage"])
			}
			emitDelta(ChatChunkDelta{}, &finish, usage)
			if writeErr != nil {
				return committed, usage, writeErr
			}
			return true, usage, writeSSEData(w, []byte("[DONE]"))
		}
	}
}

func collectCodexToChatCompletion(w http.ResponseWriter, body io.Reader, model string) (bool, *ChatCompletionUsage, error) {
	created := time.Now().Unix()
	respID := fmt.Sprintf("chatcmpl-qs-%d", time.Now().UnixNano())
	var contentBuf strings.Builder
	var reasoningBuf strings.Builder
	var toolCalls []ChatToolCall
	toolIdxByItemID := make(map[string]int)
	var usage *ChatCompletionUsage

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
events:
	for {
		ev, err := nextCodexEvent(scanner)
		if err != nil {
			return false, nil, err
		}
		evType, _ := ev["type"].(string)
		switch evType {
		case "response.created":
			if respObj, ok := ev["response"].(map[string]any); ok {
				if id, ok := respObj["id"].(string); ok && id != "" {
					respID = id
				}
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if d, ok := ev["delta"].(string); ok {
				reasoningBuf.WriteString(d)
			}
		case "response.output_text.delta":
			if d, ok := ev["delta"].(string); ok {
				contentBuf.WriteString(d)
			}
		case "response.output_item.added":
			if item, ok := ev["item"].(map[string]any); ok {
				if itemType, _ := item["type"].(string); itemType == "function_call" {
					itemID, _ := item["id"].(string)
					callID, _ := item["call_id"].(string)
					if callID == "" {
						callID = itemID
					}
					name, _ := item["name"].(string)
					idx := len(toolCalls)
					if itemID != "" {
						toolIdxByItemID[itemID] = idx
					}
					toolCalls = append(toolCalls, ChatToolCall{
						ID:   normalizeCallID(callID),
						Type: "function",
						Function: ChatFunctionCall{
							Name:      name,
							Arguments: "",
						},
					})
				}
			}
		case "response.function_call_arguments.delta":
			d, _ := ev["delta"].(string)
			itemID, _ := ev["item_id"].(string)
			if idx, ok := toolIdxByItemID[itemID]; ok && idx < len(toolCalls) {
				toolCalls[idx].Function.Arguments += d
			}
		case "response.output_item.done":
			if item, ok := ev["item"].(map[string]any); ok {
				if itemType, _ := item["type"].(string); itemType == "function_call" {
					itemID, _ := item["id"].(string)
					args, _ := item["arguments"].(string)
					if idx, ok := toolIdxByItemID[itemID]; ok && idx < len(toolCalls) && toolCalls[idx].Function.Arguments == "" {
						toolCalls[idx].Function.Arguments = args
					}
				}
			}
		case "response.completed":
			if respObj, ok := ev["response"].(map[string]any); ok {
				usage = parseResponsesUsage(respObj["usage"])
			}
			break events
		}
	}

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}
	var contentPtr *string
	if contentBuf.Len() > 0 || len(toolCalls) == 0 {
		txt := contentBuf.String()
		contentPtr = &txt
	}
	out := ChatCompletionResponse{
		ID:      respID,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []ChatCompletionChoice{
			{
				Index: 0,
				Message: ChatResponseMessage{
					Role:             "assistant",
					Content:          contentPtr,
					ReasoningContent: reasoningBuf.String(),
					ToolCalls:        toolCalls,
				},
				FinishReason: finishReason,
			},
		},
		Usage: usage,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		return true, usage, fmt.Errorf("%w: %w", errDownstreamWrite, err)
	}
	return true, usage, nil
}

func parseResponsesUsage(raw any) *ChatCompletionUsage {
	uMap, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	inTok := intFromAny(uMap["input_tokens"])
	if inTok == 0 {
		inTok = intFromAny(uMap["prompt_tokens"])
	}
	outTok := intFromAny(uMap["output_tokens"])
	if outTok == 0 {
		outTok = intFromAny(uMap["completion_tokens"])
	}
	totTok := intFromAny(uMap["total_tokens"])
	if totTok == 0 {
		totTok = inTok + outTok
	}
	var cachedTok int
	if details, ok := uMap["input_tokens_details"].(map[string]any); ok {
		cachedTok = intFromAny(details["cached_tokens"])
	} else if details, ok := uMap["prompt_tokens_details"].(map[string]any); ok {
		cachedTok = intFromAny(details["cached_tokens"])
	}
	return &ChatCompletionUsage{
		PromptTokens:     inTok,
		CompletionTokens: outTok,
		TotalTokens:      totTok,
		CachedTokens:     cachedTok,
	}
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}
