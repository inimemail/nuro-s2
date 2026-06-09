package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIEmbeddedUpstreamErrorBodyLimit = 1 << 20

type openAIPoolFailoverDecision struct {
	Failover               bool
	RetryableOnSameAccount bool
	ProbeCapability        OpenAIImagesCapability
	SkipSoftCooldown       bool
}

type openAIPoolSoftCooldownContext struct {
	ProbeCapability OpenAIImagesCapability
	ProbeModel      string
	ProbeKind       string
	CooldownSource  string
	StatusCode      int
	Reason          string
	LastProbeStatus int
	LastProbeReason string
}

func (s *OpenAIGatewayService) newOpenAIPoolRequestFailoverError(
	c *gin.Context,
	account *Account,
	upstreamReq *http.Request,
	err error,
	passthrough bool,
) *UpstreamFailoverError {
	if account == nil || !account.IsOpenAI() || !account.IsPoolMode() || err == nil {
		return nil
	}
	safeErr := sanitizeUpstreamErrorMessage(err.Error())
	if safeErr == "" {
		safeErr = "upstream request failed"
	}
	setOpsUpstreamError(c, http.StatusBadGateway, safeErr, "")
	event := OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: http.StatusBadGateway,
		Passthrough:        passthrough,
		Kind:               "failover",
		Message:            safeErr,
	}
	if upstreamReq != nil && upstreamReq.URL != nil {
		event.UpstreamURL = safeUpstreamURL(upstreamReq.URL.String())
	}
	appendOpsUpstreamError(c, event)
	body := openAIUpstreamFailoverErrorBody("Upstream request failed")
	return &UpstreamFailoverError{
		StatusCode:             http.StatusBadGateway,
		ResponseBody:           body,
		Message:                safeErr,
		RetryableOnSameAccount: true,
	}
}

func (s *OpenAIGatewayService) newOpenAIPoolEmbeddedFailoverError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	resp *http.Response,
	body []byte,
	requestedModel string,
	passthrough bool,
) *UpstreamFailoverError {
	if account == nil || !account.IsOpenAI() || !account.IsPoolMode() {
		return nil
	}
	statusCode, msg, ok := classifyOpenAIEmbeddedUpstreamError(body)
	if !ok {
		return nil
	}
	decision := classifyOpenAIPoolFailover(account, statusCode, msg, body)
	if !decision.Failover {
		return nil
	}
	msg = sanitizeUpstreamErrorMessage(strings.TrimSpace(msg))
	if msg == "" {
		msg = "Upstream request failed"
	}
	upstreamDetail := ""
	if s != nil && s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	responseBody := openAIUpstreamFailoverErrorBody(msg)
	if len(body) > 0 {
		responseBody = append([]byte(nil), body...)
	}
	header := http.Header(nil)
	requestID := ""
	if resp != nil {
		header = resp.Header
		requestID = strings.TrimSpace(resp.Header.Get("x-request-id"))
	}
	setOpsUpstreamError(c, statusCode, msg, upstreamDetail)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: statusCode,
		UpstreamRequestID:  requestID,
		Passthrough:        passthrough,
		Kind:               "failover",
		Message:            msg,
		Detail:             upstreamDetail,
	})
	if requestedModel != "" {
		s.handleOpenAIAccountUpstreamError(ctx, account, statusCode, header, responseBody, requestedModel)
	} else {
		s.handleOpenAIAccountUpstreamError(ctx, account, statusCode, header, responseBody)
	}
	return &UpstreamFailoverError{
		StatusCode:             statusCode,
		ResponseBody:           responseBody,
		ResponseHeaders:        cloneHTTPHeader(header),
		Message:                msg,
		ProbeModel:             strings.TrimSpace(requestedModel),
		ProbeKind:              openAIPoolProbeKindForModel(requestedModel),
		RetryableOnSameAccount: decision.RetryableOnSameAccount,
		SkipPoolSoftCooldown:   decision.SkipSoftCooldown,
	}
}

func openAIPoolProbeKindForModel(model string) string {
	if isOpenAIImageGenerationModel(model) {
		return "images"
	}
	return "openai"
}

