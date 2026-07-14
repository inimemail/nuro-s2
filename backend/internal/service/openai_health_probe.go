package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	OpenAIHealthProbeHeader             = "X-Sub2API-Health-Probe"
	OpenAIHealthProbeProfileResponsesV1 = "openai-responses-v1"
	OpenAIHealthProbeMaxAccountSwitches = 2
	OpenAIHealthProbeTotalTimeout       = 40 * time.Second
	OpenAIHealthProbeGrabMaxElapsed     = 1500 * time.Millisecond

	openAIHealthProbeContextKey               = "openai_responses_health_probe"
	openAIHealthProbeSessionPrefix            = "openai-health-probe-"
	openAIHealthProbeMaxBodyBytes             = 8 * 1024
	openAIHealthProbeMaxOutputTokens          = 512
	openAIHealthProbeAlternativeLookupTimeout = 500 * time.Millisecond
	openAIHealthProbeErrorCode                = "monitor_probe_empty_response"
	openAIHealthProbeUpstreamMessage          = "OpenAI health probe returned 2xx without assistant text"
	openAIHealthProbeClientMessage            = "OpenAI health probe exhausted available accounts without assistant text"
	openAIHealthProbeInstructions             = "Return exactly MONITOR_OK as plain text."
	openAIHealthProbeInput                    = "Return exactly MONITOR_OK."
)

type openAIHealthProbeRequestContextKey struct{}

func WithOpenAIHealthProbeRequestContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openAIHealthProbeRequestContextKey{}, true)
}

func IsOpenAIHealthProbeRequestContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	marked, _ := ctx.Value(openAIHealthProbeRequestContextKey{}).(bool)
	return marked
}

func ConfigureOpenAIResponsesHealthProbe(c *gin.Context, body []byte, model string, stream bool) (bool, error) {
	if c == nil {
		return false, nil
	}
	profile := strings.TrimSpace(c.GetHeader(OpenAIHealthProbeHeader))
	if profile == "" {
		return false, nil
	}
	if !strings.EqualFold(profile, OpenAIHealthProbeProfileResponsesV1) {
		return false, fmt.Errorf("unsupported %s profile", OpenAIHealthProbeHeader)
	}
	if c.Request == nil || c.Request.URL == nil || !strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/responses") {
		return false, fmt.Errorf("health probe only supports /v1/responses")
	}
	if err := validateOpenAIResponsesHealthProbeBody(body, model, stream); err != nil {
		return false, err
	}
	c.Set(openAIHealthProbeContextKey, strings.TrimSpace(model))
	return true, nil
}

func validateOpenAIResponsesHealthProbeBody(body []byte, model string, stream bool) error {
	if len(body) == 0 || len(body) > openAIHealthProbeMaxBodyBytes {
		return fmt.Errorf("health probe body must be between 1 and %d bytes", openAIHealthProbeMaxBodyBytes)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return fmt.Errorf("health probe body must be a JSON object")
	}
	allowedFields := map[string]struct{}{
		"model": {}, "instructions": {}, "input": {}, "max_output_tokens": {},
		"stream": {}, "store": {}, "reasoning": {},
	}
	for field := range fields {
		if _, allowed := allowedFields[field]; !allowed {
			return fmt.Errorf("health probe body does not support field %q", field)
		}
	}
	model = strings.TrimSpace(model)
	bodyModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" || bodyModel == "" || model != bodyModel {
		return fmt.Errorf("health probe requires a non-empty model matching the request body")
	}
	if stream || gjson.GetBytes(body, "stream").Type != gjson.False {
		return fmt.Errorf("health probe requires stream=false")
	}
	if strings.TrimSpace(gjson.GetBytes(body, "instructions").String()) != openAIHealthProbeInstructions ||
		strings.TrimSpace(gjson.GetBytes(body, "input").String()) != openAIHealthProbeInput {
		return fmt.Errorf("health probe instructions and input must match the %s template", OpenAIHealthProbeProfileResponsesV1)
	}
	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && (!tools.IsArray() || len(tools.Array()) > 0) {
		return fmt.Errorf("health probe does not support tools")
	}
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" {
		return fmt.Errorf("health probe does not support previous_response_id")
	}
	if reasoningRaw, exists := fields["reasoning"]; exists {
		var reasoning map[string]json.RawMessage
		if err := json.Unmarshal(reasoningRaw, &reasoning); err != nil || len(reasoning) != 1 || strings.TrimSpace(gjson.GetBytes(reasoningRaw, "effort").String()) != "none" {
			return fmt.Errorf("health probe only supports omitted reasoning or reasoning.effort=none")
		}
		if _, exists := reasoning["effort"]; !exists {
			return fmt.Errorf("health probe only supports omitted reasoning or reasoning.effort=none")
		}
	}
	if maxTokens := gjson.GetBytes(body, "max_output_tokens"); maxTokens.Type != gjson.Number || maxTokens.Float() != openAIHealthProbeMaxOutputTokens {
		return fmt.Errorf("health probe requires max_output_tokens=%d", openAIHealthProbeMaxOutputTokens)
	}
	store := gjson.GetBytes(body, "store")
	if store.Type != gjson.False {
		return fmt.Errorf("health probe requires store=false")
	}
	return nil
}

