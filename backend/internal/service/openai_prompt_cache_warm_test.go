package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type promptCacheWarmTestCache struct {
	entries  []OpenAIPromptCacheWarmAccount
	records  []OpenAIPromptCacheWarmAccount
	getCalls int
	avoids   []int64
}

func (c *promptCacheWarmTestCache) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	return 0, nil
}

func (c *promptCacheWarmTestCache) SetSessionAccountID(context.Context, int64, string, int64, time.Duration) error {
	return nil
}

func (c *promptCacheWarmTestCache) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (c *promptCacheWarmTestCache) DeleteSessionAccountID(context.Context, int64, string) error {
	return nil
}

func (c *promptCacheWarmTestCache) GetOpenAIPromptCacheWarmAccounts(context.Context, int64, string) ([]OpenAIPromptCacheWarmAccount, error) {
	c.getCalls++
	return append([]OpenAIPromptCacheWarmAccount(nil), c.entries...), nil
}

func (c *promptCacheWarmTestCache) RecordOpenAIPromptCacheWarmResult(_ context.Context, _ int64, _ string, accountID int64, inputTokens int, cachedTokens int, _ time.Duration) error {
	c.records = append(c.records, OpenAIPromptCacheWarmAccount{AccountID: accountID, InputTokens: int64(inputTokens), CachedTokens: int64(cachedTokens)})
	return nil
}

func (c *promptCacheWarmTestCache) AvoidOpenAIPromptCacheWarmAccount(context.Context, int64, string, int64, time.Time, time.Duration) error {
	c.avoids = append(c.avoids, 1)
	return nil
}

func promptCacheAdvancedTestAccount(id int64, priority int) *Account {
	account := promptCacheBoostTestAccount(id)
	account.Priority = priority
	account.Status = StatusActive
	account.Schedulable = true
	account.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	account.Credentials["prompt_cache_boost_upstream_hit_priority_enabled"] = true
	account.Credentials["prompt_cache_smart_routing_enabled"] = true
	account.Credentials["prompt_cache_account_relay_enabled"] = true
	account.Credentials["prompt_cache_key_optimization_enabled"] = true
	account.Credentials["prompt_cache_long_context_enhancement_enabled"] = true
	return account
}

func TestOpenAIPromptCacheAdvancedFlagsRequireUpstreamPriority(t *testing.T) {
	account := promptCacheAdvancedTestAccount(1, 1)
	require.True(t, account.IsOpenAIPromptCacheSmartRoutingEnabled())
	require.True(t, account.IsOpenAIPromptCacheAccountRelayEnabled())
	require.True(t, account.IsOpenAIPromptCacheKeyOptimizationEnabled())
	require.True(t, account.IsOpenAIPromptCacheLongContextEnhancementEnabled())

	delete(account.Credentials, "prompt_cache_boost_upstream_hit_priority_enabled")
	require.False(t, account.IsOpenAIPromptCacheSmartRoutingEnabled())
	require.False(t, account.IsOpenAIPromptCacheAccountRelayEnabled())
	require.False(t, account.IsOpenAIPromptCacheKeyOptimizationEnabled())
	require.False(t, account.IsOpenAIPromptCacheLongContextEnhancementEnabled())

	oauth := promptCacheAdvancedTestAccount(2, 1)
	oauth.Type = AccountTypeOAuth
	delete(oauth.Credentials, "pool_mode")
	require.True(t, oauth.IsOpenAIPromptCacheSmartRoutingEnabled())
}

func TestOpenAIPromptCacheKeyOptimizationIgnoresNonPrefixControls(t *testing.T) {
	account := promptCacheAdvancedTestAccount(3, 1)
	bodyA := []byte(`{"model":"gpt-5.6","instructions":"stable","tools":[{"type":"function","name":"lookup"}],"reasoning":{"effort":"high"},"response_format":{"type":"json_object"}}`)
	bodyB := []byte(`{"model":"gpt-5.6","instructions":"stable","tools":[{"type":"function","name":"lookup"}],"reasoning":{"effort":"low"},"response_format":{"type":"text"}}`)
	bodyC := []byte(`{"model":"gpt-5.6","instructions":"changed","tools":[{"type":"function","name":"lookup"}]}`)

	keyA := deriveOpenAIVirtualPromptCacheKey(account, "gpt-5.6", bodyA)
	keyB := deriveOpenAIVirtualPromptCacheKey(account, "gpt-5.6", bodyB)
	keyC := deriveOpenAIVirtualPromptCacheKey(account, "gpt-5.6", bodyC)
	require.NotEmpty(t, keyA)
	require.Equal(t, keyA, keyB)
	require.NotEqual(t, keyA, keyC)
	require.Contains(t, keyA, "nuro-pcache-v3-")
}

