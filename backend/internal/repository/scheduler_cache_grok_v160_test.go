package repository

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBuildSchedulerMetadataAccountKeepsGrokMediaEligibilityV160(t *testing.T) {
	account := service.Account{
		ID:       160,
		Platform: service.PlatformGrok,
		Type:     service.AccountTypeOAuth,
		Extra: map[string]any{
			service.GrokBillingSnapshotExtraKey: map[string]any{
				"weekly_status_code": http.StatusForbidden,
			},
			"drop_me": "large-runtime-data",
		},
	}
	got := buildSchedulerMetadataAccount(account)
	eligible, reason := got.GrokMediaGenerationEligibility()
	require.False(t, eligible)
	require.Equal(t, "billing_forbidden", reason)
	require.NotNil(t, got.Extra[service.GrokBillingSnapshotExtraKey])
	require.Nil(t, got.Extra["drop_me"])
}

func TestBuildSchedulerMetadataAccountKeepsGrokMediaOverrideV160(t *testing.T) {
	account := service.Account{
		ID:       161,
		Platform: service.PlatformGrok,
		Type:     service.AccountTypeOAuth,
		Extra: map[string]any{
			service.GrokMediaEligibleExtraKey: false,
		},
	}
	got := buildSchedulerMetadataAccount(account)
	eligible, reason := got.GrokMediaGenerationEligibility()
	require.False(t, eligible)
	require.Equal(t, "override_disabled", reason)
}
