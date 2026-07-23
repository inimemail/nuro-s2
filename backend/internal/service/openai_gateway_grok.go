package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const grokQuotaSnapshotExtraKey = GrokQuotaSnapshotExtraKey

func (s *OpenAIGatewayService) forwardGrokResponses(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	reqStream bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	clearGrokClientToolMapping(c)
	if account.Type != AccountTypeOAuth && account.Type != AccountTypeAPIKey {
		return nil, fmt.Errorf("grok account type %s is not supported by Responses forwarding", account.Type)
	}

	upstreamModel := resolveGrokUpstreamModel(account, originalModel)
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = grokDefaultResponsesModel
	}
	if isGrokImageGenerationModel(upstreamModel) {
		return nil, fmt.Errorf("model %s is an image model and is not available on the Responses endpoint; use /v1/images/generations instead", upstreamModel)
	}
	adaptedBody, _, err := adaptGrokClientTools(c, body)
	if err != nil {
		return nil, err
	}
	patchedBody, err := patchGrokResponsesBody(adaptedBody, upstreamModel)
	if err != nil {
		return nil, err
	}
	if isOpenAIResponsesCompactPath(c) {
		patchedBody, err = buildGrokCompactRequestBody(patchedBody)
		if err != nil {
			return nil, err
		}
	}
	patchedBody, err = convertOpenAICompactInputsForGrok(patchedBody)
	if err != nil {
		return nil, err
	}
	// Derive the tenant/session identity from the original client payload. The
	// adapted body intentionally rewrites client tools for xAI compatibility;
	// using it as the cache seed would make cache affinity depend on that
	// transport rewrite instead of the client's stable session/prefix.
	cacheIdentity := resolveGrokCacheIdentity(c, body, "", upstreamModel)
	patchedBody, err = applyGrokResponsesCacheIdentity(patchedBody, body, cacheIdentity, account.IsGrokOAuth())
	if err != nil {
		return nil, fmt.Errorf("apply grok prompt cache identity: %w", err)
	}
	// Free OAuth function tools must use the same mixed-tools cache route as
	// the Messages bridge. This is deliberately gated by account tier and the
	// already-derived tenant-isolated identity, so paid/API-key/unknown
	// accounts remain byte-for-byte on the existing path.
	patchedBody, err = applyGrokFreeRequestToolCacheRoute(c, patchedBody, body, account, cacheIdentity)
	if err != nil {
		return nil, fmt.Errorf("apply grok Free function-tool cache route: %w", err)
	}

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	upstreamStart := time.Now()
	var resp *http.Response
	for attempt := 0; ; attempt++ {
		upstreamReq, buildErr := buildGrokResponsesRequest(upstreamCtx, c, account, patchedBody, token, cacheIdentity, s.cfg)
		if buildErr != nil {
			return nil, buildErr
		}
		resp, err = s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
		SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
		if err != nil {
			return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
		}
		if attempt > 0 || resp.StatusCode != http.StatusBadRequest {
			break
		}

		respBody, readErr := readUpstreamResponseBodyLimited(resp.Body, resolveUpstreamResponseReadLimit(s.cfg))
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		if readErr != nil {
			return nil, fmt.Errorf("read grok upstream error response: %w", readErr)
		}
		if !isGrokInvalidEncryptedContentResponse(resp.StatusCode, respBody) {
			resp.Body = io.NopCloser(bytes.NewReader(respBody))
			break
		}
		retryBody, changed, trimErr := trimGrokInvalidEncryptedContentRetryBody(patchedBody)
		if trimErr != nil {
			return nil, fmt.Errorf("prepare Grok invalid encrypted_content retry: %w", trimErr)
		}
		if !changed {
			resp.Body = io.NopCloser(bytes.NewReader(respBody))
			break
		}
		patchedBody = retryBody
		slog.Info("grok_invalid_encrypted_content_retry", "account_id", account.ID, "cache_identity_present", cacheIdentity != "")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, readErr := readUpstreamResponseBodyLimited(resp.Body, resolveUpstreamResponseReadLimit(s.cfg))
		if readErr != nil {
			return nil, fmt.Errorf("read grok upstream error response: %w", readErr)
		}
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		upstreamMsg := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(respBody))
		if upstreamMsg == "" {
			upstreamMsg = safeUpstreamErrorMessage
		}
		s.handleGrokAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
		if s.shouldFailoverGrokUpstreamError(resp.StatusCode, respBody) {
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id")),
				Kind:               "failover",
				Message:            upstreamMsg,
			})
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		return s.handleErrorResponseWithoutAccountState(ctx, resp, c, account, patchedBody, upstreamModel)
	}

	s.updateGrokUsageFromResponse(ctx, account, resp.Header, resp.StatusCode)

	var usage *OpenAIUsage
	var firstTokenMs *int
	responseID := ""
	terminalEventType := ""
	clientDisconnected := false
	var forwardErr error
	if reqStream {
		streamResult, streamErr := s.handleStreamingResponse(ctx, resp, c, account, startTime, originalModel, upstreamModel)
		if streamResult == nil {
			if streamErr != nil {
				return nil, streamErr
			}
			return nil, errors.New("grok streaming result is nil")
		}
		usage = streamResult.usage
		firstTokenMs = streamResult.firstTokenMs
		responseID = strings.TrimSpace(streamResult.responseID)
		terminalEventType = streamResult.terminalEventType
		clientDisconnected = streamResult.clientDisconnected
		forwardErr = streamErr
	} else {
		nonStreamResult, err := s.handleNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel)
		if err != nil {
			return nil, err
		}
		usage = nonStreamResult.usage
		responseID = strings.TrimSpace(nonStreamResult.responseID)
		terminalEventType = nonStreamResult.terminalEventType
	}

	if usage == nil {
		usage = &OpenAIUsage{}
	}
	reasoningEffort := extractOpenAIReasoningEffortFromBody(patchedBody, originalModel)
	result := &OpenAIForwardResult{
		RequestID:         firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id")),
		ResponseID:        responseID,
		Usage:             *usage,
		Model:             originalModel,
		UpstreamModel:     upstreamModel,
		ReasoningEffort:   reasoningEffort,
		Stream:            reqStream,
		OpenAIWSMode:      false,
		TerminalEventType: terminalEventType,
		ResponseHeaders:   resp.Header.Clone(),
		Duration:          time.Since(startTime),
		FirstTokenMs:      firstTokenMs,
		ClientDisconnect:  clientDisconnected,
	}
	return result, forwardErr
}

