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

func TestAccountUpstreamBillingGuardUsesAccountGroupOverrideBeforeDefault(t *testing.T) {
	groupID := int64(10)
	groupLimit := 3.0
	override := 1.5
	observed := 2.0
	account := &Account{
		Platform:                               PlatformOpenAI,
		Type:                                   AccountTypeAPIKey,
		UpstreamBillingGuardEnabled:            true,
		Extra:                                  map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		UpstreamBillingGuardObservedMultiplier: &observed,
		AccountGroups: []AccountGroup{{
			GroupID: groupID,
			UpstreamBillingGuardOverrideMaxMultiplier: &override,
			GroupUpstreamBillingGuardMaxMultiplier:    &groupLimit,
			GroupPolicyLoaded:                         true,
		}},
	}

	limit, configured := account.UpstreamBillingGuardLimitForGroup(&groupID)
	require.True(t, configured)
	require.Equal(t, override, *limit)
	require.True(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID))

	account.AccountGroups[0].UpstreamBillingGuardOverrideMaxMultiplier = nil
	limit, configured = account.UpstreamBillingGuardLimitForGroup(&groupID)
	require.True(t, configured)
	require.Equal(t, groupLimit, *limit)
	require.False(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID))
}

func TestAccountUpstreamBillingGuardOverrideCannotRelaxGroupCeiling(t *testing.T) {
	groupID := int64(10)
	groupLimit := 2.0
	staleOverride := 5.0
	binding := AccountGroup{
		GroupID: groupID,
		UpstreamBillingGuardOverrideMaxMultiplier: &staleOverride,
		GroupUpstreamBillingGuardMaxMultiplier:    &groupLimit,
		GroupPolicyLoaded:                         true,
	}

	limit, configured := binding.EffectiveUpstreamBillingGuardMaxMultiplier()
	require.True(t, configured)
	require.Equal(t, groupLimit, *limit)
}

func TestAccountUpstreamBillingGuardOverrideSurvivesGroupLimitRoundTrip(t *testing.T) {
	groupID := int64(10)
	groupLimit := 2.0
	override := 1.5
	binding := AccountGroup{
		GroupID: groupID,
		UpstreamBillingGuardOverrideMaxMultiplier: &override,
		GroupUpstreamBillingGuardMaxMultiplier:    &groupLimit,
		GroupPolicyLoaded:                         true,
	}

	limit, configured := binding.EffectiveUpstreamBillingGuardMaxMultiplier()
	require.True(t, configured)
	require.Equal(t, 1.5, *limit)

	groupLimit = 1
	limit, configured = binding.EffectiveUpstreamBillingGuardMaxMultiplier()
	require.True(t, configured)
	require.Equal(t, 1.0, *limit)

	groupLimit = 2
	limit, configured = binding.EffectiveUpstreamBillingGuardMaxMultiplier()
	require.True(t, configured)
	require.Equal(t, 1.5, *limit)
	require.Equal(t, 1.5, *binding.UpstreamBillingGuardOverrideMaxMultiplier)
}

func TestAccountUpstreamBillingGuardLoadedNilGroupPolicyDisablesStaleOverride(t *testing.T) {
	staleEffective := 1.0
	staleOverride := 0.5
	binding := AccountGroup{
		UpstreamBillingGuardMaxMultiplier:         &staleEffective,
		UpstreamBillingGuardOverrideMaxMultiplier: &staleOverride,
		GroupPolicyLoaded:                         true,
	}

	limit, configured := binding.EffectiveUpstreamBillingGuardMaxMultiplier()
	require.False(t, configured)
	require.Nil(t, limit)

	binding.GroupPolicyLoaded = false
	limit, configured = binding.EffectiveUpstreamBillingGuardMaxMultiplier()
	require.True(t, configured, "old scheduler metadata must remain compatible during a rolling upgrade")
	require.Equal(t, staleEffective, *limit)
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

func TestAccountUpstreamBillingGuardLowerBoundAndEquality(t *testing.T) {
	groupID := int64(10)
	min, max := 0.8, 1.2
	observed := 0.79
	account := &Account{
		Platform: PlatformDeepSeek, Type: AccountTypeAPIKey,
		UpstreamBillingGuardEnabled:            true,
		Extra:                                  map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		UpstreamBillingGuardObservedMultiplier: &observed,
		AccountGroups: []AccountGroup{{
			GroupID: groupID, GroupPolicyLoaded: true,
			GroupUpstreamBillingGuardMinMultiplier: &min,
			GroupUpstreamBillingGuardMaxMultiplier: &max,
		}},
	}
	require.True(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID))
	observed = min
	require.False(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID), "equality at minimum is allowed")
	observed = max
	require.False(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID), "equality at maximum is allowed")
	observed = 1.21
	require.True(t, account.IsUpstreamBillingGuardBlockedForGroup(&groupID))
}

