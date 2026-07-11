package service

import (
	"context"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	openAIPromptCacheWarmTTL                    = 24 * time.Hour
	openAIPromptCacheWarmHalfLife               = 12 * time.Hour
	openAIPromptCacheWarmMinScore               = 0.12
	openAIPromptCacheWarmLoadSkew               = 10
	openAIPromptCacheWarmErrorSkew              = 0.05
	openAIPromptCacheEnhancedReplayMinBodyBytes = 8 * 1024
	openAIPromptCacheWarmLookupTimeout          = 50 * time.Millisecond
)

func (s *OpenAIGatewayService) openAIPromptCacheWarmCache() OpenAIPromptCacheWarmCache {
	if s == nil || s.cache == nil {
		return nil
	}
	cache, _ := s.cache.(OpenAIPromptCacheWarmCache)
	return cache
}

func openAIPromptCacheWarmScore(entry OpenAIPromptCacheWarmAccount, now time.Time) float64 {
	if entry.AccountID <= 0 || entry.Samples < 2 || entry.LastSuccessAt <= 0 || entry.LastHitAt <= 0 || entry.AvoidUntil > now.Unix() {
		return 0
	}
	if math.IsNaN(entry.HitRateEWMA) || math.IsInf(entry.HitRateEWMA, 0) || entry.HitRateEWMA <= 0 {
		return 0
	}
	hitRate := math.Min(entry.HitRateEWMA, 1)
	confidence := math.Min(float64(entry.Samples)/3, 1)
	age := now.Sub(time.Unix(entry.LastSuccessAt, 0))
	if age < 0 {
		age = 0
	}
	decay := math.Exp(-age.Hours() * math.Ln2 / openAIPromptCacheWarmHalfLife.Hours())
	return hitRate * confidence * decay
}

func (s *OpenAIGatewayService) recordOpenAIPromptCacheWarmResult(ctx context.Context, input *OpenAIRecordUsageInput) {
	if input == nil || input.Account == nil || input.Result == nil ||
		(!input.Account.IsOpenAIPromptCacheSmartRoutingEnabled() && !input.Account.IsOpenAIPromptCacheLongContextEnhancementEnabled()) ||
		!IsOpenAIPromptCacheBoostAffinitySessionHash(input.PromptCacheAffinityHash) {
		return
	}
	cache := s.openAIPromptCacheWarmCache()
	if cache == nil || input.Result.Usage.InputTokens <= 0 {
		return
	}
	cacheCtx, cancel := context.WithTimeout(ctx, openAIPromptCacheWarmLookupTimeout)
	defer cancel()
	_ = cache.RecordOpenAIPromptCacheWarmResult(
		cacheCtx,
		derefGroupID(input.PromptCacheGroupID),
		input.PromptCacheAffinityHash,
		input.Account.ID,
		input.Result.Usage.InputTokens,
		input.Result.Usage.CacheReadInputTokens,
		openAIPromptCacheWarmTTL,
	)
}

func (s *OpenAIGatewayService) avoidOpenAIPromptCacheWarmAccount(ctx context.Context, groupID *int64, affinityHash string, account *Account, failoverErr *UpstreamFailoverError) {
	if account == nil || failoverErr == nil || !account.IsOpenAIPromptCacheAccountRelayEnabled() || !IsOpenAIPromptCacheBoostAffinitySessionHash(affinityHash) {
		return
	}
	cache := s.openAIPromptCacheWarmCache()
	if cache == nil {
		return
	}
	delay := 10 * time.Second
	switch failoverErr.StatusCode {
	case http.StatusTooManyRequests:
		delay = time.Second
	case http.StatusUnauthorized, http.StatusForbidden:
		delay = time.Minute
	}
	cacheCtx, cancel := context.WithTimeout(ctx, openAIPromptCacheWarmLookupTimeout)
	defer cancel()
	_ = cache.AvoidOpenAIPromptCacheWarmAccount(cacheCtx, derefGroupID(groupID), affinityHash, account.ID, time.Now().Add(delay), openAIPromptCacheWarmTTL)
}