func isGrokInvalidEncryptedContentResponse(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	code := strings.TrimSpace(gjson.GetBytes(body, "code").String())
	message := ""
	errNode := gjson.GetBytes(body, "error")
	switch {
	case errNode.Type == gjson.String:
		message = errNode.String()
	case errNode.IsObject():
		message = firstNonEmpty(errNode.Get("message").String(), errNode.Get("error").String())
		if code == "" {
			code = strings.TrimSpace(errNode.Get("code").String())
		}
	default:
		message = gjson.GetBytes(body, "message").String()
	}
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return false
	}
	if strings.EqualFold(code, "invalid_encrypted_content") {
		return true
	}
	if code != "" && !strings.EqualFold(code, "invalid-argument") {
		return false
	}
	if code == "" && !strings.Contains(normalized, "decrypt") {
		return false
	}
	return strings.Contains(normalized, "encrypted_content") &&
		(strings.Contains(normalized, "decrypt") || strings.Contains(normalized, "unmodified"))
}

func requestHasGrokEncryptedReasoning(body []byte) bool {
	input := gjson.GetBytes(body, "input")
	items := input.Array()
	if input.IsObject() {
		items = []gjson.Result{input}
	}
	for _, item := range items {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			continue
		}
		enc := item.Get("encrypted_content")
		if enc.Exists() && enc.Type != gjson.Null && strings.TrimSpace(enc.String()) != "" {
			return true
		}
	}
	return false
}

type grokEncryptedContentStripRetriedKey struct{}

func markGrokEncryptedContentStripRetried(ctx context.Context) context.Context {
	return context.WithValue(ctx, grokEncryptedContentStripRetriedKey{}, true)
}

