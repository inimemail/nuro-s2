package service

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIPoolRequestFailoverError_ConnectionError(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          101,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}

	failoverErr := svc.newOpenAIPoolRequestFailoverError(nil, account, nil, errors.New("tls handshake timeout"), false)

	require.NotNil(t, failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.RetryableOnSameAccount)
}

func TestOpenAIPoolRequestFailoverError_NonPoolIgnored(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       102,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
	}

	failoverErr := svc.newOpenAIPoolRequestFailoverError(nil, account, nil, errors.New("tls handshake timeout"), false)

	require.Nil(t, failoverErr)
}

func TestClassifyOpenAIPoolFailover_ImageCapabilityErrorSwitchesWithoutSameAccountRetry(t *testing.T) {
	account := &Account{
		ID:          103,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"error":{"message":"Image generation is not enabled for this group","type":"permission_error"}}`)

	decision := classifyOpenAIPoolFailover(account, http.StatusForbidden, "Image generation is not enabled for this group", body)

	require.True(t, decision.Failover)
	require.False(t, decision.RetryableOnSameAccount)
	require.Equal(t, OpenAIImagesCapabilityNative, decision.ProbeCapability)
}

func TestOpenAIImagesUpstreamError_ImageCapabilityInfersForbiddenWithoutSameAccountRetry(t *testing.T) {
	account := &Account{
		ID:          106,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	upstreamErr := openAIImagesUpstreamErrorFromGJSON(gjson.Parse(`{
		"type":"permission_error",
		"message":"Image generation is not enabled for this group"
	}`), "")

	require.NotNil(t, upstreamErr)
	require.Equal(t, http.StatusForbidden, upstreamErr.StatusCode)
	require.True(t, upstreamErr.ShouldFailover(account))

	failoverErr := upstreamErr.ToFailoverError(account)
	require.NotNil(t, failoverErr)
	require.Equal(t, http.StatusForbidden, failoverErr.StatusCode)
	require.False(t, failoverErr.RetryableOnSameAccount)

	decision := classifyOpenAIPoolFailover(account, failoverErr.StatusCode, failoverErr.Message, failoverErr.ResponseBody)
	require.True(t, decision.Failover)
	require.Equal(t, OpenAIImagesCapabilityNative, decision.ProbeCapability)
}

func TestOpenAIImagesUpstreamError_PoolContentPolicyErrorDoesNotFailover(t *testing.T) {
	account := &Account{
		ID:          107,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	upstreamErr := &OpenAIImagesUpstreamError{
		StatusCode: http.StatusForbidden,
		ErrorType:  "invalid_request_error",
		Message:    "Your request was rejected by the content policy",
	}

	require.False(t, upstreamErr.ShouldFailover(account))
}

func TestClassifyOpenAIPoolFailover_ClientRequestErrorDoesNotSwitch(t *testing.T) {
	account := &Account{
		ID:          104,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"error":{"message":"Missing required parameter: prompt","type":"invalid_request_error"}}`)

	decision := classifyOpenAIPoolFailover(account, http.StatusBadRequest, "Missing required parameter: prompt", body)

	require.False(t, decision.Failover)
	require.False(t, decision.RetryableOnSameAccount)
}

func TestOpenAIPoolSoftCooldownState_ExposesReasonUntilCleared(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          105,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}

	svc.MarkOpenAIPoolAccountSoftCooldownWithContext(nil, account, http.StatusForbidden, []byte(`{"error":{"message":"invalid api key"}}`), openAIPoolSoftCooldownContext{})

	state := svc.OpenAIPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.False(t, state.Due)
	require.Equal(t, http.StatusForbidden, state.StatusCode)
	require.Contains(t, state.Reason, "invalid api key")

	svc.openaiPoolSoftCooldownUntil.Store(account.ID, time.Now().Add(-time.Second))
	state = svc.OpenAIPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.True(t, state.Due)

	svc.ClearAccountSchedulingBlock(account.ID)
	state = svc.OpenAIPoolSoftCooldownState(account.ID)
	require.False(t, state.Cooling)
}

func TestClassifyOpenAIEmbeddedUpstreamError_APIReturned429(t *testing.T) {
	body := []byte(`{"error":{"message":"API returned 429: {\"error\":{\"message\":\"Upstream rate limit exceeded, please retry later\",\"type\":\"rate_limit_error\"}}"}}`)

	statusCode, msg, ok := classifyOpenAIEmbeddedUpstreamError(body)

	require.True(t, ok)
	require.Equal(t, http.StatusTooManyRequests, statusCode)
	require.Contains(t, msg, "API returned 429")
}

func TestClassifyOpenAIEmbeddedUpstreamError_UpstreamRequestFailed(t *testing.T) {
	statusCode, msg, ok := classifyOpenAIEmbeddedUpstreamError([]byte("Upstream request failed"))

	require.True(t, ok)
	require.Equal(t, http.StatusBadGateway, statusCode)
	require.Equal(t, "Upstream request failed", msg)
}

func TestClassifyOpenAIEmbeddedUpstreamError_UserErrorIgnored(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"error":{"message":"invalid input","type":"invalid_request_error"}}`),
		[]byte(`{"error":{"message":"model not found","code":"model_not_found"}}`),
		[]byte(`{"id":"chatcmpl_ok","object":"chat.completion","choices":[]}`),
	} {
		statusCode, _, ok := classifyOpenAIEmbeddedUpstreamError(body)
		require.False(t, ok)
		require.Zero(t, statusCode)
	}
}
