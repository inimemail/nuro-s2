//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAI429FastPath_MarksOAuthAccountCoolingDown(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKeyAccount := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	shouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, nil)
	apiKeyShouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), apiKeyAccount, http.StatusTooManyRequests, http.Header{}, nil)

	require.False(t, shouldDisable)
	require.False(t, apiKeyShouldDisable)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(apiKeyAccount))
}

func TestOpenAIRuntimeBlock_AppliesToOpenAIAPIKeyWhenRateLimitServiceStopsScheduling(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 44, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	svc.BlockAccountScheduling(account, time.Time{}, "custom_error_code")

	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIRuntimeBlock_DoesNotApplyToOtherPlatforms(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 45, Platform: PlatformGemini, Type: AccountTypeOAuth}

	svc.BlockAccountScheduling(account, time.Time{}, "custom_error_code")

	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIRuntimeBlocker_IgnoresNonOpenAIFromRateLimitService(t *testing.T) {
	gateway := &OpenAIGatewayService{}
	repo := &rateLimitAccountRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	rateLimitService.SetAccountRuntimeBlocker(gateway)
	account := &Account{ID: 45, Platform: PlatformGemini, Type: AccountTypeOAuth}

	shouldDisable := rateLimitService.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{}, []byte("forbidden"))

	require.True(t, shouldDisable)
	require.False(t, gateway.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIModelNotFound_DoesNotRuntimeBlockWholeAccount(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := openAIModelNotFoundTempAccount()

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"code":"model_not_found","message":"model not found"}}`),
		"gpt-5.4",
	)

	require.True(t, shouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Zero(t, repo.tempCalls)
	require.Len(t, repo.modelRateLimitCalls, 1)
}

func TestOpenAIRuntimeBlock_DoesNotShortenExistingBlock(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 46, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	longUntil := time.Now().Add(10 * time.Minute)

	svc.BlockAccountScheduling(account, longUntil, "oauth_401")
	svc.BlockAccountScheduling(account, time.Time{}, "upstream_disable")

	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	actualUntil, ok := value.(time.Time)
	require.True(t, ok)
	require.WithinDuration(t, longUntil, actualUntil, time.Second)
}

func TestOpenAIRuntimeBlock_ClearAccountSchedulingBlock(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 47, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))

	svc.ClearAccountSchedulingBlock(account.ID)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIPoolSoftCooldown_529UsesOverloadCooldownSettings(t *testing.T) {
	settingRepo := newMockSettingRepo()
	data, _ := json.Marshal(OverloadCooldownSettings{Enabled: true, CooldownSeconds: 2})
	settingRepo.data[SettingKeyOverloadCooldownSettings] = string(data)
	settingSvc := NewSettingService(settingRepo, &config.Config{})
	rateLimitSvc := NewRateLimitService(nil, nil, &config.Config{}, nil, nil)
	rateLimitSvc.SetSettingService(settingSvc)
	svc := &OpenAIGatewayService{rateLimitService: rateLimitSvc}
	account := &Account{
		ID:          48,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}

	before := time.Now()
	svc.MarkOpenAIPoolAccountSoftCooldown(context.Background(), account, 529, nil)

	until, ok := svc.openAIPoolAccountSoftCooldownUntil(account)
	require.True(t, ok)
	require.WithinDuration(t, before.Add(2*time.Second), until, 2*time.Second)
}

func TestOpenAIPoolSoftCooldown_529DisabledSkipsSoftCooldown(t *testing.T) {
	settingRepo := newMockSettingRepo()
	data, _ := json.Marshal(OverloadCooldownSettings{Enabled: false, CooldownSeconds: 2})
	settingRepo.data[SettingKeyOverloadCooldownSettings] = string(data)
	settingSvc := NewSettingService(settingRepo, &config.Config{})
	rateLimitSvc := NewRateLimitService(nil, nil, &config.Config{}, nil, nil)
	rateLimitSvc.SetSettingService(settingSvc)
	svc := &OpenAIGatewayService{rateLimitService: rateLimitSvc}
	account := &Account{
		ID:          49,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}

	svc.MarkOpenAIPoolAccountSoftCooldown(context.Background(), account, 529, nil)

	require.False(t, svc.isOpenAIPoolAccountSoftCooling(account))
}

func TestOpenAIPoolSoftCooldown_ExpiredWaitsForRecoveryProbe(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          50,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}

	svc.storeOpenAIPoolSoftCooldownUntil(account.ID, time.Now().Add(-time.Second))

	require.True(t, svc.isOpenAIPoolAccountSoftCooling(account))
	require.True(t, svc.isOpenAIPoolAccountSoftCooldownDue(account))
	_, ok := svc.openAIPoolAccountSoftCooldownUntil(account)
	require.True(t, ok)
}

func TestOpenAIPoolRecoveryProbeStatusRetryable_OnlyAuthIsLongCooldown(t *testing.T) {
	require.False(t, openAIPoolRecoveryProbeStatusRetryable(http.StatusUnauthorized))
	require.False(t, openAIPoolRecoveryProbeStatusRetryable(http.StatusForbidden))
	require.True(t, openAIPoolRecoveryProbeStatusRetryable(http.StatusBadRequest))
	require.True(t, openAIPoolRecoveryProbeStatusRetryable(http.StatusNotFound))
	require.True(t, openAIPoolRecoveryProbeStatusRetryable(http.StatusTooManyRequests))
	require.True(t, openAIPoolRecoveryProbeStatusRetryable(http.StatusInternalServerError))
}

func TestShouldStopOpenAIOAuth429Failover_OnlyDuringStorm(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKeyAccount := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 1))

	for i := 0; i < openAIOAuth429StormThreshold; i++ {
		svc.recordOpenAIOAuth429()
	}

	require.True(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 1))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(apiKeyAccount, http.StatusTooManyRequests, 1))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusInternalServerError, 1))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 0))
}
