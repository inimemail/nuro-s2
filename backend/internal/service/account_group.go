package service

import "time"

type AccountGroup struct {
	AccountID int64
	GroupID   int64
	Priority  int
	// UpstreamBillingGuardMaxMultiplier is the effective limit retained for
	// compatibility with old API clients and rolling-upgrade scheduler nodes.
	UpstreamBillingGuardMaxMultiplier *float64
	// UpstreamBillingGuardMinMultiplier is the effective lower bound.
	UpstreamBillingGuardMinMultiplier *float64
	// UpstreamBillingGuardOverrideMaxMultiplier is the raw account x group
	// override. Nil means inherit the platform group's default limit.
	UpstreamBillingGuardOverrideMaxMultiplier *float64
	UpstreamBillingGuardOverrideMinMultiplier *float64
	// GroupUpstreamBillingGuardMaxMultiplier and GroupPolicyLoaded keep the
	// group default available after scheduler metadata strips the Group object.
	// The loaded bit distinguishes an explicit nil group policy from old cache
	// entries that did not carry the group default separately.
	GroupUpstreamBillingGuardMaxMultiplier *float64
	GroupUpstreamBillingGuardMinMultiplier *float64
	GroupPolicyLoaded                      bool
	CreatedAt                              time.Time

	Account *Account
	Group   *Group
}

// EffectiveUpstreamBillingGuardMaxMultiplier returns the account x group
// policy without allowing an override to relax the group-wide ceiling.
func (ag *AccountGroup) EffectiveUpstreamBillingGuardMaxMultiplier() (*float64, bool) {
	if ag == nil {
		return nil, false
	}

	var groupLimit *float64
	switch {
	case ag.Group != nil:
		if !IsUpstreamBillingProbeIdentity(ag.Group.Platform, AccountTypeAPIKey) {
			return nil, false
		}
		groupLimit = ag.Group.UpstreamBillingGuardMaxMultiplier
	case ag.GroupPolicyLoaded:
		groupLimit = ag.GroupUpstreamBillingGuardMaxMultiplier
	default:
		// Rolling-upgrade compatibility: older scheduler metadata stored the
		// effective group limit in this field and had no separate policy bit.
		if ag.UpstreamBillingGuardMaxMultiplier != nil {
			return ag.UpstreamBillingGuardMaxMultiplier, true
		}
		return nil, false
	}

	if groupLimit == nil {
		return nil, false
	}
	if override := ag.UpstreamBillingGuardOverrideMaxMultiplier; override != nil && *override <= *groupLimit {
		return override, true
	}
	return groupLimit, true
}

// EffectiveUpstreamBillingGuardBounds returns the account/group protection
// interval. Account overrides may only tighten the group interval.
func (ag *AccountGroup) EffectiveUpstreamBillingGuardBounds() (min, max *float64, configured bool) {
	if ag == nil {
		return nil, nil, false
	}
	var groupMin, groupMax *float64
	switch {
	case ag.Group != nil:
		if !IsUpstreamBillingProbeIdentity(ag.Group.Platform, AccountTypeAPIKey) {
			return nil, nil, false
		}
		groupMin, groupMax = ag.Group.UpstreamBillingGuardMinMultiplier, ag.Group.UpstreamBillingGuardMaxMultiplier
	case ag.GroupPolicyLoaded:
		groupMin, groupMax = ag.GroupUpstreamBillingGuardMinMultiplier, ag.GroupUpstreamBillingGuardMaxMultiplier
	default:
		// Old snapshots only carry the effective maximum. Preserve that behavior
		// and treat the new lower bound as unset during rolling upgrades.
		if ag.UpstreamBillingGuardMaxMultiplier != nil {
			groupMax = ag.UpstreamBillingGuardMaxMultiplier
		} else {
			return nil, nil, false
		}
	}
	if groupMin == nil && groupMax == nil {
		return nil, nil, false
	}
	if groupMin != nil {
		min = groupMin
		if override := ag.UpstreamBillingGuardOverrideMinMultiplier; override != nil && *override >= *groupMin {
			min = override
		}
	}
	if groupMax != nil {
		max = groupMax
		if override := ag.UpstreamBillingGuardOverrideMaxMultiplier; override != nil && *override <= *groupMax {
			max = override
		}
	}
	if min != nil && max != nil && *min >= *max {
		// A group default may be tightened after an older account override was
		// stored. Keep an invalid interval configured and fail closed until it is
		// corrected, rather than silently treating the binding as unrestricted.
		return min, max, true
	}
	return min, max, true
}
