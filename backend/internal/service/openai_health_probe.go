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

	openAIHealthProbeContextKey      = "openai_responses_health_probe"
	openAIHealthProbeSessionPrefix   = "openai-health-probe-"
	openAIHealthProbeMaxBodyBytes    = 8 * 1024
	openAIHealthProbeMaxOutputTokens = 512
	openAIHealthProbeErrorCode       = "monitor_probe_empty_response"
	openAIHealthProbeUpstreamMessage = "OpenAI health probe returned 2xx without assistant text"
	openAIHealthProbeClientMessage   = "OpenAI health probe exhausted available accounts without assistant text"
	openAIHealthProbeInstructions    = "Return exactly MONITOR_OK as plain text."
	openAIHealthProbeInput           = "Return exactly MONITOR_OK."
)

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
	model = strings.TrimSpace(model)
	bodyModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" || bodyModel == "" || model != bodyModel {
		return fmt.Errorf("health probe requires a non-empty model matching the request body")
	}
	if stream || gjson.GetBytes(body, "stream").Type != gjson.False {
		return fmt.Errorf("health probe requires stream=false")
	}
	if len(body) == 0 || len(body) > openAIHealthProbeMaxBodyBytes {
		return fmt.Errorf("health probe body must be between 1 and %d bytes", openAIHealthProbeMaxBodyBytes)
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
	if reasoning := gjson.GetBytes(body, "reasoning"); reasoning.Exists() && reasoning.Get("effort").String() != "none" {
		return fmt.Errorf("health probe only supports omitted reasoning or reasoning.effort=none")
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
