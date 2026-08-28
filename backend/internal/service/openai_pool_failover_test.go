package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type openAIPoolManualRuntimeBlockRecorder struct {
	clearedIDs []int64
}

func (r *openAIPoolManualRuntimeBlockRecorder) BlockAccountScheduling(account *Account, until time.Time, reason string) {
}

func (r *openAIPoolManualRuntimeBlockRecorder) ClearAccountSchedulingBlock(accountID int64) {
	r.clearedIDs = append(r.clearedIDs, accountID)
}

type openAIPoolSchedulableRepo struct {
	AccountRepository
	account              *Account
	setSchedulableValues []bool
}

func (r *openAIPoolSchedulableRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	return r.account, nil
}

func (r *openAIPoolSchedulableRepo) SetSchedulable(ctx context.Context, id int64, schedulable bool) error {
	r.setSchedulableValues = append(r.setSchedulableValues, schedulable)
	r.account.Schedulable = schedulable
	return nil
}

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

func TestOpenAIPoolRequestFailoverError_BodyStartedStopsSameAccountReplay(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          102,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	attempt := newOpenAIUpstreamAttempt()
	attempt.markBodyWriteStarted()
	req, err := http.NewRequestWithContext(withOpenAIUpstreamAttempt(context.Background(), attempt), http.MethodPost, "https://example.test/v1/responses", nil)
	require.NoError(t, err)

	failoverErr := svc.newOpenAIPoolRequestFailoverError(nil, account, req, errors.New("connection reset by peer"), false)

	require.NotNil(t, failoverErr)
	require.True(t, failoverErr.UpstreamRequestBodyStarted)
	require.False(t, failoverErr.RetryableOnSameAccount)
}

func TestOpenAIPoolRequestFailoverError_BuiltinRetryDisabled(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       101,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                       true,
			"pool_mode_retry_status_codes":    []any{},
			"pool_mode_builtin_retry_enabled": false,
		},
	}

	failoverErr := svc.newOpenAIPoolRequestFailoverError(nil, account, nil, errors.New("tls handshake timeout"), false)

	require.NotNil(t, failoverErr)
	require.False(t, failoverErr.RetryableOnSameAccount)
}

func TestOpenAIPoolResponseHeaderTimeoutTriggersImmediateSoftCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       121,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                          true,
			"pool_soft_cooldown_error_threshold": 3,
		},
	}

	failoverErr := svc.newOpenAIPoolRequestFailoverError(
		nil,
		account,
		nil,
		errors.New(`Post "https://example.test/v1/responses": http2: timeout awaiting response headers`),
		false,
	)

	require.NotNil(t, failoverErr)
	require.True(t, failoverErr.immediatePoolSoftCooldown)
	require.False(t, failoverErr.RetryableOnSameAccount)

	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr, "gpt-5.4")

	state := svc.OpenAIPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.Equal(t, "openai", state.ProbeKind)
	require.Equal(t, "gpt-5.4", state.ProbeModel)
}

func TestOpenAIPoolCommittedResponseHeaderTimeoutTriggersImmediateSoftCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       122,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                          true,
			"pool_soft_cooldown_error_threshold": 3,
		},
	}

	svc.RecordOpenAIPoolFailureAfterCommittedResponse(
		context.Background(),
		account,
		http.StatusBadGateway,
		openAITransportFailoverBody,
		"gpt-5.4",
		`Post "https://example.test/v1/responses": http2: timeout awaiting response headers`,
	)

	require.True(t, svc.OpenAIPoolSoftCooldownState(account.ID).Cooling)
}