func classifyOpenAIPoolFailover(account *Account, statusCode int, upstreamMsg string, upstreamBody []byte) openAIPoolFailoverDecision {
	if account == nil || !account.IsPoolMode() {
		return openAIPoolFailoverDecision{}
	}
	if isOpenAIPoolUserRequestedModelError(statusCode, upstreamMsg, upstreamBody) {
		return openAIPoolFailoverDecision{}
	}
	if isOpenAIPoolExplicitClientRequestError(statusCode, upstreamMsg, upstreamBody) {
		return openAIPoolFailoverDecision{}
	}
	if isOpenAIPoolDownstreamRoutingOrClientConfigError(statusCode, upstreamMsg, upstreamBody) {
		return openAIPoolFailoverDecision{
			Failover:         true,
			SkipSoftCooldown: true,
		}
	}
	decision := openAIPoolFailoverDecision{
		RetryableOnSameAccount: openAIPoolFailoverRetryableOnSameAccount(account, statusCode, upstreamMsg, upstreamBody),
	}
	if isOpenAIPoolImageCapabilityError(statusCode, upstreamMsg, upstreamBody) {
		decision.Failover = true
		decision.ProbeCapability = OpenAIImagesCapabilityNative
		decision.RetryableOnSameAccount = false
		return decision
	}
	if statusCode == 0 || statusCode == http.StatusRequestTimeout || statusCode == http.StatusTooManyRequests ||
		statusCode == 529 || statusCode >= 500 || account.IsPoolModeRetryableStatus(statusCode) ||
		isOpenAITransientProcessingError(statusCode, upstreamMsg, upstreamBody) ||
		isOpenAIPoolAccountLevelClientError(statusCode, upstreamMsg, upstreamBody) {
		decision.Failover = true
		return decision
	}
	return decision
}

func openAIPoolFailoverRetryableOnSameAccount(account *Account, statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if account == nil || !account.IsPoolMode() {
		return false
	}
	if isOpenAIPoolUserRequestedModelError(statusCode, upstreamMsg, upstreamBody) ||
		isOpenAIPoolExplicitClientRequestError(statusCode, upstreamMsg, upstreamBody) ||
		isOpenAIPoolImageCapabilityError(statusCode, upstreamMsg, upstreamBody) {
		return false
	}
	if isOpenAIPoolDownstreamRoutingOrClientConfigError(statusCode, upstreamMsg, upstreamBody) {
		return false
	}
	if account.IsPoolModeRetryableStatus(statusCode) {
		return true
	}
	if statusCode == 0 || statusCode >= 500 || statusCode == http.StatusRequestTimeout || statusCode == http.StatusTooManyRequests {
		return true
	}
	return isOpenAITransientProcessingError(statusCode, upstreamMsg, upstreamBody)
}

func isOpenAIPoolAccountLevelClientError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != http.StatusUnauthorized && statusCode != http.StatusForbidden && statusCode != http.StatusPaymentRequired {
		return false
	}
	combined := openAIPoolCombinedErrorText(upstreamMsg, upstreamBody)
	if combined == "" {
		return true
	}
	markers := []string{
		"unauthorized",
		"forbidden",
		"permission_error",
		"authentication",
		"api key",
		"apikey",
		"invalid key",
		"invalid_api_key",
		"expired key",
		"quota",
		"insufficient",
		"billing",
		"credit",
		"no credit",
		"balance",
		"no permission",
		"not enabled",
		"not authorized",
		"not allowed for this account",
		"not available for your account",
		"organization",
		"workspace",
		"project",
		"ip",
		"geo",
		"country",
		"region",
		"restricted",
	}
	return containsAnySubstring(combined, markers...)
}

func isOpenAIPoolUserRequestedModelError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusNotFound &&
		statusCode != http.StatusBadGateway && statusCode != http.StatusServiceUnavailable &&
		statusCode != http.StatusGatewayTimeout {
		return false
	}
	combined := openAIPoolCombinedErrorText(upstreamMsg, upstreamBody)
	if combined == "" {
		return false
	}
	if strings.Contains(combined, "model") {
		if containsAnySubstring(combined,
			"unknown provider",
			"no provider",
			"provider not found",
			"model provider not found",
			"model route not found",
			"no route",
			"no upstream",
		) {
			return true
		}
	}
	return containsAnySubstring(combined,
		"model_not_found",
		"model not found",
		"model does not exist",
		"model doesn't exist",
		"unknown model",
		"unsupported model",
	)
}

func isOpenAIPoolDownstreamRoutingOrClientConfigError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != http.StatusBadGateway && statusCode != http.StatusServiceUnavailable && statusCode != http.StatusGatewayTimeout {
		return false
	}
	combined := openAIPoolCombinedErrorText(upstreamMsg, upstreamBody)
	if combined == "" {
		return false
	}
	markers := []string{
		"no available channel",
		"no available channels",
		"no available account",
		"no available accounts",
		"no available upstream",
		"no available provider",
		"no available route",
		"no channel available",
		"unknown provider for model",
		"unknown provider",
		"no provider for model",
		"provider not found for model",
		"model provider not found",
		"model route not found",
		"no route for model",
		"no upstream for model",
		"request body error",
		"invalid request body",
		"malformed request body",
		"请求体错误",
		"codex auto review",
		"auto review",
		"自动审核",
		"review_model",
		"review model",
		"tun mode",
		"tun 模式",
		"node/tun",
		"节点/tun",
		"/v1 error",
		"/v1 错误",
	}
	return containsAnySubstring(combined, markers...)
}