func TestAccountUpstreamBillingGuardMinimumOverrideOnlyTightens(t *testing.T) {
	groupMin, groupMax := 0.5, 2.0
	overrideMin, staleRelaxingMin := 0.8, 0.2
	binding := AccountGroup{
		GroupPolicyLoaded:                         true,
		GroupUpstreamBillingGuardMinMultiplier:    &groupMin,
		GroupUpstreamBillingGuardMaxMultiplier:    &groupMax,
		UpstreamBillingGuardOverrideMinMultiplier: &overrideMin,
	}
	min, max, configured := binding.EffectiveUpstreamBillingGuardBounds()
	require.True(t, configured)
	require.Equal(t, overrideMin, *min)
	require.Equal(t, groupMax, *max)

	binding.UpstreamBillingGuardOverrideMinMultiplier = &staleRelaxingMin
	min, _, configured = binding.EffectiveUpstreamBillingGuardBounds()
	require.True(t, configured)
	require.Equal(t, groupMin, *min, "a stale override cannot relax the group lower bound")
}

func TestAccountUpstreamBillingGuardMinimumOnlyAndOldSnapshotCompatibility(t *testing.T) {
	min := 1.0
	binding := AccountGroup{GroupPolicyLoaded: true, GroupUpstreamBillingGuardMinMultiplier: &min}
	effectiveMin, effectiveMax, configured := binding.EffectiveUpstreamBillingGuardBounds()
	require.True(t, configured)
	require.Equal(t, min, *effectiveMin)
	require.Nil(t, effectiveMax)

	legacyMax := 1.5
	binding = AccountGroup{UpstreamBillingGuardMaxMultiplier: &legacyMax}
	effectiveMin, effectiveMax, configured = binding.EffectiveUpstreamBillingGuardBounds()
	require.True(t, configured)
	require.Nil(t, effectiveMin)
	require.Equal(t, legacyMax, *effectiveMax)
}

func TestAccountUpstreamBillingGuardCrossedEffectiveBoundsFailClosed(t *testing.T) {
	groupMin, groupMax, overrideMax := 0.5, 2.0, 0.4
	binding := AccountGroup{
		GroupID:                                   1,
		GroupPolicyLoaded:                         true,
		GroupUpstreamBillingGuardMinMultiplier:    &groupMin,
		GroupUpstreamBillingGuardMaxMultiplier:    &groupMax,
		UpstreamBillingGuardOverrideMaxMultiplier: &overrideMax,
	}
	min, max, configured := binding.EffectiveUpstreamBillingGuardBounds()
	require.True(t, configured)
	require.Equal(t, groupMin, *min)
	require.Equal(t, overrideMax, *max)

	account := &Account{
		Platform:                    PlatformOpenAI,
		Type:                        AccountTypeAPIKey,
		UpstreamBillingGuardEnabled: true,
		Extra:                       map[string]any{UpstreamBillingProbeEnabledExtraKey: true},
		AccountGroups:               []AccountGroup{binding},
	}
	require.True(t, account.IsUpstreamBillingGuardBlockedForGroup(func() *int64 { id := int64(1); return &id }()), "crossed bounds must block before the first probe")

	overrideMax = groupMin
	require.True(t, account.IsUpstreamBillingGuardBlockedForGroup(func() *int64 { id := int64(1); return &id }()), "equal bounds must block before the first probe")

	observed := 0.45
	account.UpstreamBillingGuardObservedMultiplier = &observed
	require.True(t, account.IsUpstreamBillingGuardBlockedForGroup(func() *int64 { id := int64(1); return &id }()))
}