func TestOpenAIPoolMediaResponseHeaderTimeoutKeepsExistingCooldownPolicy(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       123,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                          true,
			"pool_soft_cooldown_error_threshold": 3,
		},
	}
	req := httptest.NewRequest(http.MethodPost, "https://example.test/v1/images/generations", nil)
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAIMedia))

	failoverErr := svc.newOpenAIPoolRequestFailoverError(
		nil,
		account,
		req,
		errors.New("http2: timeout awaiting response headers"),
		false,
	)

	require.NotNil(t, failoverErr)
	require.False(t, failoverErr.immediatePoolSoftCooldown)

	mediaAccount := *account
	mediaAccount.ID = 124
	svc.RecordOpenAIPoolFailureAfterCommittedResponse(
		context.Background(),
		&mediaAccount,
		http.StatusBadGateway,
		openAITransportFailoverBody,
		"gpt-image-2",
		`Post "https://example.test/v1/responses": http2: timeout awaiting response headers`,
	)
	require.False(t, svc.OpenAIPoolSoftCooldownState(mediaAccount.ID).Cooling)
}

func TestOpenAIPoolResponseHeaderTimeoutRespectsSoftCooldownDisabled(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       125,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                  true,
			"pool_soft_cooldown_enabled": false,
		},
	}
	failoverErr := svc.newOpenAIPoolRequestFailoverError(
		nil,
		account,
		nil,
		errors.New("http2: timeout awaiting response headers"),
		false,
	)

	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr, "gpt-5.4")

	require.False(t, svc.OpenAIPoolSoftCooldownState(account.ID).Cooling)
}

func TestOpenAIPoolUpstream502MessageDoesNotTriggerImmediateHeaderCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       126,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                          true,
			"pool_soft_cooldown_error_threshold": 3,
		},
	}
	responseBody := []byte(`{"error":{"message":"timeout awaiting response headers"}}`)

	svc.RecordOpenAIPoolFailureAfterCommittedResponse(
		context.Background(),
		account,
		http.StatusBadGateway,
		responseBody,
		"gpt-5.4",
		"timeout awaiting response headers",
	)

	require.False(t, svc.OpenAIPoolSoftCooldownState(account.ID).Cooling)
}

func TestOpenAIPoolRequestFailoverError_Explicit502OverridesBuiltinDisabled(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       101,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                       true,
			"pool_mode_retry_status_codes":    []any{float64(http.StatusBadGateway)},
			"pool_mode_builtin_retry_enabled": false,
		},
	}

	failoverErr := svc.newOpenAIPoolRequestFailoverError(nil, account, nil, errors.New("tls handshake timeout"), false)

	require.NotNil(t, failoverErr)
	require.True(t, failoverErr.RetryableOnSameAccount)
}

func TestOpenAIPoolRetryConditionsAreMergedWithoutBuiltinOverride(t *testing.T) {
	account := &Account{
		ID:       101,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                       true,
			"pool_mode_retry_status_codes":    []any{float64(http.StatusServiceUnavailable)},
			"pool_mode_builtin_retry_enabled": false,
		},
	}

	require.True(t, openAIPoolFailoverRetryableOnSameAccount(account, http.StatusServiceUnavailable, "temporary outage", nil))
	require.False(t, openAIPoolFailoverRetryableOnSameAccount(account, http.StatusBadGateway, "temporary outage", nil))
	require.False(t, openAIPoolFailoverRetryableOnSameAccount(account, http.StatusTooManyRequests, "rate limited", nil))

	account.Credentials["pool_mode_builtin_retry_enabled"] = true
	require.True(t, openAIPoolFailoverRetryableOnSameAccount(account, http.StatusBadGateway, "temporary outage", nil))
	// 429 remains authoritative in the explicit status-code list even when the
	// transient system-error rule is enabled.
	require.False(t, openAIPoolFailoverRetryableOnSameAccount(account, http.StatusTooManyRequests, "rate limited", nil))
}

