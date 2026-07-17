package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountUpstreamBillingGuardIsScopedToBinding(t *testing.T) {
	observed := 2.0
	limitLow := 1.5
	limitEqual := 2.0
	limitHigh := 3.0
	account := &Account{
		Platform:                               PlatformOpenAI,
		Type:                                   AccountTypeAPIKey,
		Status:                                 StatusActive,
		Schedulable:                            true,
		UpstreamBillingGuardEnabled:            true,
		Extra:                                  map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		UpstreamBillingGuardObservedMultiplier: &observed,
		AccountGroups: []AccountGroup{
			{GroupID: 10, UpstreamBillingGuardMaxMultiplier: &limitLow},
			{GroupID: 20, UpstreamBillingGuardMaxMultiplier: &limitEqual},
			{GroupID: 30, UpstreamBillingGuardMaxMultiplier: &limitHigh},
			{GroupID: 40},
		},
	}

	group10, group20, group30, group40 := int64(10), int64(20), int64(30), int64(40)
	require.True(t, account.IsUpstreamBillingGuardBlockedForGroup(&group10))
	require.False(t, account.IsUpstreamBillingGuardBlockedForGroup(&group20), "equal to the limit must recover")
	require.False(t, account.IsUpstreamBillingGuardBlockedForGroup(&group30))
	require.False(t, account.IsUpstreamBillingGuardBlockedForGroup(&group40), "blank limit means unrestricted")

	observed = 1.5
	require.False(t, account.IsUpstreamBillingGuardBlockedForGroup(&group10), "a lower successful probe must restore scheduling")
}

func TestAccountUpstreamBillingGuardRequiresAutoProbeOnlyWhenConfigured(t *testing.T) {
	limit := 1.0
	groupID := int64(10)
	account := &Account{
		Platform:                    PlatformOpenAI,
		Type:                        AccountTypeAPIKey,
		UpstreamBillingGuardEnabled: true,
		AccountGroups:               []AccountGroup{{GroupID: groupID, UpstreamBillingGuardMaxMultiplier: &limit}},
	}

	require.True(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID))
	account.Extra = map[string]any{UpstreamBillingProbeEnabledExtraKey: "true"}
	require.True(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID), "a malformed string flag must not enable probing")
	account.Extra = map[string]any{UpstreamBillingProbeEnabledExtraKey: true}
	require.False(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID), "first successful probe is pending")
	account.AccountGroups[0].UpstreamBillingGuardMaxMultiplier = nil
	require.False(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID))
}

func TestAccountUpstreamBillingGuardMasterSwitchIsNoOpWhenDisabled(t *testing.T) {
	observed := 3.0
	limit := 1.0
	groupID := int64(10)
	account := &Account{
		Platform:                               PlatformOpenAI,
		Type:                                   AccountTypeAPIKey,
		Extra:                                  map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		UpstreamBillingGuardObservedMultiplier: &observed,
		AccountGroups:                          []AccountGroup{{GroupID: groupID, UpstreamBillingGuardMaxMultiplier: &limit}},
	}

	require.False(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID))
	account.UpstreamBillingGuardEnabled = true
	require.True(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID))
}

func TestAccountUpstreamBillingGuardPrefersHydratedGroupPolicyOverStaleBinding(t *testing.T) {
	groupID := int64(10)
	staleBindingLimit := 1.0
	groupLimit := 2.0
	account := &Account{
		Platform:                    PlatformOpenAI,
		Type:                        AccountTypeAPIKey,
		UpstreamBillingGuardEnabled: true,
		Extra:                       map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		UpstreamBillingGuardObservedMultiplier: func() *float64 {
			value := 1.5
			return &value
		}(),
		AccountGroups: []AccountGroup{{
			GroupID:                           groupID,
			UpstreamBillingGuardMaxMultiplier: &staleBindingLimit,
			Group:                             &Group{ID: groupID, Platform: PlatformOpenAI, UpstreamBillingGuardMaxMultiplier: &groupLimit},
		}},
	}

	require.False(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID))
	account.AccountGroups[0].Group.UpstreamBillingGuardMaxMultiplier = nil
	require.False(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID), "explicit group nil must mean unrestricted")
}

func TestAccountUpstreamBillingGuardIgnoresStaleBindingForHydratedNonOpenAIGroup(t *testing.T) {
	groupID := int64(20)
	staleBindingLimit := 1.0
	account := &Account{
		Platform:                    PlatformOpenAI,
		Type:                        AccountTypeAPIKey,
		UpstreamBillingGuardEnabled: true,
		Extra:                       map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		UpstreamBillingGuardObservedMultiplier: func() *float64 {
			value := 3.0
			return &value
		}(),
		AccountGroups: []AccountGroup{{
			GroupID:                           groupID,
			UpstreamBillingGuardMaxMultiplier: &staleBindingLimit,
			Group:                             &Group{ID: groupID, Platform: PlatformAnthropic, UpstreamBillingGuardMaxMultiplier: &staleBindingLimit},
		}},
	}

	require.False(t, account.HasUpstreamBillingGuardGroupLimit())
	require.False(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID))
}

func TestAccountIsSchedulableUsesOnlyRuntimeGroupDecision(t *testing.T) {
	account := &Account{
		Status: StatusActive, Schedulable: true,
		UpstreamBillingGuardBlocked: true,
	}
	require.True(t, account.IsSchedulable(), "legacy account-global guard must not disable every group")
	account.UpstreamBillingGuardGroupBlocked = true
	require.False(t, account.IsSchedulable())
}
