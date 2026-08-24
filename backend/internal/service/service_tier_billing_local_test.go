package service

import "testing"

import "github.com/stretchr/testify/require"

func TestResolveBillingServiceTierOnlyDowngrades(t *testing.T) {
	require.Equal(t, "default", ResolveBillingServiceTier("priority", "default"))
	require.Equal(t, "priority", ResolveBillingServiceTier("priority", "priority"))
	require.Equal(t, "default", ResolveBillingServiceTier("default", "priority"))
	require.Equal(t, "priority", ResolveBillingServiceTier("priority", "unknown"))
}