func TestOpenAIPoolRaceRetryRulesPreferExactStatusOver5xx(t *testing.T) {
	account := &Account{
		ID:       108,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true, "upstream_concurrency_race_enabled": true,
			"upstream_concurrency_race_http_rules": []any{
				map[string]any{"matcher": "5xx", "max_retries": 2},
				map[string]any{"matcher": "502", "max_retries": 1},
				map[string]any{"matcher": "503", "max_retries": 3},
				map[string]any{"matcher": "504", "max_retries": 4},
			},
		},
	}

	key, limit, matched := account.OpenAIUpstreamConcurrencyRaceRetryRule(http.StatusBadGateway)
	require.True(t, matched)
	require.Equal(t, "502", key)
	require.Equal(t, 1, limit)
	key, limit, matched = account.OpenAIUpstreamConcurrencyRaceRetryRule(http.StatusServiceUnavailable)
	require.True(t, matched)
	require.Equal(t, "503", key)
	require.Equal(t, 3, limit)
	key, limit, matched = account.OpenAIUpstreamConcurrencyRaceRetryRule(http.StatusGatewayTimeout)
	require.True(t, matched)
	require.Equal(t, "504", key)
	require.Equal(t, 4, limit)
	key, limit, matched = account.OpenAIUpstreamConcurrencyRaceRetryRule(http.StatusHTTPVersionNotSupported)
	require.True(t, matched)
	require.Equal(t, "5xx", key)
	require.Equal(t, 2, limit)
}

func TestOpenAIPoolRaceExactZeroRuleBlocks5xxFallback(t *testing.T) {
	account := &Account{
		ID: 110, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true, "upstream_concurrency_race_enabled": true,
			"upstream_concurrency_race_http_rules": []any{
				map[string]any{"matcher": "5xx", "max_retries": 2},
				map[string]any{"matcher": "504", "max_retries": 0},
			},
		},
	}

	key, limit, matched := account.OpenAIUpstreamConcurrencyRaceRetryRule(http.StatusGatewayTimeout)
	require.True(t, matched)
	require.Equal(t, "504", key)
	require.Zero(t, limit)
	require.False(t, OpenAIPoolFailoverRetryableOnSameAccount(account, http.StatusGatewayTimeout, "", nil))
}

func TestOpenAIPoolRaceTransportRuleRequiresNoHTTPStatus(t *testing.T) {
	account := &Account{
		ID: 109, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "upstream_concurrency_race_enabled": true},
	}
	key, limit, matched := account.OpenAIUpstreamConcurrencyRaceRetryRule(0)
	require.True(t, matched)
	require.Equal(t, "transport", key)
	require.Equal(t, 1, limit)
	_, _, matched = account.OpenAIUpstreamConcurrencyRaceRetryRule(http.StatusBadGateway)
	require.True(t, matched)

	account.Credentials["upstream_concurrency_race_transport_enabled"] = false
	_, _, matched = account.OpenAIUpstreamConcurrencyRaceRetryRule(0)
	require.False(t, matched)
}

func TestOpenAIStreamRaceTransportRuleKeepsClientErrorsNonRetryable(t *testing.T) {
	setGinTestMode()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                                   true,
			"upstream_concurrency_race_enabled":           true,
			"upstream_concurrency_race_transport_enabled": true,
		},
	}
	svc := &OpenAIGatewayService{}

	clientErr := svc.newOpenAIStreamFailoverError(
		c,
		account,
		true,
		"",
		nil,
		"invalid_request_error: missing required parameter model",
	)
	require.False(t, clientErr.RetryableOnSameAccount)
	require.Empty(t, clientErr.RetryRuleKey)
	require.Zero(t, clientErr.RetryRuleLimit)

	disconnectErr := svc.newOpenAIStreamFailoverError(
		c,
		account,
		true,
		"",
		nil,
		"OpenAI stream disconnected before completion",
	)
	require.True(t, disconnectErr.RetryableOnSameAccount)
	require.Equal(t, "transport", disconnectErr.RetryRuleKey)
	require.Equal(t, 1, disconnectErr.RetryRuleLimit)
}

