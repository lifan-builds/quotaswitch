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
	"sync"
	"time"
)

// skipThoughtSignature is Google's documented placeholder for function calls whose
// original thought signature is unknown (e.g. history from another model).
const skipThoughtSignature = "skip_thought_signature_validator"

var thoughtSignatures sync.Map // tool call ID -> Gemini thoughtSignature

func rememberThoughtSignature(part map[string]any, callID string) {
	if sig, ok := part["thoughtSignature"].(string); ok && sig != "" && callID != "" {
		thoughtSignatures.Store(callID, sig)
	}
}

func thoughtSignatureFor(callID string) string {
	if v, ok := thoughtSignatures.Load(callID); ok {
		return v.(string)
	}
	return skipThoughtSignature
}

const (
	DefaultAgyEndpoint = "https://daily-cloudcode-pa.googleapis.com"
	agyCLIUserAgent    = "antigravity/cli/1.4.5 (quotaswitch/0.3.0; os_type=darwin; arch=arm64; auth_method=consumer)"
)

func sanitizeVertexSchema(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			switch k {
			case "$schema", "additionalProperties", "$id", "definitions", "$defs", "default", "examples", "strict":
				continue
			default:
				out[k] = sanitizeVertexSchema(child)
			}
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = sanitizeVertexSchema(item)
		}
		return out
	default:
		return v
	}
}

func buildAgyGeneratePayload(req ChatCompletionRequest, projectID, upstreamModel string) map[string]any {
	var systemParts []map[string]any
	var contents []map[string]any
	toolNameByCallID := make(map[string]string)

	appendContentParts := func(role string, parts []map[string]any) {
		if len(parts) == 0 {
			return
		}
		if len(contents) > 0 {
			last := contents[len(contents)-1]
			if lastRole, _ := last["role"].(string); lastRole == role {
				if existing, ok := last["parts"].([]map[string]any); ok {
					last["parts"] = append(existing, parts...)
					return
				}
			}
		}
		contents = append(contents, map[string]any{
			"role":  role,
			"parts": parts,
		})
	}

	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "system", "developer":
			texts, _ := extractMessageParts(msg.Content, false)
			for _, t := range texts {
				if strings.TrimSpace(t) != "" {
					systemParts = append(systemParts, map[string]any{"text": t})
				}
			}
		case "user":
			texts, _ := extractMessageParts(msg.Content, false)
			var parts []map[string]any
			for _, t := range texts {
				if t != "" {
					parts = append(parts, map[string]any{"text": t})
				}
			}
			appendContentParts("user", parts)
		case "assistant":
			var parts []map[string]any
			if strings.TrimSpace(msg.ReasoningContent) != "" {
				parts = append(parts, map[string]any{
					"text":    msg.ReasoningContent,
					"thought": true,
				})
			}
			texts, _ := extractMessageParts(msg.Content, true)
			for _, t := range texts {
				if t != "" {
					parts = append(parts, map[string]any{"text": t})
				}
			}
			for _, tc := range msg.ToolCalls {
				fnName := strings.TrimSpace(tc.Function.Name)
				if fnName == "" {
					continue
				}
				callID := normalizeCallID(tc.ID)
				toolNameByCallID[callID] = fnName
				var argsObj map[string]any
				if strings.TrimSpace(tc.Function.Arguments) != "" {
					_ = json.Unmarshal([]byte(tc.Function.Arguments), &argsObj)
				}
				if argsObj == nil {
					argsObj = map[string]any{}
				}
				parts = append(parts, map[string]any{
					"functionCall": map[string]any{
						"id":   callID,
						"name": fnName,
						"args": argsObj,
					},
					"thoughtSignature": thoughtSignatureFor(callID),
				})
			}
			appendContentParts("model", parts)
		case "tool", "function":
			callID := normalizeCallID(msg.ToolCallID)
			fnName := strings.TrimSpace(msg.Name)
			if fnName == "" {
				fnName = toolNameByCallID[callID]
			}
			if fnName == "" {
				fnName = "tool"
			}
			texts, _ := extractMessageParts(msg.Content, false)
			outStr := strings.Join(texts, "\n")
			part := map[string]any{
				"functionResponse": map[string]any{
					"id":   callID,
					"name": fnName,
					"response": map[string]any{
						"output": outStr,
					},
				},
			}
			appendContentParts("user", []map[string]any{part})
		}
	}

	if len(contents) == 0 {
		contents = append(contents, map[string]any{
			"role": "user",
			"parts": []map[string]any{
				{"text": "Hello"},
			},
		})
	}

	innerReq := map[string]any{
		"contents": contents,
	}
	if len(systemParts) > 0 {
		innerReq["systemInstruction"] = map[string]any{
			"parts": systemParts,
		}
	}

	var fnDecls []map[string]any
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
		decl := map[string]any{
			"name":        name,
			"description": desc,
		}
		if params != nil {
			decl["parameters"] = sanitizeVertexSchema(params)
		}
		fnDecls = append(fnDecls, decl)
	}
	if len(fnDecls) > 0 {
		innerReq["tools"] = []map[string]any{
			{"functionDeclarations": fnDecls},
		}
	}

	genCfg := map[string]any{}
	if req.Temperature != nil {
		genCfg["temperature"] = *req.Temperature
	}
	maxOut := req.MaxCompletionTokens
	if maxOut <= 0 {
		maxOut = req.MaxTokens
	}
	if maxOut > 0 {
		genCfg["maxOutputTokens"] = maxOut
	}
	if len(genCfg) > 0 {
		innerReq["generationConfig"] = genCfg
	}

	return map[string]any{
		"project":   projectID,
		"requestId": fmt.Sprintf("qs-%d", time.Now().UnixNano()),
		"model":     upstreamModel,
		"userAgent": "antigravity",
		"request":   innerReq,
	}
}

