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