func TestOpenAIStreamFailedEventRespectsOrdinaryPoolRetryBoundaries(t *testing.T) {
	rateLimitPayload := []byte(`{"type":"response.failed","response":{"error":{"type":"rate_limit_error","message":"rate limited"}}}`)
	capacityPayload := []byte(`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"The selected model is at capacity"}}}`)
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                       true,
			"pool_mode_retry_status_codes":    []any{},
			"pool_mode_builtin_retry_enabled": false,
		},
	}

	require.False(t, openAIStreamFailedEventRetryableOnSameAccount(account, rateLimitPayload, "rate limited"))
	require.False(t, openAIStreamFailedEventRetryableOnSameAccount(account, capacityPayload, "The selected model is at capacity"))

	account.Credentials["pool_mode_builtin_retry_enabled"] = true
	require.False(t, openAIStreamFailedEventRetryableOnSameAccount(account, rateLimitPayload, "rate limited"), "built-in retry must not re-enable semantic 429")
	require.True(t, openAIStreamFailedEventRetryableOnSameAccount(account, capacityPayload, "The selected model is at capacity"))
}

func TestOpenAIStreamRaceUsesHTTPRuleForSemanticFailedEvent(t *testing.T) {
	setGinTestMode()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                                   true,
			"upstream_concurrency_race_enabled":           true,
			"upstream_concurrency_race_transport_enabled": true,
			"upstream_concurrency_race_http_rules": []any{
				map[string]any{"matcher": "429", "max_retries": float64(4)},
				map[string]any{"matcher": "5xx", "max_retries": float64(2)},
			},
		},
	}
	svc := &OpenAIGatewayService{}

	rateLimitErr := svc.newOpenAIStreamFailoverError(c, account, true, "", []byte(`{"type":"response.failed","response":{"error":{"type":"rate_limit_error","message":"rate limited"}}}`), "rate limited")
	require.Equal(t, http.StatusTooManyRequests, rateLimitErr.StatusCode)
	require.Equal(t, "429", rateLimitErr.RetryRuleKey)
	require.Equal(t, 4, rateLimitErr.RetryRuleLimit)
	require.False(t, rateLimitErr.RetryRuleTransport)

	capacityErr := svc.newOpenAIStreamFailoverError(c, account, true, "", []byte(`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"server is overloaded"}}}`), "server is overloaded")
	require.Equal(t, http.StatusBadGateway, capacityErr.StatusCode)
	require.Equal(t, "5xx", capacityErr.RetryRuleKey)
	require.Equal(t, 2, capacityErr.RetryRuleLimit)
	require.False(t, capacityErr.RetryRuleTransport)
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

func TestClassifyOpenAIPoolFailover_ContextLengthUserErrorDoesNotSwitch(t *testing.T) {
	account := &Account{
		ID:          108,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"error":{"message":"maximum context length exceeded","type":"context_length_exceeded"}}`)

	decision := classifyOpenAIPoolFailover(account, http.StatusBadRequest, "maximum context length exceeded", body)

	require.False(t, decision.Failover)
	require.False(t, decision.RetryableOnSameAccount)
	require.False(t, decision.SkipSoftCooldown)
}

func TestClassifyOpenAIPoolFailover_UpstreamModelRoutingErrorSwitchesWithoutSoftCooldown(t *testing.T) {
	account := &Account{
		ID:          118,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"error":{"message":"Model \"gpt-5.4-mini\" is not supported by any configured account in this group","type":"model_not_found"}}`)

	decision := classifyOpenAIPoolFailover(
		account,
		http.StatusNotFound,
		`Model "gpt-5.4-mini" is not supported by any configured account in this group`,
		body,
	)

	require.True(t, decision.Failover)
	require.False(t, decision.RetryableOnSameAccount)
	require.True(t, decision.SkipSoftCooldown)
}

func TestClassifyOpenAIPoolFailover_UpstreamModelRoutingProtectionCanBeDisabled(t *testing.T) {
	account := &Account{
		ID:          119,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"error":{"message":"Model \"gpt-5.4-mini\" is not supported by any configured account in this group","type":"model_not_found"}}`)

	decision := classifyOpenAIPoolFailoverWithModelLimitProtection(
		account,
		http.StatusNotFound,
		`Model "gpt-5.4-mini" is not supported by any configured account in this group`,
		body,
		false,
	)

	require.False(t, decision.Failover)
	require.False(t, decision.SkipSoftCooldown)
}

func TestClassifyOpenAIPoolFailover_AccountModelCapabilityErrorSwitchesAccount(t *testing.T) {
	account := &Account{
		ID:          120,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"error":{"message":"model is not available for this account","type":"model_not_found"}}`)

	decision := classifyOpenAIPoolFailover(account, http.StatusNotFound, "model is not available for this account", body)

	require.True(t, decision.Failover)
	require.False(t, decision.RetryableOnSameAccount)
	require.True(t, decision.SkipSoftCooldown)
}

