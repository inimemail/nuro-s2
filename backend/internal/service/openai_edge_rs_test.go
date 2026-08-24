package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIEdgeRetryRequestEffectiveCommitState(t *testing.T) {
	require.Equal(t, OpenAIEdgeCommitStateNone, (OpenAIEdgeRetryRequest{}).EffectiveCommitState())
	require.Equal(t, OpenAIEdgeCommitStateRealOutput, (OpenAIEdgeRetryRequest{WroteClientResponse: true}).EffectiveCommitState())
	require.Equal(t, OpenAIEdgeCommitStateRealOutput, (OpenAIEdgeRetryRequest{
		CommitState:         OpenAIEdgeCommitStateNone,
		WroteClientResponse: true,
	}).EffectiveCommitState())
	require.Equal(t, OpenAIEdgeCommitStateGatewayOnly, (OpenAIEdgeRetryRequest{
		CommitState:         OpenAIEdgeCommitStateGatewayOnly,
		WroteClientResponse: true,
	}).EffectiveCommitState())
	require.Equal(t, OpenAIEdgeCommitStateTerminal, (OpenAIEdgeRetryRequest{
		CommitState: OpenAIEdgeCommitStateTerminal,
	}).EffectiveCommitState())
}

func TestClassifyOpenAIEdgeStreamFailureUsesGoStreamPolicy(t *testing.T) {
	service := &OpenAIGatewayService{}
	account := &Account{
		ID:       1001,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}

	retryablePayload := []byte(`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"busy"}}}`)
	failoverErr, shouldFailover := service.ClassifyOpenAIEdgeStreamFailure(
		context.Background(), account, retryablePayload, "busy",
	)
	require.True(t, shouldFailover)
	require.NotNil(t, failoverErr)
	require.NotZero(t, failoverErr.StatusCode)

	policyPayload := []byte(`{"type":"response.failed","response":{"error":{"type":"content_policy","message":"not allowed"}}}`)
	failoverErr, shouldFailover = service.ClassifyOpenAIEdgeStreamFailure(
		context.Background(), account, policyPayload, "not allowed",
	)
	require.False(t, shouldFailover)
	require.Nil(t, failoverErr)
}
