package service

import "time"

type AccountGroup struct {
	AccountID int64
	GroupID   int64
	Priority  int
	// UpstreamBillingGuardMaxMultiplier is the effective limit retained for
	// compatibility with old API clients and rolling-upgrade scheduler nodes.
	UpstreamBillingGuardMaxMultiplier *float64
	// UpstreamBillingGuardOverrideMaxMultiplier is the raw account x group
	// override. Nil means inherit the OpenAI group's default limit.
	UpstreamBillingGuardOverrideMaxMultiplier *float64
	// GroupUpstreamBillingGuardMaxMultiplier and GroupPolicyLoaded keep the
	// group default available after scheduler metadata strips the Group object.
	// The loaded bit distinguishes an explicit nil group policy from old cache
	// entries that did not carry the group default separately.
	GroupUpstreamBillingGuardMaxMultiplier *float64
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
		if ag.Group.Platform != PlatformOpenAI {
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
