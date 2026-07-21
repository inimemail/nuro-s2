package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClassifyAnthropicPoolFailover_DownstreamRoutingErrorSkipsSoftCooldown(t *testing.T) {
	account := &Account{
		ID:          301,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(503)}},
	}
	body := []byte(`{"error":{"message":"No available accounts: no available accounts","type":"api_error"}}`)

	decision := classifyAnthropicPoolFailover(account, http.StatusServiceUnavailable, "No available accounts", body, "claude-sonnet-4-6")

	require.True(t, decision.Failover)
	require.True(t, decision.SkipSoftCooldown)
	require.False(t, decision.RetryableOnSame)
}

func TestClassifyAnthropicPoolFailover_ServerErrorKeepsSoftCooldown(t *testing.T) {
	account := &Account{
		ID:          302,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_soft_cooldown_error_threshold": 1},
	}
	body := []byte(`{"error":{"message":"upstream overloaded","type":"api_error"}}`)

	decision := classifyAnthropicPoolFailover(account, http.StatusServiceUnavailable, "upstream overloaded", body, "claude-sonnet-4-6")

	require.True(t, decision.Failover)
	require.False(t, decision.SkipSoftCooldown)
}

func TestClassifyAnthropicPoolFailover_UserRequestErrorsSkipSoftCooldown(t *testing.T) {
	account := &Account{
		ID:          306,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(400), float64(403), float64(503)}},
	}
	tests := []struct {
		name       string
		statusCode int
		message    string
		body       []byte
	}{
		{
			name:       "invalid_request",
			statusCode: http.StatusBadRequest,
			message:    "invalid request",
			body:       []byte(`{"error":{"type":"invalid_request_error","message":"messages: text content blocks must be non-empty"}}`),
		},
		{
			name:       "context_too_long",
			statusCode: http.StatusBadRequest,
			message:    "maximum context length exceeded",
			body:       []byte(`{"error":{"type":"invalid_request_error","message":"context length exceeded"}}`),
		},
		{
			name:       "tool_schema",
			statusCode: http.StatusBadRequest,
			message:    "tool schema is invalid",
			body:       []byte(`{"error":{"type":"invalid_request_error","message":"tool schema invalid json schema"}}`),
		},
		{
			name:       "wrapped_503_invalid_request",
			statusCode: http.StatusServiceUnavailable,
			message:    "API returned 400: invalid_request_error",
			body:       []byte(`{"error":{"message":"API returned 400: {\"error\":{\"type\":\"invalid_request_error\",\"message\":\"max_tokens is too large\"}}"}}`),
		},
		{
			name:       "content_policy",
			statusCode: http.StatusForbidden,
			message:    "request was blocked by safety policy",
			body:       []byte(`{"error":{"type":"permission_error","message":"prompt was rejected by content policy"}}`),
		},
		{
			name:       "invalid_model",
			statusCode: http.StatusServiceUnavailable,
			message:    "model is not available",
			body:       []byte(`{"error":{"message":"model is not available for model claude-bad"}}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := classifyAnthropicPoolFailover(account, tt.statusCode, tt.message, tt.body, "claude-sonnet-4-6")
			require.True(t, decision.SkipSoftCooldown)
			require.False(t, decision.RetryableOnSame)
		})
	}
}

func TestClassifyAnthropicPoolFailover_UserErrorWithDescriptionSkipsSoftCooldown(t *testing.T) {
	account := &Account{
		ID:          311,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(400)}},
	}
	body := []byte(`{"error":{"type":"invalid_request_error","message":"invalid request: tool description is invalid"}}`)

	decision := classifyAnthropicPoolFailover(account, http.StatusBadRequest, "invalid request: tool description is invalid", body, "claude-sonnet-4-6")

	require.True(t, decision.SkipSoftCooldown)
	require.False(t, decision.RetryableOnSame)
}

func TestClassifyAnthropicPoolFailover_AccountAndUpstreamErrorsKeepSoftCooldown(t *testing.T) {
	account := &Account{
		ID:          307,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_soft_cooldown_error_threshold": 1},
	}
	tests := []struct {
		name       string
		statusCode int
		message    string
		body       []byte
	}{
		{
			name:       "auth",
			statusCode: http.StatusUnauthorized,
			message:    "invalid api key",
			body:       []byte(`{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`),
		},
		{
			name:       "rate_limit",
			statusCode: http.StatusTooManyRequests,
			message:    "rate limit exceeded",
			body:       []byte(`{"error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`),
		},
		{
			name:       "payment_required_402",
			statusCode: http.StatusPaymentRequired,
			message:    "credit balance is too low",
			body:       []byte(`{"error":{"type":"permission_error","message":"Your credit balance is too low to access the Anthropic API"}}`),
		},
		{
			name:       "credit_balance_400",
			statusCode: http.StatusBadRequest,
			message:    "Your credit balance is too low to access the Anthropic API",
			body:       []byte(`{"error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API"}}`),
		},
		{
			name:       "overloaded_529",
			statusCode: 529,
			message:    "overloaded",
			body:       []byte(`{"error":{"type":"overloaded_error","message":"temporarily overloaded"}}`),
		},
		{
			name:       "billing",
			statusCode: http.StatusForbidden,
			message:    "credit balance exhausted",
			body:       []byte(`{"error":{"type":"permission_error","message":"credit balance exhausted"}}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := classifyAnthropicPoolFailover(account, tt.statusCode, tt.message, tt.body, "claude-sonnet-4-6")
			require.True(t, decision.Failover)
			require.False(t, decision.SkipSoftCooldown)
		})
	}
}

func TestGatewayUpstreamFailoverError_AnthropicDownstreamErrorSkipsSoftCooldownAndRetry(t *testing.T) {
	account := &Account{
		ID:          308,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(503)}},
	}
	body := []byte(`{"error":{"message":"No available accounts: no available accounts","type":"api_error"}}`)

	failoverErr := newGatewayUpstreamFailoverError(account, http.StatusServiceUnavailable, body, "claude-sonnet-4-6")

	require.True(t, failoverErr.SkipPoolSoftCooldown)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.Equal(t, "claude-sonnet-4-6", failoverErr.ProbeModel)
}

func TestAnthropicPoolRequestErrorSoftCooldown_SkipsClientCancellation(t *testing.T) {
	require.False(t, shouldAnthropicPoolRequestErrorSoftCooldown(context.Canceled, "context canceled"))
	require.False(t, shouldAnthropicPoolRequestErrorSoftCooldown(errors.New("request canceled by client"), "request canceled by client"))
	require.True(t, shouldAnthropicPoolRequestErrorSoftCooldown(errors.New("tls handshake timeout"), "tls handshake timeout"))
}

func TestHandleFailoverSideEffects_AnthropicUserErrorDoesNotSoftCooldown(t *testing.T) {
	account := &Account{
		ID:          309,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(503)}},
	}
	body := `{"error":{"message":"API returned 400: invalid_request_error: max_tokens is too large","type":"api_error"}}`
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	svc := &GatewayService{rateLimitService: &RateLimitService{}}

	svc.handleFailoverSideEffects(context.Background(), resp, account, "claude-sonnet-4-6")

	state := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.False(t, state.Cooling)
}

func TestHandleFailoverSideEffects_AnthropicAccountErrorKeepsSoftCooldown(t *testing.T) {
	account := &Account{
		ID:          310,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_soft_cooldown_error_threshold": 1},
	}
	body := `{"error":{"message":"temporarily overloaded","type":"overloaded_error"}}`
	resp := &http.Response{
		StatusCode: 529,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	svc := &GatewayService{rateLimitService: &RateLimitService{}}

	svc.handleFailoverSideEffects(context.Background(), resp, account, "claude-sonnet-4-6")

	state := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.Equal(t, 529, state.StatusCode)
}

func TestRateLimitService_AnthropicPoolBalanceErrorDoesNotPauseScheduling(t *testing.T) {
	account := &Account{
		ID:       311,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                  true,
			"custom_error_codes_enabled": true,
			"custom_error_codes":         []any{float64(http.StatusBadRequest)},
		},
	}
	body := []byte(`{"error":{"message":"Your credit balance is too low to access the Anthropic API","type":"invalid_request_error"}}`)
	svc := &RateLimitService{}

	require.False(t, svc.HandleUpstreamError(context.Background(), account, http.StatusBadRequest, http.Header{}, body))
	require.Equal(t, ErrorPolicySkipped, svc.CheckErrorPolicy(context.Background(), account, http.StatusBadRequest, body))
}

func TestHandleRetryExhaustedSideEffects_AnthropicUserErrorDoesNotSoftCooldown(t *testing.T) {
	account := &Account{
		ID:          312,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(503)}},
	}
	body := `{"error":{"message":"API returned 400: invalid_request_error: max_tokens is too large","type":"api_error"}}`
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	svc := &GatewayService{rateLimitService: &RateLimitService{}}

	svc.handleRetryExhaustedSideEffects(context.Background(), resp, account, "claude-sonnet-4-6")

	state := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.False(t, state.Cooling)
}

func TestHandleRetryExhaustedSideEffects_AnthropicAccountErrorSoftCooldowns(t *testing.T) {
	account := &Account{
		ID:          313,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_soft_cooldown_error_threshold": 1},
	}
	body := `{"error":{"message":"temporarily overloaded","type":"overloaded_error"}}`
	resp := &http.Response{
		StatusCode: 529,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	svc := &GatewayService{rateLimitService: &RateLimitService{}}

	svc.handleRetryExhaustedSideEffects(context.Background(), resp, account, "claude-sonnet-4-6")

	state := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.Equal(t, 529, state.StatusCode)
	require.Equal(t, "retry_exhausted", state.CooldownSource)
}

func TestGatewayService_ShouldFailoverUpstreamError_IncludesPaymentRequired(t *testing.T) {
	svc := &GatewayService{}
	require.True(t, svc.shouldFailoverUpstreamError(http.StatusPaymentRequired))
}

func TestHandleFailoverSideEffects_AnthropicPoolPaymentRequiredSoftCooldownsWithoutRateLimitService(t *testing.T) {
	account := &Account{
		ID:          313,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_soft_cooldown_error_threshold": 1},
	}
	body := `{"error":{"type":"permission_error","message":"Your credit balance is too low to access the Anthropic API"}}`
	resp := &http.Response{
		StatusCode: http.StatusPaymentRequired,
		Header:     http.Header{"x-request-id": []string{"req_credit_402"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	svc := &GatewayService{}

	require.NotPanics(t, func() {
		svc.handleFailoverSideEffects(context.Background(), resp, account, "claude-sonnet-4-6")
	})

	state := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.Equal(t, http.StatusPaymentRequired, state.StatusCode)
	require.Equal(t, "upstream_failure", state.CooldownSource)
}

func TestMaybeAnthropicPoolClientErrorFailover_CreditBalance400(t *testing.T) {
	account := &Account{
		ID:          314,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_soft_cooldown_error_threshold": 1},
	}
	body := `{"error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API"}}`
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"x-request-id": []string{"req_credit_400"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	svc := &GatewayService{rateLimitService: &RateLimitService{}}

	failoverErr, ok := svc.maybeAnthropicPoolClientErrorFailover(context.Background(), resp, nil, account, "claude-sonnet-4-6", "[test]", false)

	require.True(t, ok)
	require.NotNil(t, failoverErr)
	require.Equal(t, http.StatusBadRequest, failoverErr.StatusCode)
	require.False(t, failoverErr.SkipPoolSoftCooldown)
	state := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.True(t, state.Cooling)
	require.Equal(t, http.StatusBadRequest, state.StatusCode)
}

func TestMaybeAnthropicPoolClientErrorFailover_UserBadRequestDoesNotFailover(t *testing.T) {
	account := &Account{
		ID:          315,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := `{"error":{"type":"invalid_request_error","message":"max_tokens is too large"}}`
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	svc := &GatewayService{rateLimitService: &RateLimitService{}}

	failoverErr, ok := svc.maybeAnthropicPoolClientErrorFailover(context.Background(), resp, nil, account, "claude-sonnet-4-6", "[test]", false)

	require.False(t, ok)
	require.Nil(t, failoverErr)
	state := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.False(t, state.Cooling)
}

func TestAnthropicPoolFailoverSwitch_DownstreamRoutingErrorSkipsSoftCooldown(t *testing.T) {
	svc := &GatewayService{}
	account := &Account{
		ID:          303,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	failoverErr := &UpstreamFailoverError{
		StatusCode:   http.StatusServiceUnavailable,
		Message:      "No available accounts: no available accounts",
		ResponseBody: []byte(`{"error":{"message":"No available accounts: no available accounts","type":"api_error"}}`),
	}

	svc.HandleAnthropicAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr, "claude-sonnet-4-6")

	state := svc.AnthropicPoolSoftCooldownState(account.ID)
	require.False(t, state.Cooling)
}

func TestAnthropicPoolFailoverSwitch_DefaultSoftCooldownThresholdRequiresThreeErrors(t *testing.T) {
	svc := &GatewayService{}
	account := &Account{
		ID:          316,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	failoverErr := &UpstreamFailoverError{
		StatusCode:   http.StatusInternalServerError,
		Message:      "server error",
		ResponseBody: []byte(`{"error":{"message":"server error"}}`),
	}

	for i := 0; i < 2; i++ {
		svc.HandleAnthropicAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr, "claude-sonnet-4-6")
		require.False(t, svc.AnthropicPoolSoftCooldownState(account.ID).Cooling)
	}

	svc.HandleAnthropicAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr, "claude-sonnet-4-6")
	require.True(t, svc.AnthropicPoolSoftCooldownState(account.ID).Cooling)
}

func TestAnthropicPoolFailoverSwitch_SuccessResetsSoftCooldownFailureThreshold(t *testing.T) {
	svc := &GatewayService{}
	account := &Account{
		ID:          317,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_soft_cooldown_error_threshold": 3},
	}
	failoverErr := &UpstreamFailoverError{
		StatusCode:   http.StatusInternalServerError,
		Message:      "server error",
		ResponseBody: []byte(`{"error":{"message":"server error"}}`),
	}

	svc.HandleAnthropicAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr, "claude-sonnet-4-6")
	svc.HandleAnthropicAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr, "claude-sonnet-4-6")
	require.False(t, svc.AnthropicPoolSoftCooldownState(account.ID).Cooling)

	svc.ReportAccountScheduleResult(account, true, nil)

	svc.HandleAnthropicAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr, "claude-sonnet-4-6")
	svc.HandleAnthropicAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr, "claude-sonnet-4-6")
	require.False(t, svc.AnthropicPoolSoftCooldownState(account.ID).Cooling)
	svc.HandleAnthropicAccountFailoverSwitch(context.Background(), nil, "", account, failoverErr, "claude-sonnet-4-6")
	require.True(t, svc.AnthropicPoolSoftCooldownState(account.ID).Cooling)
}

type accountUpdateRuntimeBlockRepo struct {
	AccountRepository
	account     *Account
	updateCalls int
}

func (r *accountUpdateRuntimeBlockRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	if r.account == nil {
		return nil, nil
	}
	copied := *r.account
	return &copied, nil
}

func (r *accountUpdateRuntimeBlockRepo) Update(ctx context.Context, account *Account) error {
	r.updateCalls++
	copied := *account
	r.account = &copied
	return nil
}

func TestAdminServiceUpdateAccount_CredentialChangeClearsRuntimeBlock(t *testing.T) {
	repo := &accountUpdateRuntimeBlockRepo{
		account: &Account{
			ID:          304,
			Platform:    PlatformAnthropic,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Credentials: map[string]any{"pool_mode": true},
		},
	}
	blocker := &openAIPoolManualRuntimeBlockRecorder{}
	svc := &adminServiceImpl{accountRepo: repo, runtimeBlocker: blocker}

	updated, err := svc.UpdateAccount(context.Background(), 304, &UpdateAccountInput{
		Credentials: map[string]any{"pool_mode": false},
	})

	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, 1, repo.updateCalls)
	require.Equal(t, []int64{304}, blocker.clearedIDs)
}

func TestAdminServiceRuntimeState_UsesCompositeAnthropicPoolBlocker(t *testing.T) {
	account := &Account{
		ID:          305,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"pool_mode": true},
	}
	repo := &accountUpdateRuntimeBlockRepo{account: account}
	anthropicGateway := &GatewayService{}
	anthropicGateway.MarkAnthropicPoolAccountSoftCooldown(context.Background(), account, http.StatusServiceUnavailable, nil, anthropicPoolSoftCooldownContext{
		ProbeModel: "claude-sonnet-4-6",
		ProbeKind:  "messages",
		StatusCode: http.StatusServiceUnavailable,
		Reason:     "upstream overloaded",
	})
	blocker := NewCompositeAccountRuntimeBlocker(nil, anthropicGateway, nil, nil)
	svc := &adminServiceImpl{accountRepo: repo}
	SetAdminServiceRuntimeBlocker(svc, blocker)

	got, err := svc.GetAccount(context.Background(), account.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AnthropicPoolSoftCooldownUntil)
	require.Equal(t, http.StatusServiceUnavailable, got.AnthropicPoolSoftCooldownStatusCode)

	require.NoError(t, svc.clearAccountRuntimeSchedulingBlock(context.Background(), account.ID))
	got, err = svc.GetAccount(context.Background(), account.ID)
	require.NoError(t, err)
	require.Nil(t, got.AnthropicPoolSoftCooldownUntil)
}