func grokEncryptedContentStripRetried(ctx context.Context) bool {
	v, _ := ctx.Value(grokEncryptedContentStripRetriedKey{}).(bool)
	return v
}

func stripAnthropicThinkingSignatures(body []byte) ([]byte, bool) {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"signature"`)) {
		return body, false
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return body, false
	}
	messages, ok := req["messages"].([]any)
	if !ok {
		return body, false
	}
	changed := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, rawBlock := range content {
			block, ok := rawBlock.(map[string]any)
			if !ok || block["type"] != "thinking" {
				continue
			}
			if _, exists := block["signature"]; exists {
				delete(block, "signature")
				changed = true
			}
		}
	}
	if !changed {
		return body, false
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body, false
	}
	return out, true
}

func trimGrokInvalidEncryptedContentRetryBody(body []byte) ([]byte, bool, error) {
	if !requestHasGrokEncryptedReasoning(body) {
		return body, false, nil
	}
	var requestBody map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&requestBody); err != nil {
		return nil, false, err
	}
	if !trimOpenAIEncryptedReasoningItems(requestBody) {
		return body, false, nil
	}
	retryBody, err := json.Marshal(requestBody)
	if err != nil {
		return nil, false, err
	}
	return retryBody, true, nil
}

func resolveGrokUpstreamModel(account *Account, originalModel string) string {
	if account != nil {
		if mapped, matched := account.ResolveMappedModel(originalModel); matched && strings.TrimSpace(mapped) != "" {
			return mapped
		}
	}
	if mapped := xai.DefaultModelMapping()[strings.TrimSpace(originalModel)]; strings.TrimSpace(mapped) != "" {
		return mapped
	}
	return originalModel
}

func patchGrokResponsesBody(body []byte, upstreamModel string) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("invalid json request body")
	}
	out, err := sjson.SetBytes(body, "model", upstreamModel)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokResponsesModelCapabilities(out, upstreamModel)
	if err != nil {
		return nil, err
	}
	for _, unsupportedField := range []string{"prompt_cache_retention", "safety_identifier"} {
		if gjson.GetBytes(out, unsupportedField).Exists() {
			out, err = sjson.DeleteBytes(out, unsupportedField)
			if err != nil {
				return nil, err
			}
		}
	}
	if strings.EqualFold(upstreamModel, "grok-4.5") {
		for _, unsupportedField := range []string{"presence_penalty", "presencePenalty", "frequency_penalty", "frequencyPenalty", "stop"} {
			if gjson.GetBytes(out, unsupportedField).Exists() {
				out, err = sjson.DeleteBytes(out, unsupportedField)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	out, err = sanitizeGrokResponsesUnsupportedFields(out)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokResponsesInput(out)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokReasoningNullContent(out)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokResponsesTools(out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// sanitizeGrokReasoningNullContent removes the null content field that xAI's
// Responses decoder rejects for reasoning input items.
func sanitizeGrokReasoningNullContent(body []byte) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body, nil
	}
	items := input.Array()
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		content := item.Get("content")
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" || !content.Exists() || content.Type != gjson.Null {
			continue
		}
		updated, err := sjson.DeleteBytes(body, fmt.Sprintf("input.%d.content", i))
		if err != nil {
			return nil, err
		}
		body = updated
	}
	return body, nil
}

func sanitizeGrokResponsesModelCapabilities(body []byte, upstreamModel string) ([]byte, error) {
	if !grokModelRejectsReasoningEffort(upstreamModel) {
		return body, nil
	}

	out := body
	for _, field := range []string{"reasoning", "reasoning_effort", "reasoningEffort"} {
		if !gjson.GetBytes(out, field).Exists() {
			continue
		}
		var err error
		out, err = sjson.DeleteBytes(out, field)
		if err != nil {
			return nil, fmt.Errorf("remove unsupported Grok Composer %s: %w", field, err)
		}
	}
	return out, nil
}

func grokModelRejectsReasoningEffort(model string) bool {
	model = strings.TrimSpace(strings.ToLower(model))
	if slash := strings.LastIndex(model, "/"); slash >= 0 {
		model = strings.TrimSpace(model[slash+1:])
	}
	switch model {
	case "grok-composer", "grok-composer-2.5-fast", "composer-2.5":
		return true
	default:
		return false
	}
}

func sanitizeGrokResponsesUnsupportedFields(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"external_web_access"`)) {
		return body, nil
	}
	if !gjson.GetBytes(body, "external_web_access").Exists() {
		return body, nil
	}
	return sjson.DeleteBytes(body, "external_web_access")
}

