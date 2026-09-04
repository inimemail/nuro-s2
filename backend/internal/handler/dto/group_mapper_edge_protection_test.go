package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGroupEdgeProtectionOverrideIsAdminOnly(t *testing.T) {
	disabled := false
	group := &service.Group{
		ID:                    1,
		Platform:              service.PlatformOpenAI,
		EdgeProtectionEnabled: &disabled,
	}

	publicJSON, err := json.Marshal(GroupFromService(group))
	require.NoError(t, err)
	require.NotContains(t, string(publicJSON), "edge_protection_enabled")

	adminJSON, err := json.Marshal(GroupFromServiceAdmin(group))
	require.NoError(t, err)
	require.Contains(t, string(adminJSON), `"edge_protection_enabled":false`)
}

func TestGroupUpstreamBillingGuardBoundsAreAdminOnly(t *testing.T) {
	min, max := 0.8, 1.2
	group := &service.Group{
		ID:                                1,
		Platform:                          service.PlatformOpenAI,
		UpstreamBillingGuardMinMultiplier: &min,
		UpstreamBillingGuardMaxMultiplier: &max,
	}

	publicJSON, err := json.Marshal(GroupFromService(group))
	require.NoError(t, err)
	require.NotContains(t, string(publicJSON), "upstream_billing_guard_min_multiplier")
	require.NotContains(t, string(publicJSON), "upstream_billing_guard_max_multiplier")

	adminJSON, err := json.Marshal(GroupFromServiceAdmin(group))
	require.NoError(t, err)
	require.Contains(t, string(adminJSON), `"upstream_billing_guard_min_multiplier":0.8`)
	require.Contains(t, string(adminJSON), `"upstream_billing_guard_max_multiplier":1.2`)
}