func TestClassifyOpenAIPoolFailover_AccountModelCapability403SwitchesAccount(t *testing.T) {
	account := &Account{
		ID:          122,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	message := "model is not available for this account"

	decision := classifyOpenAIPoolFailover(account, http.StatusForbidden, message, []byte(`{"error":{"message":"model is not available for this account"}}`))

	require.True(t, decision.Failover)
	require.False(t, decision.RetryableOnSameAccount)
	require.True(t, decision.SkipSoftCooldown)
}

func TestOpenAIPoolFailoverRetryableOnSameAccount_AccountModelCapabilityIsBlocked(t *testing.T) {
	account := &Account{
		ID:          124,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}

	require.False(t, OpenAIPoolFailoverRetryableOnSameAccount(
		account,
		http.StatusForbidden,
		"model is not available for this account",
		[]byte(`{"error":{"message":"model is not available for this account"}}`),
	))
}

func TestClassifyOpenAIPoolFailover_Generic403ModelErrorDoesNotBecomeModelRoutingError(t *testing.T) {
	account := &Account{
		ID:          123,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"error":{"message":"model not found","type":"model_not_found"}}`)

	require.False(t, isOpenAIPoolModelRoutingError(http.StatusForbidden, "model not found", body))
	decision := classifyOpenAIPoolFailover(account, http.StatusForbidden, "model not found", body)
	require.False(t, decision.SkipSoftCooldown)
}

func TestClassifyOpenAIPoolFailover_GenericModelNotFoundRemainsClientErrorWhenProtectionDisabled(t *testing.T) {
	account := &Account{
		ID:          121,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"error":{"message":"model not found","type":"model_not_found"}}`)

	decision := classifyOpenAIPoolFailoverWithModelLimitProtection(account, http.StatusNotFound, "model not found", body, false)

	require.False(t, decision.Failover)
	require.False(t, decision.RetryableOnSameAccount)
	require.False(t, decision.SkipSoftCooldown)
}

func TestClassifyOpenAIPoolFailover_AccountPermissionErrorStillSwitches(t *testing.T) {
	account := &Account{
		ID:          109,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"error":{"message":"model is not available for your account","type":"permission_denied"}}`)

	decision := classifyOpenAIPoolFailover(account, http.StatusServiceUnavailable, "model is not available for your account", body)

	require.True(t, decision.Failover)
	require.True(t, decision.RetryableOnSameAccount)
	require.False(t, decision.SkipSoftCooldown)
}

func TestClassifyOpenAIPoolFailover_DownstreamRoutingErrorSwitchesWithoutSoftCooldown(t *testing.T) {
	account := &Account{
		ID:          110,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(503)}},
	}
	body := []byte(`{"error":{"message":"No available channel for model gpt-image-1 under group GPT-Image-2 (distributor)"}}`)

	decision := classifyOpenAIPoolFailover(account, http.StatusServiceUnavailable, "No available channel for model gpt-image-1", body)

	require.True(t, decision.Failover)
	require.False(t, decision.RetryableOnSameAccount)
	require.True(t, decision.SkipSoftCooldown)
}

func TestClassifyOpenAIPoolFailover_UnknownProviderModelIsUserError(t *testing.T) {
	account := &Account{
		ID:          111,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(502)}},
	}
	body := []byte(`{"error":{"message":"unknown provider customer-router for model user-typed-wrong-model"}}`)

	decision := classifyOpenAIPoolFailover(account, http.StatusBadGateway, "unknown provider customer-router for model user-typed-wrong-model", body)

	require.False(t, decision.Failover)
	require.False(t, decision.RetryableOnSameAccount)
	require.False(t, decision.SkipSoftCooldown)
}

func TestClassifyOpenAIPoolFailover_ClientConfig503SwitchesWithoutSoftCooldown(t *testing.T) {
	account := &Account{
		ID:          111,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(503)}},
	}
	body := []byte(`{"error":{"message":"503 请求体错误：可能与 re 开头错误、/v1 错误、Codex 自动审核或节点/TUN 模式有关，可尝试关闭自动审核或设置 review_model=\"gpt-5.4\""}}`)

	decision := classifyOpenAIPoolFailover(account, http.StatusServiceUnavailable, "请求体错误", body)

	require.True(t, decision.Failover)
	require.False(t, decision.RetryableOnSameAccount)
	require.True(t, decision.SkipSoftCooldown)
}

func TestOpenAIPoolFailoverSwitch_DownstreamRoutingErrorSkipsSoftCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          112,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"error":{"message":"No available channel for model gpt-image-1 under group GPT-Image-2 (distributor)"}}`)
	failoverErr := &UpstreamFailoverError{
		StatusCode:           http.StatusServiceUnavailable,
		ResponseBody:         body,
		Message:              "No available channel for model gpt-image-1",
		SkipPoolSoftCooldown: true,
	}

	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr, "gpt-image-1")

	require.False(t, svc.isOpenAIPoolAccountSoftCooling(account))
}