// additional_tools is a Codex/Responses Lite private input carrier. xAI's
// Responses schema rejects this carrier before inference, while ordinary
// input items remain valid. Top-level supported tools are handled separately.
func sanitizeGrokResponsesInput(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"additional_tools"`)) {
		return body, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body, nil
	}

	rawItems := input.Array()
	filtered := make([]json.RawMessage, 0, len(rawItems))
	for _, item := range rawItems {
		if strings.TrimSpace(item.Get("type").String()) == "additional_tools" {
			continue
		}
		filtered = append(filtered, json.RawMessage(item.Raw))
	}
	if len(filtered) == len(rawItems) {
		return body, nil
	}
	encoded, err := json.Marshal(filtered)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "input", encoded)
}

var grokResponsesSupportedToolTypes = map[string]struct{}{
	"code_execution":     {},
	"code_interpreter":   {},
	"collections_search": {},
	"file_search":        {},
	"function":           {},
	"mcp":                {},
	"shell":              {},
	"web_search":         {},
	"x_search":           {},
}

func sanitizeGrokResponsesTools(body []byte) ([]byte, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body, nil
	}

	rawTools := tools.Array()
	filteredTools := make([]json.RawMessage, 0, len(rawTools))
	for _, tool := range rawTools {
		toolType := strings.TrimSpace(tool.Get("type").String())
		if _, ok := grokResponsesSupportedToolTypes[toolType]; ok {
			filteredTools = append(filteredTools, json.RawMessage(tool.Raw))
		}
	}

	var err error
	if len(filteredTools) != len(rawTools) {
		if len(filteredTools) == 0 {
			body, err = sjson.DeleteBytes(body, "tools")
		} else {
			var encoded []byte
			encoded, err = json.Marshal(filteredTools)
			if err != nil {
				return nil, err
			}
			body, err = sjson.SetRawBytes(body, "tools", encoded)
		}
		if err != nil {
			return nil, err
		}
	}

	toolChoice := gjson.GetBytes(body, "tool_choice")
	if !toolChoice.Exists() {
		return body, nil
	}
	if shouldDropGrokToolChoice(toolChoice, filteredTools) {
		body, err = sjson.DeleteBytes(body, "tool_choice")
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

func shouldDropGrokToolChoice(toolChoice gjson.Result, tools []json.RawMessage) bool {
	if len(tools) == 0 {
		return true
	}
	if !toolChoice.IsObject() {
		return false
	}
	choiceType := strings.TrimSpace(toolChoice.Get("type").String())
	if choiceType == "" {
		return false
	}
	if _, ok := grokResponsesSupportedToolTypes[choiceType]; !ok {
		return true
	}
	if choiceType == "function" {
		choiceName := strings.TrimSpace(toolChoice.Get("name").String())
		if choiceName == "" {
			choiceName = strings.TrimSpace(toolChoice.Get("function.name").String())
		}
		if choiceName == "" {
			return false
		}
		for _, tool := range tools {
			var item struct {
				Type     string `json:"type"`
				Name     string `json:"name"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(tool, &item); err != nil {
				continue
			}
			name := strings.TrimSpace(item.Name)
			if name == "" {
				name = strings.TrimSpace(item.Function.Name)
			}
			if strings.TrimSpace(item.Type) == "function" && name == choiceName {
				return false
			}
		}
		return true
	}
	return false
}

func buildGrokResponsesRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token, cacheIdentity string, cfg *config.Config) (*http.Request, error) {
	SetActualOpenAIUpstreamEndpoint(c, "/v1/responses")
	targetURL, err := buildGrokResponsesURL(account, cfg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	applyGrokOAuthIdentityHeaders(req.Header, targetURL, account.IsGrokOAuth())
	applyGrokCacheHeaders(req.Header, cacheIdentity)
	if c != nil {
		if v := c.GetHeader("OpenAI-Beta"); strings.TrimSpace(v) != "" {
			req.Header.Set("OpenAI-Beta", v)
		}
	}
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

const (
	grokDefaultResponsesModel        = "grok-4.5"
	grokUpstreamUserAgent            = "sub2api-grok/1.0"
	grokCLIVersion                   = "0.2.93"
	grokRateLimitFallbackCooldown    = 2 * time.Minute
	grokRateLimitRepeatCooldown      = 10 * time.Minute
	grokRateLimitSustainedCooldown   = 30 * time.Minute
	grokRateLimitMaxAdaptiveCooldown = time.Hour
	grokRateLimitBackoffQuietPeriod  = time.Hour
)

func applyGrokCLIHeaders(headers http.Header) {
	if headers == nil {
		return
	}
	headers.Set("User-Agent", grokUpstreamUserAgent)
	headers.Set("X-Grok-Client-Version", grokCLIVersion)
}

// applyGrokOAuthIdentityHeaders adds the protocol identity required by the
// official Grok endpoints. Custom or regional OAuth endpoints must receive
// only the neutral gateway user agent, so CLI/relay identity cannot cross a
// user-configured domain.
func applyGrokOAuthIdentityHeaders(headers http.Header, targetURL string, oauth bool) {
	if oauth && isGrokOfficialOAuthTarget(targetURL) {
		applyGrokCLIHeaders(headers)
		return
	}
	if headers != nil {
		headers.Set("User-Agent", grokUpstreamUserAgent)
	}
}

func isGrokOfficialOAuthTarget(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "api.x.ai" || host == "cli-chat-proxy.grok.com"
}

func (s *OpenAIGatewayService) updateGrokUsageSnapshot(ctx context.Context, account *Account, snapshot *xai.QuotaSnapshot) {
	if s == nil || account == nil || account.ID <= 0 || snapshot == nil {
		return
	}
	accountID := account.ID
	now := time.Now()
	resetAt, hasActiveLimit := grokRateLimitResetAtForAccount(account, snapshot, now)
	if hasActiveLimit {
		normalizeGrokExhaustedWindowResets(snapshot, resetAt, now)
	}
	recovery := isSuccessfulGrokRateLimitRecovery(account, snapshot)
	critical := snapshot.StatusCode == http.StatusTooManyRequests || hasActiveLimit || recovery
	if s.codexSnapshotThrottle != nil {
		allowed := s.codexSnapshotThrottle.Allow(accountID, now)
		if !critical && !allowed {
			return
		}
	}

	stateCtx := ctx
	if hasActiveLimit || recovery {
		var cancel context.CancelFunc
		stateCtx, cancel = openAIAccountStateContext(ctx)
		defer cancel()
	}
	if s.accountRepo != nil {
		_ = s.accountRepo.UpdateExtra(stateCtx, accountID, map[string]any{
			grokQuotaSnapshotExtraKey: snapshot,
		})
	}
	// Pool-mode upstream state belongs to the upstream pool. Keep the quota
	// snapshot for observability, but do not persist a generic local block.
	if account.IsPoolMode() {
		return
	}
	if hasActiveLimit {
		s.rateLimitGrok(stateCtx, account, resetAt)
	} else if recovery {
		if clearGrokRateLimitAfterRecovery(stateCtx, s.accountRepo, account) {
			s.clearGrokRateLimitRuntimeBlockAfterRecovery(account)
		}
	}
}

func (s *OpenAIGatewayService) updateGrokUsageFromResponse(ctx context.Context, account *Account, headers http.Header, statusCode int) {
	snapshot := parseGrokQuotaSnapshot(headers, statusCode, time.Now())
	if snapshot != nil {
		s.updateGrokUsageSnapshot(ctx, account, snapshot)
		return
	}
	recoverySnapshot := &xai.QuotaSnapshot{StatusCode: statusCode}
	if isSuccessfulGrokRateLimitRecovery(account, recoverySnapshot) {
		stateCtx, cancel := openAIAccountStateContext(ctx)
		defer cancel()
		if clearGrokRateLimitAfterRecovery(stateCtx, s.accountRepo, account) {
			s.clearGrokRateLimitRuntimeBlockAfterRecovery(account)
		}
	}
}

func parseGrokQuotaSnapshot(headers http.Header, statusCode int, now time.Time) *xai.QuotaSnapshot {
	snapshot := xai.ParseQuotaHeaders(headers, statusCode)
	if snapshot == nil && statusCode == http.StatusTooManyRequests {
		return &xai.QuotaSnapshot{
			StatusCode: statusCode,
			UpdatedAt:  now.UTC().Format(time.RFC3339),
		}
	}
	return snapshot
}

func normalizeGrokExhaustedWindowResets(snapshot *xai.QuotaSnapshot, resetAt, now time.Time) {
	if snapshot == nil || !resetAt.After(now) {
		return
	}
	for _, window := range []*xai.QuotaWindow{snapshot.Requests, snapshot.Tokens} {
		if window == nil || window.Remaining == nil || *window.Remaining > 0 {
			continue
		}
		candidate := time.Time{}
		if window.ResetUnix != nil && *window.ResetUnix > 0 {
			candidate = time.Unix(*window.ResetUnix, 0)
		} else if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(window.ResetAt)); err == nil {
			candidate = parsed
		}
		if !candidate.After(now) {
			candidate = resetAt
		}
		resetUnix := candidate.Unix()
		window.ResetUnix = &resetUnix
		window.ResetAt = candidate.UTC().Format(time.RFC3339)
	}
}

func grokRateLimitResetAt(snapshot *xai.QuotaSnapshot, now time.Time) (time.Time, bool) {
	if snapshot == nil {
		return time.Time{}, false
	}

	retryAfterExpired := false
	var resetAt time.Time
	if snapshot.RetryAfterSeconds != nil && *snapshot.RetryAfterSeconds > 0 {
		observedAt := now
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(snapshot.UpdatedAt)); err == nil {
			observedAt = parsed
		}
		retryAfterResetAt := observedAt.Add(time.Duration(*snapshot.RetryAfterSeconds) * time.Second)
		if retryAfterResetAt.After(now) {
			resetAt = retryAfterResetAt
		} else {
			retryAfterExpired = true
		}
	}

	exhausted := false
	for _, window := range []*xai.QuotaWindow{snapshot.Requests, snapshot.Tokens} {
		if window == nil || window.Remaining == nil || *window.Remaining > 0 {
			continue
		}
		exhausted = true
		candidate := time.Time{}
		if window.ResetUnix != nil && *window.ResetUnix > 0 {
			candidate = time.Unix(*window.ResetUnix, 0)
		} else if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(window.ResetAt)); err == nil {
			candidate = parsed
		}
		if candidate.After(now) && candidate.After(resetAt) {
			resetAt = candidate
		}
	}
	if !resetAt.IsZero() {
		return resetAt, true
	}
	if retryAfterExpired {
		return time.Time{}, false
	}
	if exhausted || snapshot.StatusCode == http.StatusTooManyRequests {
		return now.Add(grokRateLimitFallbackCooldown), true
	}
	return time.Time{}, false
}