func (s *Server) handleAgyChatCompletions(w http.ResponseWriter, r *http.Request, req ChatCompletionRequest, upstreamModel string) {
	candidates, err := s.Pool.AgyCandidates()
	if err != nil || len(candidates) == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_agy_accounts", fmt.Sprintf("no Antigravity accounts available: %v", err))
		return
	}

	var lastStatus int
	var lastErr error
	for i := range candidates {
		cand := candidates[i]
		accessToken, projectID, err := s.Pool.ResolveAgySession(r.Context(), &cand, false)
		if err != nil {
			lastErr = err
			s.Pool.MarkCooldown(BackendAgy, cand.ID, defaultCooldown)
			continue
		}

		payload := buildAgyGeneratePayload(req, projectID, upstreamModel)
		rawBody, err := json.Marshal(payload)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		resp, status, doErr := s.doAgyStreamRequest(r.Context(), accessToken, rawBody)
		if status == http.StatusUnauthorized {
			if tok2, proj2, rErr := s.Pool.ResolveAgySession(r.Context(), &cand, true); rErr == nil {
				payload["project"] = proj2
				if raw2, mErr := json.Marshal(payload); mErr == nil {
					resp, status, doErr = s.doAgyStreamRequest(r.Context(), tok2, raw2)
				}
			}
		}
		if doErr != nil {
			lastStatus = status
			lastErr = doErr
			if shouldFailoverStatus(status, doErr.Error()) {
				s.Pool.MarkCooldown(BackendAgy, cand.ID, cooldownForStatus(status))
				continue
			}
			writeOpenAIError(w, nonZeroStatus(status, http.StatusBadGateway), "upstream_error", doErr.Error())
			return
		}

		s.Pool.MarkHealthy(BackendAgy, cand.ID)
		defer resp.Body.Close()
		w.Header().Set("X-Quotaswitch-Provider", BackendAgy)
		w.Header().Set("X-Quotaswitch-Account", cand.DisplayName)

		if req.Stream {
			streamAgyToChatCompletions(w, resp.Body, req.Model)
		} else {
			collectAgyToChatCompletion(w, resp.Body, req.Model)
		}
		return
	}

	if lastErr == nil {
		lastErr = errors.New("all Antigravity accounts exhausted or unavailable")
	}
	writeOpenAIError(w, nonZeroStatus(lastStatus, http.StatusTooManyRequests), "quota_exhausted", fmt.Sprintf("Antigravity pool exhausted: %v", lastErr))
}

func (s *Server) doAgyStreamRequest(ctx context.Context, accessToken string, rawBody []byte) (*http.Response, int, error) {
	base := strings.TrimRight(strings.TrimSpace(s.AgyEndpoint), "/")
	if base == "" {
		base = DefaultAgyEndpoint
	}
	endpoint := base + "/v1internal:streamGenerateContent?alt=sse"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawBody))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", agyCLIUserAgent)

	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
		msg := strings.TrimSpace(string(bodyBytes))
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, resp.StatusCode, fmt.Errorf("Antigravity CloudCode HTTP %d: %s", resp.StatusCode, msg)
	}
	return resp, resp.StatusCode, nil
}

