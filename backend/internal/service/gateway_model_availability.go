package service

import (
	"context"
	"strings"
)

// ModelAvailabilityDiagnosis reports whether a requested model is configured
// for at least one account in a group, ignoring transient runtime state such as
// rate limits, quota auto-pauses, and temp-unschedulable markers.
type ModelAvailabilityDiagnosis struct {
	HasAccountsInPool bool
	HasModelSupport   bool
}

type ModelAvailabilityDiagnoser interface {
	DiagnoseModelAvailabilityForPlatform(
		ctx context.Context,
		groupID *int64,
		requestedModel string,
		platform string,
	) ModelAvailabilityDiagnosis
}

func (s *GatewayService) DiagnoseModelAvailabilityForPlatform(
	ctx context.Context,
	groupID *int64,
	requestedModel string,
	platform string,
) ModelAvailabilityDiagnosis {
	if s == nil {
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" || strings.TrimSpace(platform) == "" {
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}

	accounts, _, err := s.listSchedulableAccounts(ctx, groupID, platform, false)
	if err != nil {
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}

	diag := ModelAvailabilityDiagnosis{}
	for i := range accounts {
		diag.HasAccountsInPool = true
		if s.isModelSupportedByAccountWithContext(ctx, &accounts[i], requestedModel) {
			diag.HasModelSupport = true
			return diag
		}
	}
	return diag
}