func IsOpenAIResponsesHealthProbe(c *gin.Context) bool {
	_, ok := OpenAIResponsesHealthProbeModel(c)
	return ok
}

func OpenAIResponsesHealthProbeModel(c *gin.Context) (string, bool) {
	if c == nil {
		return "", false
	}
	value, ok := c.Get(openAIHealthProbeContextKey)
	model, valid := value.(string)
	model = strings.TrimSpace(model)
	return model, ok && valid && model != ""
}

func NewOpenAIHealthProbeSessionHash() string {
	return openAIHealthProbeSessionPrefix + uuid.NewString()
}

func IsOpenAIHealthProbeSessionHash(sessionHash string) bool {
	value := strings.TrimSpace(sessionHash)
	return strings.HasPrefix(value, openAIHealthProbeSessionPrefix) && len(value) > len(openAIHealthProbeSessionPrefix)
}

func (s *OpenAIGatewayService) HasOpenAIHealthProbeAlternativeAccount(ctx context.Context, current *Account, req OpenAIAccountScheduleRequest) bool {
	if s == nil || current == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lookupCtx, cancel := context.WithTimeout(ctx, openAIHealthProbeAlternativeLookupTimeout)
	defer cancel()

	platform := normalizeOpenAICompatibleRequestPlatform(req.RequestPlatform)
	accounts, err := s.listSchedulableAccountsForPlatform(lookupCtx, req.GroupID, platform)
	if err != nil || len(accounts) == 0 {
		return false
	}

	var schedGroup *Group
	if req.GroupID != nil && s.schedulerSnapshot != nil {
		schedGroup, _ = s.schedulerSnapshot.GetGroupByID(lookupCtx, *req.GroupID)
	}

	candidates := make([]*Account, 0, len(accounts))
	loadReq := make([]AccountWithConcurrency, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if account.ID == current.ID || account.Priority != current.Priority {
			continue
		}
		if _, excluded := req.ExcludedIDs[account.ID]; excluded {
			continue
		}
		if s.schedulerSnapshot != nil {
			account, err = s.getSchedulableAccount(lookupCtx, account.ID)
			if err != nil || account == nil || account.Priority != current.Priority {
				continue
			}
		}
		if !isOpenAIAccountEligibleForRequest(lookupCtx, account, req.RequestedModel, req.RequireCompact, req.RequiredCapability, req.RequiredImageCapability, platform) {
			continue
		}
		if !parentHealthyForShadow(account, s.parentAccountLookup(lookupCtx)) ||
			s.isOpenAIAccountRuntimeBlocked(account) || s.isOpenAIPoolAccountSoftCooling(account) {
			continue
		}
		if schedGroup != nil && schedGroup.RequirePrivacySet && !account.IsPrivacySet() {
			continue
		}
		if req.RequiredTransport != OpenAIUpstreamTransportAny &&
			req.RequiredTransport != OpenAIUpstreamTransportHTTPSSE &&
			!s.isOpenAIAccountTransportCompatible(account, req.RequiredTransport) {
			continue
		}
		if !s.latestOpenAIAccountMatchesGroup(lookupCtx, account, req.GroupID) {
			continue
		}
		if req.GroupID != nil && s.needsUpstreamChannelRestrictionCheck(lookupCtx, req.GroupID) &&
			s.isUpstreamModelRestrictedByChannel(lookupCtx, *req.GroupID, account, req.RequestedModel, req.RequireCompact) {
			continue
		}
		candidates = append(candidates, account)
		loadReq = append(loadReq, AccountWithConcurrency{ID: account.ID, MaxConcurrency: account.EffectiveLoadFactor()})
	}
	if len(candidates) == 0 {
		return false
	}
	if lookupCtx.Err() != nil {
		return false
	}
	if s.concurrencyService == nil {
		return true
	}
	type loadResult struct {
		loadMap map[int64]*AccountLoadInfo
		err     error
	}
	loadResultCh := make(chan loadResult, 1)
	go func() {
		loadMap, loadErr := s.concurrencyService.GetAccountsLoadBatchFresh(lookupCtx, loadReq)
		loadResultCh <- loadResult{loadMap: loadMap, err: loadErr}
	}()
	var loadMap map[int64]*AccountLoadInfo
	select {
	case <-lookupCtx.Done():
		return false
	case result := <-loadResultCh:
		if result.err != nil {
			return false
		}
		loadMap = result.loadMap
	}
	for _, candidate := range candidates {
		loadInfo := loadMap[candidate.ID]
		if loadInfo == nil || loadInfo.LoadRate < 100 {
			return true
		}
	}
	return false
}