func streamAgyToChatCompletions(w http.ResponseWriter, body io.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	created := time.Now().Unix()
	chunkID := fmt.Sprintf("chatcmpl-qs-agy-%d", time.Now().UnixNano())
	sentRole := false
	hasToolCalls := false
	nextToolIdx := 0
	var lastUsage *ChatCompletionUsage

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
		_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
		if flusher != nil {
			flusher.Flush()
		}
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		dataStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if dataStr == "" || dataStr == "[DONE]" {
			continue
		}
		var outer map[string]any
		if err := json.Unmarshal([]byte(dataStr), &outer); err != nil {
			continue
		}
		if tid, ok := outer["traceId"].(string); ok && tid != "" {
			chunkID = "chatcmpl-" + tid
		}
		respObj, ok := outer["response"].(map[string]any)
		if !ok {
			respObj = outer
		}
		if u := parseAgyUsage(respObj["usageMetadata"]); u != nil {
			lastUsage = u
		}
		cands, ok := respObj["candidates"].([]any)
		if !ok || len(cands) == 0 {
			continue
		}
		cand0, ok := cands[0].(map[string]any)
		if !ok {
			continue
		}
		if contentObj, ok := cand0["content"].(map[string]any); ok {
			if parts, ok := contentObj["parts"].([]any); ok {
				for _, rawPart := range parts {
					part, ok := rawPart.(map[string]any)
					if !ok {
						continue
					}
					if fc, ok := part["functionCall"].(map[string]any); ok {
						hasToolCalls = true
						name, _ := fc["name"].(string)
						callID, _ := fc["id"].(string)
						if callID == "" {
							callID = fmt.Sprintf("call_%s_%d", chunkID, nextToolIdx+1)
						}
						rememberThoughtSignature(part, callID)
						argsStr := "{}"
						if argsObj := fc["args"]; argsObj != nil {
							if b, err := json.Marshal(argsObj); err == nil {
								argsStr = string(b)
							}
						}
						idx := nextToolIdx
						nextToolIdx++
						emitDelta(ChatChunkDelta{
							ToolCalls: []ChatToolCall{
								{
									Index: &idx,
									ID:    callID,
									Type:  "function",
									Function: ChatFunctionCall{
										Name:      name,
										Arguments: argsStr,
									},
								},
							},
						}, nil, nil)
						continue
					}
					txt, _ := part["text"].(string)
					if txt == "" {
						continue
					}
					if thought, _ := part["thought"].(bool); thought {
						emitDelta(ChatChunkDelta{ReasoningContent: txt}, nil, nil)
					} else {
						emitDelta(ChatChunkDelta{Content: txt}, nil, nil)
					}
				}
			}
		}
	}

	finish := "stop"
	if hasToolCalls {
		finish = "tool_calls"
	}
	emitDelta(ChatChunkDelta{}, &finish, lastUsage)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func collectAgyToChatCompletion(w http.ResponseWriter, body io.Reader, model string) {
	created := time.Now().Unix()
	respID := fmt.Sprintf("chatcmpl-qs-agy-%d", time.Now().UnixNano())
	var contentBuf strings.Builder
	var reasoningBuf strings.Builder
	var toolCalls []ChatToolCall
	var lastUsage *ChatCompletionUsage

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		dataStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if dataStr == "" || dataStr == "[DONE]" {
			continue
		}
		var outer map[string]any
		if err := json.Unmarshal([]byte(dataStr), &outer); err != nil {
			continue
		}
		if tid, ok := outer["traceId"].(string); ok && tid != "" {
			respID = "chatcmpl-" + tid
		}
		respObj, ok := outer["response"].(map[string]any)
		if !ok {
			respObj = outer
		}
		if u := parseAgyUsage(respObj["usageMetadata"]); u != nil {
			lastUsage = u
		}
		cands, ok := respObj["candidates"].([]any)
		if !ok || len(cands) == 0 {
			continue
		}
		cand0, ok := cands[0].(map[string]any)
		if !ok {
			continue
		}
		if contentObj, ok := cand0["content"].(map[string]any); ok {
			if parts, ok := contentObj["parts"].([]any); ok {
				for _, rawPart := range parts {
					part, ok := rawPart.(map[string]any)
					if !ok {
						continue
					}
					if fc, ok := part["functionCall"].(map[string]any); ok {
						name, _ := fc["name"].(string)
						callID, _ := fc["id"].(string)
						if callID == "" {
							callID = fmt.Sprintf("call_%s_%d", respID, len(toolCalls)+1)
						}
						rememberThoughtSignature(part, callID)
						argsStr := "{}"
						if argsObj := fc["args"]; argsObj != nil {
							if b, err := json.Marshal(argsObj); err == nil {
								argsStr = string(b)
							}
						}
						toolCalls = append(toolCalls, ChatToolCall{
							ID:   callID,
							Type: "function",
							Function: ChatFunctionCall{
								Name:      name,
								Arguments: argsStr,
							},
						})
						continue
					}
					txt, _ := part["text"].(string)
					if txt == "" {
						continue
					}
					if thought, _ := part["thought"].(bool); thought {
						reasoningBuf.WriteString(txt)
					} else {
						contentBuf.WriteString(txt)
					}
				}
			}
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
		Usage: lastUsage,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func parseAgyUsage(raw any) *ChatCompletionUsage {
	uMap, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	inTok := intFromAny(uMap["promptTokenCount"])
	outTok := intFromAny(uMap["candidatesTokenCount"])
	totTok := intFromAny(uMap["totalTokenCount"])
	if totTok == 0 {
		totTok = inTok + outTok
	}
	return &ChatCompletionUsage{
		PromptTokens:     inTok,
		CompletionTokens: outTok,
		TotalTokens:      totTok,
	}
}