func grokRateLimitResetAtForAccount(account *Account, snapshot *xai.QuotaSnapshot, now time.Time) (time.Time, bool) {
	resetAt, limited := grokRateLimitResetAt(snapshot, now)
	if !limited || account == nil || !account.IsGrokOAuth() || snapshot == nil || snapshot.StatusCode != http.StatusTooManyRequests {
		return resetAt, limited
	}
	if account.RateLimitedAt == nil || account.RateLimitResetAt == nil {
		return resetAt, true
	}
	previousResetAt := *account.RateLimitResetAt
	if previousResetAt.After(now) || now.Sub(previousResetAt) > grokRateLimitBackoffQuietPeriod {
		return resetAt, true
	}
	previousCooldown := previousResetAt.Sub(*account.RateLimitedAt)
	if previousCooldown <= 0 {
		return resetAt, true
	}
	adaptiveCooldown := grokRateLimitRepeatCooldown
	switch {
	case previousCooldown >= grokRateLimitSustainedCooldown:
		adaptiveCooldown = grokRateLimitMaxAdaptiveCooldown
	case previousCooldown >= grokRateLimitRepeatCooldown:
		adaptiveCooldown = grokRateLimitSustainedCooldown
	}
	if adaptiveResetAt := now.Add(adaptiveCooldown); adaptiveResetAt.After(resetAt) {
		resetAt = adaptiveResetAt
	}
	return resetAt, true
}