func (s *OpenAIGatewayService) ReleaseOpenAIHealthProbeSession(ctx context.Context, groupID *int64, sessionHash string) {
	if s == nil || !IsOpenAIHealthProbeSessionHash(sessionHash) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = s.deleteStickySessionAccountID(cleanupCtx, groupID, sessionHash)
}

func IsolateOpenAIHealthProbeFailover(c *gin.Context, failoverErr *UpstreamFailoverError) {
	if !IsOpenAIResponsesHealthProbe(c) || failoverErr == nil {
		return
	}
	failoverErr.SkipPoolSoftCooldown = true
	failoverErr.SkipPromptCacheAvoidance = true
	failoverErr.SkipStickySessionEviction = true
	failoverErr.SkipSchedulePenalty = true
}

// ApplyOpenAIHealthProbeRetryPolicy aligns pool accounts with the pool's
// same-account retry classifier. Non-pool accounts keep their existing retry
// decision. Both gain one probe-only condition: an upstream 2xx response whose
// assistant text is empty is retryable.
func ApplyOpenAIHealthProbeRetryPolicy(c *gin.Context, account *Account, failoverErr *UpstreamFailoverError) {
	if !IsOpenAIResponsesHealthProbe(c) || failoverErr == nil {
		return
	}
	retryable := failoverErr.RetryableOnSameAccount
	if account != nil && account.IsPoolMode() {
		retryable = openAIPoolFailoverRetryableOnSameAccount(account, failoverErr.StatusCode, failoverErr.Message, failoverErr.ResponseBody)
	}
	failoverErr.RetryableOnSameAccount = IsOpenAIHealthProbeEmptyErrorBody(failoverErr.ResponseBody) || retryable
}

func openAIUpstreamRequestContext(ctx context.Context, c *gin.Context) (context.Context, context.CancelFunc) {
	if IsOpenAIResponsesHealthProbe(c) {
		if ctx == nil {
			return context.Background(), func() {}
		}
		return ctx, func() {}
	}
	return detachUpstreamContext(ctx)
}

func newOpenAIHealthProbeEmptyFailoverError(c *gin.Context, account *Account, resp *http.Response, body []byte) *UpstreamFailoverError {
	probeModel, isProbe := OpenAIResponsesHealthProbeModel(c)
	if !isProbe || resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	if strings.TrimSpace(extractOpenAIResponsesText(body)) != "" {
		return nil
	}

	responseBody := openAIHealthProbeErrorBody()
	upstreamRequestID := strings.TrimSpace(resp.Header.Get("x-request-id"))
	setOpsUpstreamError(c, resp.StatusCode, openAIHealthProbeUpstreamMessage, "")
	if account != nil {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  upstreamRequestID,
			Kind:               "health_probe_empty",
			Message:            openAIHealthProbeUpstreamMessage,
		})
	}

	failoverErr := &UpstreamFailoverError{
		StatusCode:                http.StatusBadGateway,
		ResponseBody:              responseBody,
		ResponseHeaders:           resp.Header.Clone(),
		Message:                   openAIHealthProbeUpstreamMessage,
		ProbeModel:                probeModel,
		SkipPoolSoftCooldown:      true,
		SkipPromptCacheAvoidance:  true,
		SkipStickySessionEviction: true,
		SkipSchedulePenalty:       true,
	}
	ApplyOpenAIHealthProbeRetryPolicy(c, account, failoverErr)
	IsolateOpenAIHealthProbeFailover(c, failoverErr)
	return failoverErr
}

func openAIHealthProbeErrorBody() []byte {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "upstream_error",
			"code":    openAIHealthProbeErrorCode,
			"message": openAIHealthProbeUpstreamMessage,
		},
	})
	if err != nil {
		return []byte(`{"error":{"type":"upstream_error","code":"monitor_probe_empty_response","message":"OpenAI health probe returned 2xx without assistant text"}}`)
	}
	return body
}

func IsOpenAIHealthProbeEmptyErrorBody(body []byte) bool {
	return strings.TrimSpace(gjson.GetBytes(body, "error.code").String()) == openAIHealthProbeErrorCode
}

func OpenAIHealthProbeClientMessage() string {
	return openAIHealthProbeClientMessage
}

func ShouldStartOpenAIHealthProbeDefaultFallback(c *gin.Context, failoverErr *UpstreamFailoverError, alreadyStarted bool) bool {
	return !alreadyStarted && IsOpenAIResponsesHealthProbe(c) && failoverErr != nil &&
		IsOpenAIHealthProbeEmptyErrorBody(failoverErr.ResponseBody)
}

func BuildOpenAIHealthProbeDefaultFallbackBody(model string) ([]byte, error) {
	challenge := generateChallenge()
	return providerOpenAIResponsesAdapter.buildBody(strings.TrimSpace(model), challenge.Prompt)
}