func isOpenAIPoolExplicitClientRequestError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode == http.StatusForbidden {
		if isOpenAIPoolImageCapabilityError(statusCode, upstreamMsg, upstreamBody) ||
			isOpenAIPoolAccountLevelClientError(statusCode, upstreamMsg, upstreamBody) {
			return false
		}
		combined := openAIPoolCombinedErrorText(upstreamMsg, upstreamBody)
		return containsAnySubstring(combined,
			"content policy",
			"policy_violation",
			"safety",
			"moderation",
			"high-risk cyber",
			"violat",
			"prompt was rejected",
			"input was rejected",
		)
	}
	if statusCode != http.StatusBadRequest && statusCode != http.StatusNotFound &&
		statusCode != http.StatusRequestEntityTooLarge && statusCode != http.StatusUnprocessableEntity &&
		statusCode != http.StatusMethodNotAllowed && statusCode != http.StatusUnsupportedMediaType {
		return false
	}
	if isOpenAITransientProcessingError(statusCode, upstreamMsg, upstreamBody) {
		return false
	}
	combined := openAIPoolCombinedErrorText(upstreamMsg, upstreamBody)
	if combined == "" {
		return statusCode == http.StatusBadRequest || statusCode == http.StatusNotFound ||
			statusCode == http.StatusRequestEntityTooLarge || statusCode == http.StatusUnprocessableEntity
	}
	if isOpenAIPoolImageCapabilityError(statusCode, upstreamMsg, upstreamBody) ||
		isOpenAIPoolAccountLevelClientError(statusCode, upstreamMsg, upstreamBody) {
		return false
	}
	markers := []string{
		"invalid_request",
		"invalid request",
		"bad request",
		"missing required",
		"unsupported parameter",
		"unknown parameter",
		"invalid parameter",
		"invalid type",
		"invalid value",
		"model_not_found",
		"model not found",
		"not found",
		"payload too large",
		"request entity too large",
		"context length",
		"maximum context",
		"too many tokens",
		"content policy",
		"policy_violation",
		"safety",
		"moderation",
		"violat",
		"not supported",
		"unsupported",
		"method not allowed",
	}
	return containsAnySubstring(combined, markers...)
}

func isOpenAIPoolImageCapabilityError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != http.StatusForbidden && statusCode != http.StatusBadRequest && statusCode != http.StatusNotFound {
		return false
	}
	combined := openAIPoolCombinedErrorText(upstreamMsg, upstreamBody)
	if combined == "" {
		return false
	}
	return strings.Contains(combined, "image generation is not enabled for this group") ||
		(strings.Contains(combined, "image generation") && strings.Contains(combined, "not enabled")) ||
		(strings.Contains(combined, "images") && strings.Contains(combined, "not enabled")) ||
		(strings.Contains(combined, "image") && strings.Contains(combined, "permission"))
}

func openAIPoolCombinedErrorText(upstreamMsg string, upstreamBody []byte) string {
	parts := []string{strings.TrimSpace(upstreamMsg)}
	if len(upstreamBody) > 0 {
		for _, path := range []string{
			"error.message", "error.type", "error.code", "error.param",
			"type", "code", "message", "detail",
			"response.error.message", "response.error.type", "response.error.code",
		} {
			if v := strings.TrimSpace(gjson.GetBytes(upstreamBody, path).String()); v != "" {
				parts = append(parts, v)
			}
		}
		if len(upstreamBody) <= 4096 {
			parts = append(parts, string(upstreamBody))
		}
	}
	return strings.ToLower(strings.Join(parts, " "))
}

