package service

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func (s *OpenAIGatewayService) DiagnoseModelAvailabilityForPlatform(
	ctx context.Context,
	groupID *int64,
	requestedModel string,
	platform string,
) ModelAvailabilityDiagnosis {
	if s == nil {
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}
	if strings.TrimSpace(platform) == "" {
		platform = PlatformOpenAI
	}

	accounts, err := s.listSchedulableAccountsForPlatform(ctx, groupID, platform)
	if err != nil {
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}

	diag := ModelAvailabilityDiagnosis{}
	for i := range accounts {
		diag.HasAccountsInPool = true
		if accounts[i].IsModelSupported(requestedModel) {
			diag.HasModelSupport = true
			return diag
		}
	}
	return diag
}

func (s *OpenAIGatewayService) listSchedulableAccountsForPlatform(ctx context.Context, groupID *int64, platform string) ([]Account, error) {
	platform = strings.TrimSpace(platform)
	if platform == "" || platform == PlatformOpenAI {
		return s.listSchedulableAccounts(ctx, groupID)
	}
	if s.schedulerSnapshot != nil {
		accounts, _, err := s.schedulerSnapshot.ListSchedulableAccounts(ctx, groupID, platform, false)
		return accounts, err
	}
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return s.accountRepo.ListSchedulableByPlatform(ctx, platform)
	}
	if groupID != nil {
		return s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, platform)
	}
	return s.accountRepo.ListSchedulableUngroupedByPlatform(ctx, platform)
}
