package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAntigravityTokenCacheKeyV025AccountIsolation(t *testing.T) {
	a := &Account{ID: 101, Credentials: map[string]any{"project_id": "shared-project"}}
	b := &Account{ID: 202, Credentials: map[string]any{"project_id": "shared-project"}}
	require.Equal(t, "ag:account:101", AntigravityTokenCacheKey(a))
	require.Equal(t, "ag:account:202", AntigravityTokenCacheKey(b))
	a.Credentials["project_id"] = "changed-project"
	require.Equal(t, "ag:account:101", AntigravityTokenCacheKey(a))
}