func containsAnySubstring(text string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func openAIUpstreamFailoverErrorBody(message string) []byte {
	message = sanitizeUpstreamErrorMessage(strings.TrimSpace(message))
	if message == "" {
		message = "Upstream request failed"
	}
	body, err := json.Marshal(gin.H{
		"error": gin.H{
			"type":    "upstream_error",
			"message": message,
		},
	})
	if err != nil {
		return []byte(`{"error":{"type":"upstream_error","message":"Upstream request failed"}}`)
	}
	return body
}

func classifyOpenAIEmbeddedUpstreamError(body []byte) (int, string, bool) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || len(trimmed) > openAIEmbeddedUpstreamErrorBodyLimit {
		return 0, "", false
	}
	if !gjson.Valid(trimmed) {
		return classifyOpenAIErrorText(trimmed)
	}

	if errObj := gjson.Get(trimmed, "error"); errObj.Exists() && errObj.IsObject() {
		return classifyOpenAIErrorObject(errObj, trimmed)
	}
	if strings.EqualFold(strings.TrimSpace(gjson.Get(trimmed, "type").String()), "error") {
		return classifyOpenAIErrorObject(gjson.Parse(trimmed), trimmed)
	}
	if msg := strings.TrimSpace(gjson.Get(trimmed, "message").String()); msg != "" && jsonLooksLikePlainError(trimmed) {
		return classifyOpenAIErrorText(msg)
	}
	return 0, "", false
}

func classifyOpenAIErrorObject(errObj gjson.Result, raw string) (int, string, bool) {
	msg := strings.TrimSpace(errObj.Get("message").String())
	if msg == "" {
		msg = strings.TrimSpace(errObj.Get("error.message").String())
	}
	code := strings.TrimSpace(errObj.Get("code").String())
	errType := strings.TrimSpace(errObj.Get("type").String())
	statusCode := int(errObj.Get("status_code").Int())
	if statusCode <= 0 {
		statusCode = int(errObj.Get("status").Int())
	}
	if statusCode <= 0 {
		statusCode = inferOpenAIEmbeddedStatus(msg, code, errType, raw)
	}
	if msg == "" {
		msg = firstNonEmptyString(code, errType, "Upstream request failed")
	}
	if statusCode <= 0 {
		return 0, "", false
	}
	return statusCode, msg, true
}

func classifyOpenAIErrorText(text string) (int, string, bool) {
	msg := sanitizeUpstreamErrorMessage(strings.TrimSpace(text))
	if msg == "" {
		return 0, "", false
	}
	statusCode := inferOpenAIEmbeddedStatus(msg, "", "", text)
	if statusCode <= 0 {
		return 0, "", false
	}
	return statusCode, msg, true
}

func inferOpenAIEmbeddedStatus(message, code, errType, raw string) int {
	combined := strings.ToLower(strings.TrimSpace(message + " " + code + " " + errType + " " + raw))
	if combined == "" {
		return 0
	}
	if status := parseAPIReturnedStatus(combined); status > 0 {
		return status
	}
	switch {
	case strings.Contains(combined, "api returned 429"),
		strings.Contains(combined, "rate_limit_error"),
		strings.Contains(combined, "rate limit exceeded"),
		strings.Contains(combined, "too many requests"):
		return http.StatusTooManyRequests
	case strings.Contains(combined, "api returned 401"):
		return http.StatusUnauthorized
	case strings.Contains(combined, "api returned 403"):
		return http.StatusForbidden
	case strings.Contains(combined, "image generation is not enabled for this group"),
		(strings.Contains(combined, "image generation") && strings.Contains(combined, "not enabled")),
		(strings.Contains(combined, "images") && strings.Contains(combined, "not enabled")):
		return http.StatusForbidden
	case strings.Contains(combined, "api returned 502"):
		return http.StatusBadGateway
	case strings.Contains(combined, "api returned 503"):
		return http.StatusServiceUnavailable
	case strings.Contains(combined, "api returned 504"):
		return http.StatusGatewayTimeout
	case strings.Contains(combined, "api returned 524"):
		return 524
	case strings.Contains(combined, "upstream request failed"),
		strings.Contains(combined, "upstream service temporarily unavailable"),
		strings.Contains(combined, "bad gateway"),
		strings.Contains(combined, "gateway timeout"),
		strings.Contains(combined, "origin connect timeout"):
		return http.StatusBadGateway
	default:
		return 0
	}
}

func parseAPIReturnedStatus(text string) int {
	idx := strings.Index(strings.ToLower(text), "api returned ")
	if idx < 0 {
		return 0
	}
	rest := strings.TrimSpace(text[idx+len("api returned "):])
	fields := strings.FieldsFunc(rest, func(r rune) bool {
		return r < '0' || r > '9'
	})
	if len(fields) == 0 {
		return 0
	}
	status, err := strconv.Atoi(fields[0])
	if err != nil || status < 100 || status > 599 {
		return 0
	}
	return status
}

func jsonLooksLikePlainError(raw string) bool {
	value := gjson.Parse(raw)
	if !value.IsObject() {
		return false
	}
	for _, key := range []string{"id", "object", "choices", "output", "data", "usage"} {
		if value.Get(key).Exists() {
			return false
		}
	}
	return true
}
