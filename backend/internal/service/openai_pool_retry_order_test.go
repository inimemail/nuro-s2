package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIPoolModeUpstreamError_DoesNotRuntimeBlockBeforeRetryExhausted(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          142,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "pool_soft_cooldown_enabled": true, "pool_mode_retry_count": 3},
	}

	shouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, nil)

	require.False(t, shouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	_, cooling := svc.openAIPoolAccountSoftCooldownUntil(account)
	require.False(t, cooling)
}