func TestPrioritizeOpenAIPromptCacheWarmCandidatesPreservesHardPriority(t *testing.T) {
	now := time.Now()
	warmCache := &promptCacheWarmTestCache{entries: []OpenAIPromptCacheWarmAccount{
		{AccountID: 2, HitRateEWMA: 0.95, Samples: 5, LastSuccessAt: now.Unix(), LastHitAt: now.Unix()},
	}}
	svc := &OpenAIGatewayService{cache: warmCache}
	accountA := promptCacheAdvancedTestAccount(1, 1)
	accountB := promptCacheAdvancedTestAccount(2, 2)
	candidates := []openAIAccountCandidateScore{
		{account: accountA, loadInfo: &AccountLoadInfo{}, healthScore: 1, hasHealthScore: true},
		{account: accountB, loadInfo: &AccountLoadInfo{}, healthScore: 1, hasHealthScore: true},
	}
	req := OpenAIAccountScheduleRequest{SessionHash: openAIPromptCacheBoostOptimizedAffinitySessionPrefix + "priority"}

	ordered := svc.prioritizeOpenAIPromptCacheWarmCandidates(context.Background(), req, candidates)
	require.Equal(t, int64(1), ordered[0].account.ID)
}

func TestPrioritizeOpenAIPromptCacheWarmCandidatesDoesNotOverrideExistingTieBreakers(t *testing.T) {
	now := time.Now()
	warmCache := &promptCacheWarmTestCache{entries: []OpenAIPromptCacheWarmAccount{
		{AccountID: 2, HitRateEWMA: 0.95, Samples: 5, LastSuccessAt: now.Unix(), LastHitAt: now.Unix()},
	}}
	svc := &OpenAIGatewayService{cache: warmCache}
	first := promptCacheAdvancedTestAccount(1, 1)
	warm := promptCacheAdvancedTestAccount(2, 1)
	firstUsed := now.Add(-time.Minute)
	warmUsed := now
	first.LastUsedAt = &firstUsed
	warm.LastUsedAt = &warmUsed
	candidates := []openAIAccountCandidateScore{
		{account: first, loadInfo: &AccountLoadInfo{}, healthScore: 1, hasHealthScore: true},
		{account: warm, loadInfo: &AccountLoadInfo{}, healthScore: 1, hasHealthScore: true},
	}
	req := OpenAIAccountScheduleRequest{SessionHash: openAIPromptCacheBoostOptimizedAffinitySessionPrefix + "strict-tie"}

	ordered := svc.prioritizeOpenAIPromptCacheWarmCandidates(context.Background(), req, candidates)
	require.Equal(t, []int64{1, 2}, []int64{ordered[0].account.ID, ordered[1].account.ID})
}

func TestPrioritizeOpenAIPromptCacheWarmCandidatesDisabledAvoidsCacheLookup(t *testing.T) {
	warmCache := &promptCacheWarmTestCache{}
	svc := &OpenAIGatewayService{cache: warmCache}
	accountA := promptCacheBoostTestAccount(11)
	accountB := promptCacheBoostTestAccount(12)
	for _, account := range []*Account{accountA, accountB} {
		account.Priority = 1
		account.Status = StatusActive
		account.Schedulable = true
		account.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
		account.Credentials["prompt_cache_boost_upstream_hit_priority_enabled"] = true
	}
	candidates := []openAIAccountCandidateScore{
		{account: accountA, loadInfo: &AccountLoadInfo{}},
		{account: accountB, loadInfo: &AccountLoadInfo{}},
	}
	req := OpenAIAccountScheduleRequest{SessionHash: openAIPromptCacheBoostUpstreamAffinitySessionPrefix + "disabled"}

	ordered := svc.prioritizeOpenAIPromptCacheWarmCandidates(context.Background(), req, candidates)
	require.Equal(t, []int64{11, 12}, []int64{ordered[0].account.ID, ordered[1].account.ID})
	require.Zero(t, warmCache.getCalls)
}