func TestOpenAIPoolFailoverSwitch_ClientConfig503SkipsSoftCooldownWithoutExplicitFlag(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          117,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(503)}},
	}
	body := []byte(`{"error":{"message":"503 请求体错误：可能与 re 开头错误、/v1 错误、Codex 自动审核或节点/TUN 模式有关"}}`)
	failoverErr := &UpstreamFailoverError{
		StatusCode:             http.StatusServiceUnavailable,
		ResponseBody:           body,
		Message:                "503 请求体错误",
		RetryableOnSameAccount: true,
	}

	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr, "gpt-5.5")

	require.False(t, svc.isOpenAIPoolAccountSoftCooling(account))
}

func TestOpenAIPoolFailoverSwitch_UserModelErrorSkipsSoftCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          116,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"error":{"message":"unknown provider customer-router for model user-typed-wrong-model"}}`)
	failoverErr := &UpstreamFailoverError{
		StatusCode:   http.StatusBadGateway,
		ResponseBody: body,
		Message:      "unknown provider customer-router for model user-typed-wrong-model",
	}

	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr, "user-typed-wrong-model")

	require.False(t, svc.isOpenAIPoolAccountSoftCooling(account))
}

func TestOpenAIPoolFailoverSwitch_ImagePoolDefaultsToImageProbe(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       113,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                          true,
			"image_pool_mode":                    true,
			"pool_soft_cooldown_error_threshold": 1,
		},
	}
	failoverErr := &UpstreamFailoverError{
		StatusCode:   529,
		ResponseBody: []byte(`{"error":{"message":"overloaded"}}`),
		Message:      "overloaded",
	}

	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr)

	state := svc.OpenAIPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.Equal(t, "images", state.ProbeKind)
}

func TestOpenAIPoolFailoverSwitch_PreservesExplicitImageProbeFields(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          114,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_soft_cooldown_error_threshold": 1},
	}
	failoverErr := &UpstreamFailoverError{
		StatusCode:      529,
		ResponseBody:    []byte(`{"error":{"message":"overloaded"}}`),
		Message:         "overloaded",
		ProbeCapability: OpenAIImagesCapabilityNative,
		ProbeModel:      "image-alias",
		ProbeKind:       "images",
	}

	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr)

	state := svc.OpenAIPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.Equal(t, "images", state.ProbeKind)
	require.Equal(t, "image-alias", state.ProbeModel)
}

func TestOpenAIPoolFailoverSwitch_DefaultSoftCooldownThresholdRequiresThreeErrors(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          118,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	failoverErr := &UpstreamFailoverError{
		StatusCode:   http.StatusInternalServerError,
		ResponseBody: []byte(`{"error":{"message":"server error"}}`),
		Message:      "server error",
	}

	for i := 0; i < 2; i++ {
		svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr)
		require.False(t, svc.OpenAIPoolSoftCooldownState(account.ID).Cooling)
	}

	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr)
	require.True(t, svc.OpenAIPoolSoftCooldownState(account.ID).Cooling)
}

func TestOpenAIPoolFailoverSwitch_SuccessResetsSoftCooldownFailureThreshold(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          119,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_soft_cooldown_error_threshold": 3},
	}
	failoverErr := &UpstreamFailoverError{
		StatusCode:   http.StatusInternalServerError,
		ResponseBody: []byte(`{"error":{"message":"server error"}}`),
		Message:      "server error",
	}

	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr)
	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr)
	require.False(t, svc.OpenAIPoolSoftCooldownState(account.ID).Cooling)

	// A normal request success must not reset the pool failure generation;
	// recovery probe is the sole pool cooldown exit.
	svc.ReportOpenAIAccountScheduleResultForRequest(account, "gpt-test", true, nil)

	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr)
	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr)
	require.True(t, svc.OpenAIPoolSoftCooldownState(account.ID).Cooling)
	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr)
	require.True(t, svc.OpenAIPoolSoftCooldownState(account.ID).Cooling)
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
	require.LessOrEqual(t, time.Until(state.Until), openAIPoolSoftCooldownMax+time.Second)

	svc.openaiPoolSoftCooldownUntil.Store(account.ID, time.Now().Add(-time.Second))
	state = svc.OpenAIPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.True(t, state.Due)

	svc.ClearAccountSchedulingBlock(account.ID)
	state = svc.OpenAIPoolSoftCooldownState(account.ID)
	require.False(t, state.Cooling)
}

func TestOpenAIPoolSoftCooldown_CapsLongCooldownsAtOneMinute(t *testing.T) {
	svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{}}
	account := &Account{
		ID:          115,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"error":{"type":"rate_limit_exceeded","message":"rate limited","resets_in_seconds":600}}`)

	svc.MarkOpenAIPoolAccountSoftCooldownWithContext(context.Background(), account, http.StatusTooManyRequests, body, openAIPoolSoftCooldownContext{})

	state := svc.OpenAIPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.False(t, state.Due)
	require.Equal(t, http.StatusTooManyRequests, state.StatusCode)
	require.LessOrEqual(t, time.Until(state.Until), openAIPoolSoftCooldownMax+time.Second)
	require.Greater(t, time.Until(state.Until), openAIPoolSoftCooldownMax-5*time.Second)
}