func (s *OpenAIGatewayService) prioritizeOpenAIPromptCacheWarmCandidates(ctx context.Context, req OpenAIAccountScheduleRequest, candidates []openAIAccountCandidateScore) []openAIAccountCandidateScore {
	if len(candidates) <= 1 || !IsOpenAIPromptCacheBoostAffinitySessionHash(req.SessionHash) {
		return candidates
	}
	baseline := candidates[0]
	if baseline.account == nil || baseline.loadInfo == nil {
		return candidates
	}
	preferSoonestReset := false
	if s != nil {
		weights := s.openAIWSSchedulerWeights()
		preferSoonestReset = weights.Reset > 0 || s.schedulingConfig().PreferSoonestReset
	}
	cacheCandidateAvailable := false
	for _, candidate := range candidates {
		if openAIPromptCacheWarmCandidateCompatible(req, candidate, baseline, preferSoonestReset) {
			cacheCandidateAvailable = true
			break
		}
	}
	if !cacheCandidateAvailable {
		return candidates
	}
	cache := s.openAIPromptCacheWarmCache()
	if cache == nil {
		return candidates
	}
	cacheCtx, cancel := context.WithTimeout(ctx, openAIPromptCacheWarmLookupTimeout)
	defer cancel()
	entries, err := cache.GetOpenAIPromptCacheWarmAccounts(cacheCtx, derefGroupID(req.GroupID), req.SessionHash)
	if err != nil || len(entries) == 0 {
		return candidates
	}
	now := time.Now()
	scores := make(map[int64]float64, len(entries))
	for _, entry := range entries {
		if score := openAIPromptCacheWarmScore(entry, now); score >= openAIPromptCacheWarmMinScore {
			scores[entry.AccountID] = score
		}
	}
	if len(scores) == 0 {
		return candidates
	}
	warm := make([]openAIAccountCandidateScore, 0, 3)
	rest := make([]openAIAccountCandidateScore, 0, len(candidates))
	for _, candidate := range candidates {
		account := candidate.account
		if account == nil || candidate.loadInfo == nil {
			rest = append(rest, candidate)
			continue
		}
		score, known := scores[account.ID]
		eligible := known && openAIPromptCacheWarmCandidateCompatible(req, candidate, baseline, preferSoonestReset)
		if eligible {
			candidate.score = score
			warm = append(warm, candidate)
			continue
		}
		rest = append(rest, candidate)
	}
	if len(warm) == 0 {
		return candidates
	}
	sort.SliceStable(warm, func(i, j int) bool { return warm[i].score > warm[j].score })
	relayEnabled := warm[0].account.IsOpenAIPromptCacheAccountRelayEnabled()
	if !relayEnabled && len(warm) > 1 {
		selectedID := warm[0].account.ID
		ordered := make([]openAIAccountCandidateScore, 0, len(candidates))
		ordered = append(ordered, warm[0])
		for _, candidate := range candidates {
			if candidate.account != nil && candidate.account.ID == selectedID {
				continue
			}
			ordered = append(ordered, candidate)
		}
		return ordered
	}
	ordered := make([]openAIAccountCandidateScore, 0, len(candidates))
	ordered = append(ordered, warm...)
	ordered = append(ordered, rest...)
	return ordered
}

func openAIPromptCacheWarmCandidateCompatible(
	req OpenAIAccountScheduleRequest,
	candidate openAIAccountCandidateScore,
	baseline openAIAccountCandidateScore,
	preferSoonestReset bool,
) bool {
	if candidate.account == nil || candidate.loadInfo == nil || baseline.account == nil || baseline.loadInfo == nil {
		return false
	}
	return candidate.account.IsOpenAIPromptCacheSmartRoutingEnabled() &&
		candidate.account.Priority == baseline.account.Priority &&
		candidate.account.IsPoolMode() == baseline.account.IsPoolMode() &&
		(!req.RequireCompact || openAICompactSupportTier(candidate.account) == openAICompactSupportTier(baseline.account)) &&
		sameOpenAIHealthScoreTie(candidate.healthScore, candidate.hasHealthScore, baseline.healthScore, baseline.hasHealthScore) &&
		(!preferSoonestReset || sameOpenAIAccountResetTie(candidate.account, baseline.account)) &&
		candidate.loadInfo.LoadRate <= baseline.loadInfo.LoadRate+openAIPromptCacheWarmLoadSkew &&
		candidate.loadInfo.WaitingCount <= baseline.loadInfo.WaitingCount+1 &&
		candidate.errorRate <= baseline.errorRate+openAIPromptCacheWarmErrorSkew
}

func (s *OpenAIGatewayService) shouldEnhanceOpenAIPromptCacheLongContext(ctx context.Context, c *gin.Context, account *Account, model string, body []byte) bool {
	if account == nil || !account.IsOpenAIPromptCacheLongContextEnhancementEnabled() || len(body) < openAIPromptCacheEnhancedReplayMinBodyBytes || len(body) >= openAIPromptCacheBoostMinBodyBytes {
		return false
	}
	affinityHash := deriveOpenAIPromptCacheBoostAffinityHashForAccount(account, model, body)
	if !IsOpenAIPromptCacheBoostAffinitySessionHash(affinityHash) {
		return false
	}
	cache := s.openAIPromptCacheWarmCache()
	if cache == nil {
		return false
	}
	groupID := getOpenAIGroupIDFromContext(c)
	cacheCtx, cancel := context.WithTimeout(ctx, openAIPromptCacheWarmLookupTimeout)
	defer cancel()
	entries, err := cache.GetOpenAIPromptCacheWarmAccounts(cacheCtx, groupID, affinityHash)
	if err != nil {
		return false
	}
	now := time.Now()
	for _, entry := range entries {
		if entry.AccountID != account.ID || entry.Samples < 3 || entry.HitRateEWMA < 0.65 || entry.LastHitAt <= 0 {
			continue
		}
		if now.Sub(time.Unix(entry.LastHitAt, 0)) <= 6*time.Hour && entry.AvoidUntil <= now.Unix() {
			return true
		}
	}
	return false
}
