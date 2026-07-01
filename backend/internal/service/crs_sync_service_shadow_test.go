package service

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGuardCRSShadowParentInvariantRejectsShadowEvenForOpenAIOAuth(t *testing.T) {
	parentID := int64(1)
	shadow := &Account{
		ID:              2,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
	}

	err := guardCRSShadowParentInvariant(context.Background(), nil, shadow, PlatformOpenAI, AccountTypeOAuth)

	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "spark-shadow")
}

func TestCRSSyncServiceRefreshOAuthTokenSkipsShadow(t *testing.T) {
	parentID := int64(1)
	svc := &CRSSyncService{}
	shadow := &Account{
		ID:              2,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
		Credentials: map[string]any{
			"access_token": "shadow-token",
		},
	}

	got := svc.refreshOAuthToken(context.Background(), shadow)

	require.Nil(t, got)
	require.Equal(t, "shadow-token", shadow.GetOpenAIAccessToken())
}