func normalizeGrokRateLimitResetAt(account *Account, resetAt, now time.Time) time.Time {
	if !resetAt.After(now) {
		resetAt = now.Add(grokRateLimitFallbackCooldown)
	}
	if account != nil && account.RateLimitResetAt != nil && account.RateLimitResetAt.After(resetAt) {
		resetAt = *account.RateLimitResetAt
	}
	return resetAt
}

type grokRateLimitExtendingRepository interface {
	SetRateLimitedIfLater(ctx context.Context, id int64, resetAt time.Time) error
}

type grokRateLimitRecoveryRepository interface {
	ClearRateLimitIfObserved(ctx context.Context, id int64, observedLimitedAt, observedResetAt time.Time) (bool, error)
}

func isSuccessfulGrokRateLimitRecovery(account *Account, snapshot *xai.QuotaSnapshot) bool {
	return account != nil && account.IsGrokOAuth() &&
		account.RateLimitedAt != nil && account.RateLimitResetAt != nil &&
		snapshot != nil && snapshot.StatusCode >= http.StatusOK && snapshot.StatusCode < http.StatusMultipleChoices
}

func clearGrokRateLimitAfterRecovery(ctx context.Context, repo AccountRepository, account *Account) bool {
	if repo == nil || account == nil || account.RateLimitedAt == nil || account.RateLimitResetAt == nil || ctx.Err() != nil {
		return false
	}
	recoveryRepo, ok := repo.(grokRateLimitRecoveryRepository)
	if !ok {
		return false
	}
	applied, err := recoveryRepo.ClearRateLimitIfObserved(ctx, account.ID, *account.RateLimitedAt, *account.RateLimitResetAt)
	if err != nil {
		slog.Warn("grok_rate_limit_recovery_clear_failed", "account_id", account.ID, "error", err)
		return false
	}
	return applied
}

func (s *OpenAIGatewayService) clearGrokRateLimitRuntimeBlockAfterRecovery(account *Account) {
	if s == nil || account == nil || account.ID <= 0 || account.RateLimitResetAt == nil {
		return
	}
	now := time.Now()
	if (account.TempUnschedulableUntil != nil && now.Before(*account.TempUnschedulableUntil)) ||
		(account.OverloadUntil != nil && now.Before(*account.OverloadUntil)) {
		return
	}
	// Grok shares only the runtime fast-path map with OpenAI. Do not call the
	// generic clear method here because it also resets OpenAI pool soft cooldowns.
	if current, loaded := s.openaiAccountRuntimeBlockUntil.Load(account.ID); loaded {
		currentUntil, _, ok := parseAccountRuntimeDeadline(current)
		if !ok || currentUntil.After(*account.RateLimitResetAt) ||
			!s.openaiAccountRuntimeBlockUntil.CompareAndDelete(account.ID, current) {
			return
		}
	}
	s.clearOpenAIAccountCooldownInRedis(account.ID)
	// A new local block can race with the Redis delete. Reassert it so the
	// distributed account-slot arbiter cannot miss the newer cooldown.
	if current, loaded := s.openaiAccountRuntimeBlockUntil.Load(account.ID); loaded {
		if currentUntil, generation, ok := parseAccountRuntimeDeadline(current); ok && currentUntil.After(time.Now()) {
			s.storeOpenAIAccountCooldownInRedis(account.ID, currentUntil, generation)
			return
		}
	}
	s.publishOpenAISchedulingRuntimeEvent(context.Background(), SchedulerEventAccountUpdated, account.ID, "grok_rate_limit_recovered")
}