func TestRecoverAccountState_ClearsRuntimeOnlyPoolSoftCooldown(t *testing.T) {
	repo := stubOpenAIAccountRepo{accounts: []Account{
		{
			ID:          108,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Credentials: map[string]any{"pool_mode": true},
		},
	}}
	blocker := &openAIPoolManualRuntimeBlockRecorder{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	svc.SetAccountRuntimeBlocker(blocker)

	result, err := svc.RecoverAccountState(context.Background(), 108, AccountRecoveryOptions{})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.ClearedError)
	require.False(t, result.ClearedRateLimit)
	require.Equal(t, []int64{108}, blocker.clearedIDs)
}

func TestSetAccountSchedulable_DisablingClearsRuntimePoolSoftCooldown(t *testing.T) {
	repo := &openAIPoolSchedulableRepo{account: &Account{
		ID:          109,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"pool_mode": true},
	}}
	blocker := &openAIPoolManualRuntimeBlockRecorder{}
	svc := &adminServiceImpl{accountRepo: repo, runtimeBlocker: blocker}

	updated, err := svc.SetAccountSchedulable(context.Background(), 109, false)

	require.NoError(t, err)
	require.NotNil(t, updated)
	require.False(t, updated.Schedulable)
	require.Equal(t, []bool{false}, repo.setSchedulableValues)
	require.Equal(t, []int64{109}, blocker.clearedIDs)
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
