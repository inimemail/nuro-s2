//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestV027GeminiVariantGatewaySelectionAndRequiredAccount(t *testing.T) {
	for _, batch := range []bool{false, true} {
		groupID := int64(5)
		group := &Group{ID: groupID, Platform: PlatformGemini, Status: StatusActive, Hydrated: true}
		originalCtx := context.WithValue(context.Background(), ctxkey.Group, group)
		ctx := WithGeminiThinkingVariantRequest(originalCtx, []byte(`{"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`))
		account := Account{ID: 1, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 2, Priority: 1, GroupIDs: []int64{groupID}, Extra: map[string]any{"mixed_scheduling": true}, Credentials: map[string]any{"model_mapping": map[string]any{"gemini-3.8-flash-low": "low-target"}}}
		repo := &mockAccountRepoForPlatform{accounts: []Account{account}, accountsByID: map[int64]*Account{1: &account}}
		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = batch
		svc := &GatewayService{accountRepo: repo, groupRepo: &mockGroupRepoForGateway{groups: map[int64]*Group{groupID: group}}, cache: &mockGatewayCacheForPlatform{}, cfg: cfg, concurrencyService: NewConcurrencyService(&mockConcurrencyCache{acquireResults: map[int64]bool{1: true}, loadMap: map[int64]*AccountLoadInfo{1: {AccountID: 1, LoadRate: 0}}})}
		_, err := svc.SelectAccountWithLoadAwareness(originalCtx, &groupID, "", "gemini-3.8-flash", nil, "", 0)
		require.Error(t, err, "non-native requests must retain the existing whitelist")
		selection, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "", "gemini-3.8-flash", nil, "", 0)
		require.NoError(t, err)
		require.Equal(t, int64(1), selection.Account.ID)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		selection, err = svc.SelectRequiredAccountWithLoadAwareness(ctx, &groupID, 1, "", "gemini-3.8-flash", nil)
		require.NoError(t, err)
		require.Equal(t, int64(1), selection.Account.ID)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		account.Extra["model_rate_limits"] = map[string]any{"low-target": map[string]any{"rate_limit_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)}}
		_, err = svc.SelectRequiredAccountWithLoadAwareness(ctx, &groupID, 1, "", "gemini-3.8-flash", nil)
		require.Error(t, err, "required-account retries must not bypass variant cooldown")
		delete(account.Extra, "model_rate_limits")
		for _, pricedModel := range []string{"low-target", "different-target"} {
			channel := Channel{ID: 1, Status: StatusActive, GroupIDs: []int64{groupID}, RestrictModels: true, BillingModelSource: BillingModelSourceUpstream, ModelPricing: []ChannelModelPricing{{Platform: PlatformGemini, Models: []string{pricedModel}}}}
			svc.channelService = newTestChannelService(makeStandardRepo(channel, map[int64]string{groupID: PlatformGemini}))
			selection, err = svc.SelectRequiredAccountWithLoadAwareness(ctx, &groupID, 1, "", "gemini-3.8-flash", nil)
			if pricedModel == "low-target" {
				require.NoError(t, err)
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			} else {
				require.Error(t, err, "upstream channel restrictions must use the actual variant target")
			}
		}
	}
}