func TestPrioritizeOpenAIPromptCacheWarmCandidatesSkipsLowerPriorityFeatureAccounts(t *testing.T) {
	warmCache := &promptCacheWarmTestCache{}
	svc := &OpenAIGatewayService{cache: warmCache}
	higherPriority := promptCacheBoostTestAccount(13)
	higherPriority.Priority = 1
	higherPriority.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	higherPriority.Credentials["prompt_cache_boost_upstream_hit_priority_enabled"] = true
	lowerPriority := promptCacheAdvancedTestAccount(14, 2)
	candidates := []openAIAccountCandidateScore{
		{account: higherPriority, loadInfo: &AccountLoadInfo{}},
		{account: lowerPriority, loadInfo: &AccountLoadInfo{}},
	}
	req := OpenAIAccountScheduleRequest{SessionHash: openAIPromptCacheBoostUpstreamAffinitySessionPrefix + "priority-gate"}

	_ = svc.prioritizeOpenAIPromptCacheWarmCandidates(context.Background(), req, candidates)
	require.Zero(t, warmCache.getCalls)
}

func TestPrioritizeOpenAIPromptCacheWarmCandidatesWithoutRelayPreservesFallbackOrder(t *testing.T) {
	now := time.Now()
	warmCache := &promptCacheWarmTestCache{entries: []OpenAIPromptCacheWarmAccount{
		{AccountID: 22, HitRateEWMA: 0.95, Samples: 5, LastSuccessAt: now.Unix(), LastHitAt: now.Unix()},
		{AccountID: 23, HitRateEWMA: 0.80, Samples: 5, LastSuccessAt: now.Unix(), LastHitAt: now.Unix()},
	}}
	svc := &OpenAIGatewayService{cache: warmCache}
	first := promptCacheAdvancedTestAccount(21, 1)
	selected := promptCacheAdvancedTestAccount(22, 1)
	delete(selected.Credentials, "prompt_cache_account_relay_enabled")
	second := promptCacheAdvancedTestAccount(23, 1)
	candidates := []openAIAccountCandidateScore{
		{account: first, loadInfo: &AccountLoadInfo{}, healthScore: 1, hasHealthScore: true},
		{account: second, loadInfo: &AccountLoadInfo{}, healthScore: 1, hasHealthScore: true},
		{account: selected, loadInfo: &AccountLoadInfo{}, healthScore: 1, hasHealthScore: true},
	}
	req := OpenAIAccountScheduleRequest{SessionHash: openAIPromptCacheBoostOptimizedAffinitySessionPrefix + "no-relay"}

	ordered := svc.prioritizeOpenAIPromptCacheWarmCandidates(context.Background(), req, candidates)
	require.Equal(t, []int64{22, 21, 23}, []int64{ordered[0].account.ID, ordered[1].account.ID, ordered[2].account.ID})
}

func TestPrioritizeOpenAIPromptCacheWarmCandidatesPreservesCompactSupportTier(t *testing.T) {
	now := time.Now()
	warmCache := &promptCacheWarmTestCache{entries: []OpenAIPromptCacheWarmAccount{
		{AccountID: 22, HitRateEWMA: 0.95, Samples: 5, LastSuccessAt: now.Unix(), LastHitAt: now.Unix()},
	}}
	svc := &OpenAIGatewayService{cache: warmCache}
	supported := promptCacheAdvancedTestAccount(21, 1)
	supported.Extra = map[string]any{"openai_compact_supported": true}
	unknown := promptCacheAdvancedTestAccount(22, 1)
	candidates := []openAIAccountCandidateScore{
		{account: supported, loadInfo: &AccountLoadInfo{}, healthScore: 1, hasHealthScore: true},
		{account: unknown, loadInfo: &AccountLoadInfo{}, healthScore: 1, hasHealthScore: true},
	}
	req := OpenAIAccountScheduleRequest{
		SessionHash:    openAIPromptCacheBoostOptimizedAffinitySessionPrefix + "compact",
		RequireCompact: true,
	}

	ordered := svc.prioritizeOpenAIPromptCacheWarmCandidates(context.Background(), req, candidates)
	require.Equal(t, []int64{21, 22}, []int64{ordered[0].account.ID, ordered[1].account.ID})
}

