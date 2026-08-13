package service

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNormalizeGroupModelPricingForcesOwningPlatform(t *testing.T) {
	price := 1.0
	got, err := normalizeGroupModelPricing(PlatformGrok, []ChannelModelPricing{{
		Platform: PlatformOpenAI, Models: []string{" GROK-4.6 "},
		BillingMode: BillingModeToken, InputPrice: &price,
	}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, PlatformGrok, got[0].Platform)
	require.Equal(t, []string{"grok-4.6"}, got[0].Models)
}

func TestGroupHasImageGenerationCapabilityRequiresResolvedCompositeProvider(t *testing.T) {
	group := &Group{Platform: PlatformComposite, AllowImageGeneration: true}
	require.False(t, GroupHasImageGenerationCapability(context.Background(), group))
	require.True(t, GroupHasImageGenerationCapability(WithResolvedTargetPlatform(context.Background(), PlatformOpenAI), group))
	require.True(t, GroupHasImageGenerationCapability(WithResolvedTargetPlatform(context.Background(), PlatformGrok), group))
	require.False(t, GroupHasImageGenerationCapability(WithResolvedTargetPlatform(context.Background(), PlatformAnthropic), group))

	group.AllowImageGeneration = false
	require.False(t, GroupHasImageGenerationCapability(WithResolvedTargetPlatform(context.Background(), PlatformOpenAI), group))
}

func TestResolveGroupPricingExactBeforeWildcard(t *testing.T) {
	exact := 2.0
	wildcard := 1.0
	group := &Group{LongContextPricingEnabled: true, ModelPricing: []ChannelModelPricing{
		{Platform: PlatformGrok, Models: []string{"grok-*"}, BillingMode: BillingModeToken, InputPrice: &wildcard},
		{Platform: PlatformGrok, Models: []string{"grok-4.6"}, BillingMode: BillingModeToken, InputPrice: &exact},
	}}
	resolver := NewModelPricingResolver(nil, NewBillingService(nil, nil))
	resolved := resolver.Resolve(context.Background(), PricingInput{Model: "grok-4.6", Group: group})
	require.Equal(t, PricingSourceGroup, resolved.Source)
	require.NotNil(t, resolved.BasePricing)
	require.Equal(t, exact, resolved.BasePricing.InputPricePerToken)
}

func TestValidateGroupVideoPricingRequiresPrice(t *testing.T) {
	_, err := normalizeGroupModelPricing(PlatformGrok, []ChannelModelPricing{{
		Models: []string{"grok-imagine-video"}, BillingMode: BillingModeVideo,
	}})
	require.Error(t, err)
}

func TestNormalizeGroupModelPricingClearsHiddenTokenIntervals(t *testing.T) {
	price := 1.0
	got, err := normalizeGroupModelPricing(PlatformGrok, []ChannelModelPricing{{
		Models: []string{"grok-4.6"}, BillingMode: BillingModeToken,
		InputPrice: &price, Intervals: []PricingInterval{{InputPrice: &price}},
	}})
	require.NoError(t, err)
	require.Empty(t, got[0].Intervals)
}

func TestResponsesProbeContract(t *testing.T) {
	account := &Account{Credentials: map[string]any{"model_mapping": map[string]any{
		"requested-b": "upstream-b", "requested-a": "upstream-a", "wildcard": "gpt-*",
	}}}
	require.Equal(t, "upstream-a", selectResponsesProbeModel(account))

	payload := openaiResponsesProbePayload("upstream-a")
	require.Equal(t, "upstream-a", jsonPathString(t, payload, "model"))
	require.Equal(t, "required", jsonPathString(t, payload, "tool_choice"))

	tests := []struct {
		name       string
		status     int
		body       string
		conclusive bool
		supported  bool
	}{
		{"function call", http.StatusOK, `{"status":"completed","output":[{"type":"function_call"}]}`, true, true},
		{"no function call", http.StatusOK, `{"status":"completed","output":[{"type":"message"}]}`, true, false},
		{"failed", http.StatusOK, `{"status":"failed"}`, false, false},
		{"max output incomplete", http.StatusOK, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, false, false},
		{"not found", http.StatusNotFound, `{}`, true, false},
		{"business error proves endpoint", http.StatusUnprocessableEntity, `{}`, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			require.Equal(t, tt.conclusive, responsesProbeVerdictIsConclusive(tt.status, body))
			if tt.conclusive {
				require.Equal(t, tt.supported, decideResponsesProbeSupport(tt.status, body))
			}
		})
	}

	headers := make(http.Header)
	applyOpenAICodexProbeHeaders(headers)
	require.Equal(t, "codex_cli_rs", headers.Get("originator"))
	require.Equal(t, "responses=experimental", headers.Get("OpenAI-Beta"))
	require.NotEmpty(t, headers.Get("version"))
	require.NotEmpty(t, headers.Get("X-Codex-Window-ID"))
}

func TestGroupModelPricingMigrationIsNonDestructive(t *testing.T) {
	path := filepath.Join("..", "..", "migrations", "207_group_model_pricing.sql")
	sqlBytes, err := os.ReadFile(path)
	require.NoError(t, err)
	sqlText := strings.ToLower(string(sqlBytes))
	require.Contains(t, sqlText, "long_context_pricing_enabled boolean not null default true")
	require.Contains(t, sqlText, "model_pricing jsonb")
	require.NotContains(t, sqlText, "update groups")
}

func TestPerRequestZeroTierDoesNotFallBackToDefaultPrice(t *testing.T) {
	zero := 0.0
	resolver := NewModelPricingResolver(nil, NewBillingService(nil, nil))
	resolved := &ResolvedPricing{
		Mode: BillingModeVideo, DefaultPerRequestPrice: 2,
		RequestTiers: []PricingInterval{{TierLabel: "720p", PerRequestPrice: &zero}},
	}
	cost, err := resolver.billingService.CalculateCostUnified(CostInput{
		Ctx: context.Background(), Model: "grok-imagine-video", UsageUnits: 10,
		SizeTier: "720P", RateMultiplier: 1, Resolver: resolver, Resolved: resolved,
	})
	require.NoError(t, err)
	require.Zero(t, cost.TotalCost)
}

type leaderLockCacheStub struct {
	acquired bool
	err      error
	released bool
}

func (s *leaderLockCacheStub) TryAcquireLeaderLock(context.Context, string, string, time.Duration) (bool, error) {
	return s.acquired, s.err
}

func (s *leaderLockCacheStub) ReleaseLeaderLock(context.Context, string, string) error {
	s.released = true
	return nil
}

func TestSingletonLeaderLockUsesOwnerCheckedCacheRelease(t *testing.T) {
	cache := &leaderLockCacheStub{acquired: true}
	release, ok := tryAcquireSingletonLeaderLock(context.Background(), cache, nil, "backup", "instance-a", time.Minute)
	require.True(t, ok)
	require.NotNil(t, release)
	require.False(t, cache.released)
	release()
	require.True(t, cache.released)

	cache = &leaderLockCacheStub{acquired: false}
	release, ok = tryAcquireSingletonLeaderLock(context.Background(), cache, nil, "backup", "instance-b", time.Minute)
	require.False(t, ok)
	require.Nil(t, release)
}

func jsonPathString(t *testing.T, data []byte, key string) string {
	t.Helper()
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(data, &decoded))
	value, _ := decoded[key].(string)
	return value
}