func persistGrokRateLimit(ctx context.Context, repo AccountRepository, account *Account, resetAt time.Time) {
	if repo == nil || account == nil || account.ID <= 0 || account.IsPoolMode() {
		return
	}
	resetAt = normalizeGrokRateLimitResetAt(account, resetAt, time.Now())
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	var err error
	if extendingRepo, ok := repo.(grokRateLimitExtendingRepository); ok {
		err = extendingRepo.SetRateLimitedIfLater(stateCtx, account.ID, resetAt)
	} else {
		err = repo.SetRateLimited(stateCtx, account.ID, resetAt)
	}
	if err != nil {
		slog.Warn("persist_grok_rate_limit_failed", "account_id", account.ID, "reset_at", resetAt.UTC(), "error", err)
	}
}

func (s *OpenAIGatewayService) rateLimitGrok(ctx context.Context, account *Account, resetAt time.Time) {
	if s == nil || account == nil || account.IsPoolMode() {
		return
	}
	resetAt = normalizeGrokRateLimitResetAt(account, resetAt, time.Now())
	runtimeUntil := resetAt
	if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(runtimeUntil) {
		runtimeUntil = *account.TempUnschedulableUntil
	}
	s.BlockAccountScheduling(account, runtimeUntil, "429")
	persistGrokRateLimit(ctx, s.accountRepo, account, resetAt)
}

func (s *OpenAIGatewayService) handleGrokAccountUpstreamError(ctx context.Context, account *Account, statusCode int, headers http.Header, responseBody []byte) {
	if s == nil || account == nil {
		return
	}
	now := time.Now()
	s.updateGrokUsageSnapshot(ctx, account, parseGrokQuotaSnapshot(headers, statusCode, now))
	if account.IsPoolMode() {
		return
	}
	if isGrokContentPolicyRejection(statusCode, responseBody) {
		return
	}
	switch statusCode {
	case http.StatusUnauthorized:
		s.tempUnscheduleGrok(ctx, account, 10*time.Minute, "grok credentials unauthorized")
	case http.StatusPaymentRequired:
		s.tempUnscheduleGrok(ctx, account, 30*time.Minute, "grok payment required")
	case http.StatusForbidden:
		s.tempUnscheduleGrok(ctx, account, 30*time.Minute, "grok access or entitlement denied")
	case http.StatusTooManyRequests:
		// updateGrokUsageSnapshot installs the runtime and durable rate-limit state.
	default:
		if statusCode >= 500 && !account.IsPoolMode() {
			s.tempUnscheduleGrok(ctx, account, 2*time.Minute, "grok upstream temporary error")
		}
	}
	_ = responseBody
}

func (s *OpenAIGatewayService) tempUnscheduleGrok(ctx context.Context, account *Account, cooldown time.Duration, reason string) {
	if s == nil || account == nil || account.IsPoolMode() {
		return
	}
	until := time.Now().Add(cooldown)
	if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(until) {
		until = *account.TempUnschedulableUntil
	}
	if account.IsGrokOAuth() {
		repo, ok := s.accountRepo.(grokOAuthConditionalStateRepository)
		if !ok {
			slog.Warn("grok_conditional_state_repository_missing", "account_id", account.ID)
			return
		}
		stateCtx, cancel := openAIAccountStateContext(ctx)
		defer cancel()
		applied, err := repo.SetGrokOAuthTempUnschedulableIfCredentialsUnchanged(
			stateCtx, account.ID, cloneCredentials(account.Credentials),
			cloneInt64Pointer(account.ProxyID), until, reason,
		)
		if err != nil {
			slog.Warn("persist_grok_temp_unschedulable_failed", "account_id", account.ID, "error", err)
			return
		}
		if !applied {
			return
		}
		s.BlockAccountScheduling(account, until, reason)
		return
	}
	s.BlockAccountScheduling(account, until, reason)
	if s.accountRepo != nil {
		stateCtx, cancel := openAIAccountStateContext(ctx)
		defer cancel()
		_ = s.accountRepo.SetTempUnschedulable(stateCtx, account.ID, until, reason)
	}
}