func TestPrioritizeOpenAIPromptCacheWarmCandidatesRelaysHealthyPeers(t *testing.T) {
	now := time.Now()
	warmCache := &promptCacheWarmTestCache{entries: []OpenAIPromptCacheWarmAccount{
		{AccountID: 2, HitRateEWMA: 0.95, Samples: 5, LastSuccessAt: now.Unix(), LastHitAt: now.Unix()},
		{AccountID: 3, HitRateEWMA: 0.80, Samples: 5, LastSuccessAt: now.Unix(), LastHitAt: now.Unix()},
	}}
	svc := &OpenAIGatewayService{cache: warmCache}
	accountA := promptCacheAdvancedTestAccount(1, 1)
	accountB := promptCacheAdvancedTestAccount(2, 1)
	accountC := promptCacheAdvancedTestAccount(3, 1)
	candidates := []openAIAccountCandidateScore{
		{account: accountA, loadInfo: &AccountLoadInfo{}, healthScore: 1, hasHealthScore: true},
		{account: accountC, loadInfo: &AccountLoadInfo{}, healthScore: 1, hasHealthScore: true},
		{account: accountB, loadInfo: &AccountLoadInfo{}, healthScore: 1, hasHealthScore: true},
	}
	req := OpenAIAccountScheduleRequest{SessionHash: openAIPromptCacheBoostOptimizedAffinitySessionPrefix + "relay"}

	ordered := svc.prioritizeOpenAIPromptCacheWarmCandidates(context.Background(), req, candidates)
	require.Equal(t, []int64{2, 3, 1}, []int64{ordered[0].account.ID, ordered[1].account.ID, ordered[2].account.ID})
}

func TestRecordOpenAIPromptCacheWarmResultUsesSuccessfulUsage(t *testing.T) {
	warmCache := &promptCacheWarmTestCache{}
	svc := &OpenAIGatewayService{cache: warmCache}
	account := promptCacheAdvancedTestAccount(4, 1)
	groupID := int64(9)
	svc.recordOpenAIPromptCacheWarmResult(context.Background(), &OpenAIRecordUsageInput{
		Account:                 account,
		PromptCacheGroupID:      &groupID,
		PromptCacheAffinityHash: openAIPromptCacheBoostOptimizedAffinitySessionPrefix + "record",
		Result: &OpenAIForwardResult{Usage: OpenAIUsage{
			InputTokens:          1000,
			CacheReadInputTokens: 700,
		}},
	})

	require.Len(t, warmCache.records, 1)
	require.Equal(t, int64(4), warmCache.records[0].AccountID)
	require.Equal(t, int64(700), warmCache.records[0].CachedTokens)
}

func TestOpenAIPromptCacheWarmAvoidanceIgnoresClientRequestErrors(t *testing.T) {
	warmCache := &promptCacheWarmTestCache{}
	svc := &OpenAIGatewayService{cache: warmCache}
	account := promptCacheAdvancedTestAccount(6, 1)
	sessionHash := openAIPromptCacheBoostOptimizedAffinitySessionPrefix + "client-error"

	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, sessionHash, account, &UpstreamFailoverError{
		StatusCode: http.StatusBadRequest,
		Message:    "invalid_request_error: invalid parameter",
	})
	require.Empty(t, warmCache.avoids)

	svc.HandleOpenAIAccountFailoverSwitch(context.Background(), nil, sessionHash, account, &UpstreamFailoverError{
		StatusCode: http.StatusTooManyRequests,
		Message:    "rate limited",
	})
	require.Len(t, warmCache.avoids, 1)
}

func TestShouldEnhanceOpenAIPromptCacheLongContextRequiresProvenHit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now()
	account := promptCacheAdvancedTestAccount(5, 1)
	warmCache := &promptCacheWarmTestCache{entries: []OpenAIPromptCacheWarmAccount{
		{AccountID: account.ID, HitRateEWMA: 0.8, Samples: 4, LastSuccessAt: now.Unix(), LastHitAt: now.Unix()},
	}}
	svc := &OpenAIGatewayService{cache: warmCache}
	groupID := int64(12)
	contextRecorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(contextRecorder)
	ginContext.Set("api_key", &APIKey{GroupID: &groupID})
	body := []byte(`{"model":"gpt-5.6","instructions":"` + strings.Repeat("stable-prefix-", 700) + `"}`)

	require.GreaterOrEqual(t, len(body), openAIPromptCacheEnhancedReplayMinBodyBytes)
	require.Less(t, len(body), openAIPromptCacheBoostMinBodyBytes)
	require.True(t, svc.shouldEnhanceOpenAIPromptCacheLongContext(context.Background(), ginContext, account, "gpt-5.6", body))

	warmCache.entries[0].HitRateEWMA = 0.2
	require.False(t, svc.shouldEnhanceOpenAIPromptCacheLongContext(context.Background(), ginContext, account, "gpt-5.6", body))
}
