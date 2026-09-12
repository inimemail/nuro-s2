package service

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const (
	accountHealthErrorCeiling         = 0.20
	accountHealthErrorGap             = 0.15
	accountHealthFirstCostRatio       = 1.25
	accountHealthFirstCostAbsoluteGap = 0.20
	accountHealthCostTierEpsilon      = 1e-9
	accountHealthTTFTBand15sMs        = 15_000.0
	accountHealthTTFTBand30sMs        = 30_000.0
	accountHealthTTFTBand60sMs        = 60_000.0
	// accountHealthScoreBandThreshold 是"健康相近视为同带"的容差。带越窄，
	// 速度/错误率差距越容易把更优账号排到前面；带越宽越偏向负载均衡。
	// 健康分满分 1.0，TTFT 维度权重见 accountHealthTTFTWeight，因此约 2x 的
	// 首 token 速度差即可跨带胜出，10%~30% 的抖动仍判为同带走均衡。
	accountHealthScoreBandThreshold           = 0.12
	accountHealthCacheAffinityMaxGap          = 0.10
	accountHealthUnknownScore                 = 0.82
	accountHealthUnknownTTFTScore             = 0.78
	accountHealthUnknownMinSamples      int64 = 3
	accountHealthUnknownExploreEvery          = uint64(20)
	accountHealthUnknownExploreCooldown       = time.Minute
	accountHealthDegradedRecoveryDelay        = 5 * time.Minute
	// 健康分权重：错误率 + 首 token(TTFT)。提高 TTFT 权重让"更丝滑/回复更快"
	// 的账号在同优先级里更容易被优先选中（仅作用于无粘性的 load-balance 选号层）。
	accountHealthErrorWeight = 0.55
	accountHealthTTFTWeight  = 0.45
)

type adaptiveAccountHealthProfile struct {
	account      *Account
	errorRate    float64
	errorSamples int64
	p50          float64
	p90          float64
	hasTTFT      bool
	ttftSamples  int64
	errorUpdated time.Time
	ttftUpdated  time.Time
}

type adaptiveTTFTSwitchPolicy struct {
	enabled         bool
	thresholdMs     float64
	sampleFreshness time.Duration
}

func defaultAdaptiveTTFTSwitchPolicy() adaptiveTTFTSwitchPolicy {
	return adaptiveTTFTSwitchPolicy{
		enabled:         true,
		thresholdMs:     float64(DefaultAdaptiveTTFTSwitchThresholdSeconds) * 1000,
		sampleFreshness: time.Duration(DefaultAdaptiveHealthSampleFreshnessMinutes) * time.Minute,
	}
}

func adaptiveTTFTSwitchPolicyForGroup(group *Group) adaptiveTTFTSwitchPolicy {
	policy := defaultAdaptiveTTFTSwitchPolicy()
	if group == nil {
		return policy
	}
	// A zero threshold identifies legacy in-memory/cache values that predate
	// this setting. Preserve the migration default instead of interpreting the
	// zero-value bool as an explicit disable.
	policy.sampleFreshness = time.Duration(NormalizeAdaptiveHealthSampleFreshnessMinutes(group.AdaptiveHealthSampleFreshnessMinutes)) * time.Minute
	if group.AdaptiveTTFTSwitchThresholdSeconds != 0 {
		policy.enabled = group.AdaptiveTTFTSwitchEnabled
		policy.thresholdMs = float64(NormalizeAdaptiveTTFTSwitchThresholdSeconds(group.AdaptiveTTFTSwitchThresholdSeconds)) * 1000
	}
	return policy
}

func adaptiveTTFTSwitchPolicyFromValues(enabled bool, thresholdSeconds int, freshnessMinutes ...int) adaptiveTTFTSwitchPolicy {
	policy := defaultAdaptiveTTFTSwitchPolicy()
	if thresholdSeconds != 0 {
		policy.enabled = enabled
		policy.thresholdMs = float64(NormalizeAdaptiveTTFTSwitchThresholdSeconds(thresholdSeconds)) * 1000
	}
	if len(freshnessMinutes) > 0 {
		policy.sampleFreshness = time.Duration(NormalizeAdaptiveHealthSampleFreshnessMinutes(freshnessMinutes[0])) * time.Minute
	}
	return policy
}

func adaptiveAccountPolicyIndexes(profiles []adaptiveAccountHealthProfile, costBalanced bool, now time.Time) []int {
	return adaptiveAccountPolicyIndexesWithTTFTPolicy(profiles, costBalanced, now, defaultAdaptiveTTFTSwitchPolicy())
}

func adaptiveAccountPolicyIndexesWithTTFTPolicy(profiles []adaptiveAccountHealthProfile, costBalanced bool, now time.Time, policy adaptiveTTFTSwitchPolicy) []int {
	if len(profiles) == 0 {
		return nil
	}
	profiles = freshAdaptiveHealthProfiles(profiles, now, policy)
	errorQualified := adaptiveErrorQualifiedIndexes(profiles)
	if len(errorQualified) == 0 {
		for i := range profiles {
			errorQualified = append(errorQualified, i)
		}
	}
	if costBalanced {
		return adaptiveLowestUsableCostTierWithTTFTPolicy(profiles, errorQualified, now, policy)
	}
	return adaptiveHealthFirstIndexesWithTTFTPolicy(profiles, errorQualified, policy)
}

func freshAdaptiveHealthProfiles(profiles []adaptiveAccountHealthProfile, now time.Time, policy adaptiveTTFTSwitchPolicy) []adaptiveAccountHealthProfile {
	if len(profiles) == 0 || now.IsZero() || policy.sampleFreshness <= 0 {
		return profiles
	}
	fresh := append([]adaptiveAccountHealthProfile(nil), profiles...)
	for i := range fresh {
		if !fresh[i].errorUpdated.IsZero() && now.Sub(fresh[i].errorUpdated) >= policy.sampleFreshness {
			fresh[i].errorRate = 0
			fresh[i].errorSamples = 0
		}
		if !fresh[i].ttftUpdated.IsZero() && now.Sub(fresh[i].ttftUpdated) >= policy.sampleFreshness {
			fresh[i].p50 = 0
			fresh[i].p90 = 0
			fresh[i].hasTTFT = false
			fresh[i].ttftSamples = 0
		}
	}
	return fresh
}

func filterAdaptiveAccountsToPolicyTier(accounts, available []accountWithLoad, stats *accountRuntimeHealthStats, strategy string, now time.Time, history map[int64]AccountTTFTHistory) []accountWithLoad {
	return filterAdaptiveAccountsToPolicyTierWithTTFTPolicy(accounts, available, stats, strategy, now, history, defaultAdaptiveTTFTSwitchPolicy())
}

func filterAdaptiveAccountsToPolicyTierWithTTFTPolicy(accounts, available []accountWithLoad, stats *accountRuntimeHealthStats, strategy string, now time.Time, history map[int64]AccountTTFTHistory, policy adaptiveTTFTSwitchPolicy) []accountWithLoad {
	if len(accounts) == 0 || len(available) == 0 || !IsAdaptiveHealthSchedulingStrategy(strategy) {
		return available
	}
	candidates := freshAccountHealthCandidates(buildAccountHealthCandidatesWithHistory(accounts, stats, history), now, policy)
	profiles := make([]adaptiveAccountHealthProfile, len(candidates))
	for i, candidate := range candidates {
		profiles[i] = adaptiveAccountHealthProfile{account: candidate.item.account, errorRate: candidate.errorRate, errorSamples: candidate.sampleCount, p50: candidate.ttft, p90: candidate.ttftP90, hasTTFT: candidate.hasTTFT, ttftSamples: candidate.ttftSampleCount, errorUpdated: candidate.errorUpdated, ttftUpdated: candidate.ttftUpdated}
	}
	indexes := adaptiveAccountPolicyIndexesWithTTFTPolicy(profiles, IsHealthCostBalancedSchedulingStrategy(strategy), now, policy)
	allowed := make(map[int64]struct{}, len(indexes))
	for _, index := range indexes {
		if account := candidates[index].item.account; account != nil {
			allowed[account.ID] = struct{}{}
		}
	}
	filtered := make([]accountWithLoad, 0, len(available))
	for _, item := range available {
		if item.account != nil {
			if _, ok := allowed[item.account.ID]; ok {
				filtered = append(filtered, item)
			}
		}
	}
	return filtered
}

func adaptiveErrorQualifiedIndexes(profiles []adaptiveAccountHealthProfile) []int {
	best, known := 0.0, false
	for _, profile := range profiles {
		if profile.errorSamples < accountHealthUnknownMinSamples {
			continue
		}
		if !known || profile.errorRate < best {
			best, known = profile.errorRate, true
		}
	}
	ceiling := accountHealthErrorCeiling
	if known && best+accountHealthErrorGap > ceiling {
		ceiling = best + accountHealthErrorGap
	}
	result := make([]int, 0, len(profiles))
	for i, profile := range profiles {
		// Unknown runtime health is not an error. Keep such accounts eligible so
		// a newly discovered low-cost upstream can receive bounded evaluation.
		if profile.account != nil && (profile.errorSamples < accountHealthUnknownMinSamples || profile.errorRate <= ceiling) {
			result = append(result, i)
		}
	}
	return result
}

func adaptiveHealthFirstCostBand(profiles []adaptiveAccountHealthProfile, indexes []int, now time.Time) []int {
	_ = now
	return adaptiveHealthFirstIndexesWithTTFTPolicy(profiles, indexes, defaultAdaptiveTTFTSwitchPolicy())
}

func adaptiveHealthFirstIndexesWithTTFTPolicy(profiles []adaptiveAccountHealthProfile, indexes []int, policy adaptiveTTFTSwitchPolicy) []int {
	if !policy.enabled {
		return append([]int(nil), indexes...)
	}
	hasAcceptable := false
	for _, index := range indexes {
		if !adaptiveProfileExceedsTTFTThreshold(profiles[index], policy) {
			hasAcceptable = true
			break
		}
	}
	if !hasAcceptable {
		return append([]int(nil), indexes...)
	}
	result := make([]int, 0, len(indexes))
	for _, index := range indexes {
		if !adaptiveProfileExceedsTTFTThreshold(profiles[index], policy) {
			result = append(result, index)
		}
	}
	return result
}

// adaptiveSameHealthFirstCostBand reports whether two declared upstream rates
// belong to the same cost band used by the health-first policy. A session
// affinity may be retained only inside this band; a materially cheaper account
// must remain free to replace an expensive affinity.
func adaptiveSameHealthFirstCostBand(a, b float64) bool {
	if a < 0 || b < 0 {
		return false
	}
	if a == 0 || b == 0 {
		return a == 0 && b == 0
	}
	lower, higher := a, b
	if lower > higher {
		lower, higher = higher, lower
	}
	return higher <= lower*accountHealthFirstCostRatio && higher-lower <= accountHealthFirstCostAbsoluteGap
}

func adaptiveLowestUsableCostTier(profiles []adaptiveAccountHealthProfile, indexes []int, now time.Time) []int {
	return adaptiveLowestUsableCostTierWithTTFTPolicy(profiles, indexes, now, defaultAdaptiveTTFTSwitchPolicy())
}

func adaptiveLowestUsableCostTierWithTTFTPolicy(profiles []adaptiveAccountHealthProfile, indexes []int, now time.Time, policy adaptiveTTFTSwitchPolicy) []int {
	remaining := append([]int(nil), indexes...)
	var lowestTier []int
	for len(remaining) > 0 {
		lowest := adaptiveLowestMultiplier(profiles, remaining, now)
		if lowest == math.MaxFloat64 {
			return remaining
		}
		tier := make([]int, 0, len(remaining))
		next := make([]int, 0, len(remaining))
		for _, index := range remaining {
			rate, declared := accountEffectiveUpstreamMultiplier(profiles[index].account, now)
			if declared && math.Abs(rate-lowest) <= accountHealthCostTierEpsilon {
				tier = append(tier, index)
			} else {
				next = append(next, index)
			}
		}
		if lowestTier == nil {
			lowestTier = append([]int(nil), tier...)
			if !policy.enabled || !adaptiveTierAllExceedTTFTThreshold(profiles, tier, policy) {
				return tier
			}
			if len(next) == 0 {
				return tier
			}
			remaining = next
			continue
		}
		// Unknown accounts are default-healthy and must receive normal traffic in
		// the next exact multiplier layer. A measured layer is selected only when
		// it clears the configured limit or is clearly faster than the cheapest
		// degraded layer; proven equally-slow expensive layers are skipped.
		if adaptiveTierHasUntrustedTTFT(profiles, tier) || !adaptiveTierAllExceedTTFTThreshold(profiles, tier, policy) || adaptiveTierClearlyFaster(profiles, lowestTier, tier) {
			return tier
		}
		remaining = next
	}
	return lowestTier
}

func adaptiveProfileExceedsTTFTThreshold(profile adaptiveAccountHealthProfile, policy adaptiveTTFTSwitchPolicy) bool {
	return policy.enabled && adaptiveTrustedTTFT(profile.hasTTFT, profile.ttftSamples, profile.p50) && profile.p50 >= policy.thresholdMs
}

func adaptiveTierAllExceedTTFTThreshold(profiles []adaptiveAccountHealthProfile, tier []int, policy adaptiveTTFTSwitchPolicy) bool {
	if !policy.enabled || len(tier) == 0 {
		return false
	}
	for _, index := range tier {
		if !adaptiveProfileExceedsTTFTThreshold(profiles[index], policy) {
			return false
		}
	}
	return true
}

func adaptiveTierHasUntrustedTTFT(profiles []adaptiveAccountHealthProfile, tier []int) bool {
	for _, index := range tier {
		profile := profiles[index]
		if !adaptiveTrustedTTFT(profile.hasTTFT, profile.ttftSamples, profile.p50) {
			return true
		}
	}
	return false
}

func adaptiveTierClearlyFaster(profiles []adaptiveAccountHealthProfile, baseline, candidate []int) bool {
	for _, baselineIndex := range baseline {
		base := profiles[baselineIndex]
		improved := false
		for _, candidateIndex := range candidate {
			alternative := profiles[candidateIndex]
			if !adaptiveTrustedTTFT(alternative.hasTTFT, alternative.ttftSamples, alternative.p50) {
				continue
			}
			if adaptiveTTFTMateriallySlowerThan(base.p50, base.p90, alternative.p50, alternative.p90) ||
				(base.p50-alternative.p50 >= 5_000 && alternative.p50 <= base.p50*0.75) {
				improved = true
				break
			}
		}
		if !improved {
			return false
		}
	}
	return len(baseline) > 0
}

func adaptiveTierIsSeriouslySlow(profiles []adaptiveAccountHealthProfile, tier, all []int) bool {
	tierIDs := make(map[int64]struct{}, len(tier))
	for _, index := range tier {
		if profiles[index].account != nil {
			tierIDs[profiles[index].account.ID] = struct{}{}
		}
	}
	compared := false
	for _, tierIndex := range tier {
		candidate := profiles[tierIndex]
		if !adaptiveTrustedTTFT(candidate.hasTTFT, candidate.ttftSamples, candidate.p50) {
			return false
		}
		candidateSlow := false
		for _, otherIndex := range all {
			alternative := profiles[otherIndex]
			if alternative.account == nil || !adaptiveTrustedTTFT(alternative.hasTTFT, alternative.ttftSamples, alternative.p50) {
				continue
			}
			if _, sameTier := tierIDs[alternative.account.ID]; sameTier {
				continue
			}
			compared = true
			if adaptiveTTFTMateriallySlowerThan(candidate.p50, candidate.p90, alternative.p50, alternative.p90) {
				candidateSlow = true
				break
			}
		}
		if !candidateSlow {
			return false
		}
	}
	return compared
}

func adaptiveTTFTMateriallySlowerThan(candidateP50, candidateP90, alternativeP50, alternativeP90 float64) bool {
	if candidateP50 <= 0 || alternativeP50 <= 0 {
		return false
	}
	// TTFT is evaluated in broad, stable bands. Small samples and ordinary
	// jitter must not promote a more expensive account.
	candidateP50Band := adaptiveTTFTBand(candidateP50)
	alternativeP50Band := adaptiveTTFTBand(alternativeP50)
	if candidateP50Band != alternativeP50Band {
		return candidateP50Band > alternativeP50Band
	}
	// P90 is only a tail-latency tie breaker after P50 lands in the same band.
	return candidateP90 > 0 && alternativeP90 > 0 &&
		adaptiveTTFTBand(candidateP90) >= adaptiveTTFTBand(alternativeP90)+2
}

func adaptiveTTFTBand(ms float64) int {
	if ms <= 0 || ms <= accountHealthTTFTBand15sMs {
		return 0
	}
	if ms <= accountHealthTTFTBand30sMs {
		return 1
	}
	if ms <= accountHealthTTFTBand60sMs {
		return 2
	}
	return 3
}

func adaptiveTrustedTTFT(hasTTFT bool, samples int64, p50 float64) bool {
	return hasTTFT && samples >= accountHealthUnknownMinSamples && p50 > 0
}

// adaptiveProfileHealthLess compares only adaptive health dimensions. It is a
// strict, transitive ordering even when some candidates still have incomplete
// evidence. Unknown error/TTFT evidence is treated optimistically as healthy;
// the caller applies multiplier, affinity and load tie-breakers afterwards.
func adaptiveProfileHealthLess(a, b adaptiveAccountHealthProfile, costBalanced bool, policy adaptiveTTFTSwitchPolicy) (bool, bool) {
	aError, bError := adaptiveEffectiveErrorRate(a), adaptiveEffectiveErrorRate(b)
	if !costBalanced {
		if aError != bError {
			return aError < bError, true
		}
		aTTFTBand := adaptiveHealthFirstTTFTOrderBand(a, policy)
		bTTFTBand := adaptiveHealthFirstTTFTOrderBand(b, policy)
		if aTTFTBand != bTTFTBand {
			return aTTFTBand < bTTFTBand, true
		}
		return false, false
	}

	aTTFTBand := adaptiveCostBalancedTTFTOrderBand(a)
	bTTFTBand := adaptiveCostBalancedTTFTOrderBand(b)
	if aTTFTBand != bTTFTBand {
		return aTTFTBand < bTTFTBand, true
	}
	if aError != bError {
		return aError < bError, true
	}
	return false, false
}

func adaptiveEffectiveErrorRate(profile adaptiveAccountHealthProfile) float64 {
	if profile.errorSamples < accountHealthUnknownMinSamples {
		return 0
	}
	return profile.errorRate
}

func adaptiveHealthFirstTTFTOrderBand(profile adaptiveAccountHealthProfile, policy adaptiveTTFTSwitchPolicy) int {
	if !adaptiveProfileExceedsTTFTThreshold(profile, policy) {
		return 0
	}
	return adaptiveTTFTBand(profile.p50) + 1
}

func adaptiveCostBalancedTTFTOrderBand(profile adaptiveAccountHealthProfile) int {
	if !adaptiveTrustedTTFT(profile.hasTTFT, profile.ttftSamples, profile.p50) {
		return 0
	}
	return adaptiveTTFTBand(profile.p50)
}

func adaptiveTTFTMateriallyWorseInAnyDimension(candidateP50, candidateP90, alternativeP50, alternativeP90 float64) bool {
	return adaptiveTTFTMateriallySlowerThan(candidateP50, candidateP90, alternativeP50, alternativeP90)
}

func adaptiveLowestMultiplier(profiles []adaptiveAccountHealthProfile, indexes []int, now time.Time) float64 {
	lowest := math.MaxFloat64
	for _, index := range indexes {
		rate, declared := accountEffectiveUpstreamMultiplier(profiles[index].account, now)
		if declared && rate < lowest {
			lowest = rate
		}
	}
	return lowest
}

func adaptiveHighestMultiplier(profiles []adaptiveAccountHealthProfile, indexes []int, now time.Time) float64 {
	highest := 0.0
	for _, index := range indexes {
		rate, declared := accountEffectiveUpstreamMultiplier(profiles[index].account, now)
		if declared && rate > highest {
			highest = rate
		}
	}
	return highest
}

func adaptiveHasDeclaredMultiplier(profiles []adaptiveAccountHealthProfile, indexes []int, now time.Time) bool {
	for _, index := range indexes {
		if _, declared := accountEffectiveUpstreamMultiplier(profiles[index].account, now); declared {
			return true
		}
	}
	return false
}

func adaptiveLatencyLess(aP50, aP90 float64, aKnown bool, bP50, bP90 float64, bKnown bool) (bool, bool) {
	if !aKnown || !bKnown {
		return false, false
	}
	if adaptiveTTFTMateriallySlowerThan(aP50, aP90, bP50, bP90) {
		return false, true
	}
	if adaptiveTTFTMateriallySlowerThan(bP50, bP90, aP50, aP90) {
		return true, true
	}
	return false, false
}

type accountRuntimeHealthStats struct {
	accounts           sync.Map
	selectionCounter   atomic.Uint64
	unknownExploreAt   sync.Map
	degradedRecoveryAt sync.Map
	healthFirstCounter sync.Map
	healthFirstProbeAt sync.Map
	warmingUpCounter   sync.Map
	warmingUpAt        sync.Map
}

type accountHealthGroupProbeKey struct {
	groupID   int64
	accountID int64
}

type accountRuntimeHealthStat struct {
	mu                sync.Mutex
	errorRateEWMABits atomic.Uint64
	ttftEWMABits      atomic.Uint64
	sampleCount       atomic.Int64
	ttftSampleCount   atomic.Int64
	lastUpdatedNano   atomic.Int64
	errorUpdatedNano  atomic.Int64
	ttftUpdatedNano   atomic.Int64
}

func newAccountRuntimeHealthStats() *accountRuntimeHealthStats {
	return &accountRuntimeHealthStats{}
}

func (s *accountRuntimeHealthStats) loadOrCreate(accountID int64) *accountRuntimeHealthStat {
	if s == nil || accountID <= 0 {
		return nil
	}
	if value, ok := s.accounts.Load(accountID); ok {
		stat, _ := value.(*accountRuntimeHealthStat)
		if stat != nil {
			return stat
		}
	}

	stat := &accountRuntimeHealthStat{}
	stat.ttftEWMABits.Store(math.Float64bits(math.NaN()))
	actual, _ := s.accounts.LoadOrStore(accountID, stat)
	existing, _ := actual.(*accountRuntimeHealthStat)
	if existing != nil {
		return existing
	}
	return stat
}

func (s *accountRuntimeHealthStats) report(accountID int64, success bool, firstTokenMs *int) {
	if s == nil || accountID <= 0 {
		return
	}
	const alpha = 0.2
	stat := s.loadOrCreate(accountID)
	if stat == nil {
		return
	}
	stat.mu.Lock()
	defer stat.mu.Unlock()
	stat.sampleCount.Add(1)
	updatedNano := time.Now().UnixNano()
	stat.lastUpdatedNano.Store(updatedNano)
	stat.errorUpdatedNano.Store(updatedNano)

	errorSample := 1.0
	if success {
		errorSample = 0.0
	}
	updateEWMAAtomic(&stat.errorRateEWMABits, errorSample, alpha)

	if firstTokenMs == nil || *firstTokenMs <= 0 {
		return
	}
	stat.ttftSampleCount.Add(1)
	stat.ttftUpdatedNano.Store(updatedNano)
	ttft := float64(*firstTokenMs)
	ttftBits := math.Float64bits(ttft)
	for {
		oldBits := stat.ttftEWMABits.Load()
		oldValue := math.Float64frombits(oldBits)
		if math.IsNaN(oldValue) {
			if stat.ttftEWMABits.CompareAndSwap(oldBits, ttftBits) {
				return
			}
			continue
		}
		newValue := alpha*ttft + (1-alpha)*oldValue
		if stat.ttftEWMABits.CompareAndSwap(oldBits, math.Float64bits(newValue)) {
			return
		}
	}
}

// resetErrorHealth makes a recovered account unknown/default-healthy again.
// TTFT evidence is deliberately preserved.
func (s *accountRuntimeHealthStats) resetErrorHealth(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	value, ok := s.accounts.Load(accountID)
	if !ok {
		return
	}
	stat, _ := value.(*accountRuntimeHealthStat)
	if stat == nil {
		return
	}
	stat.mu.Lock()
	stat.errorRateEWMABits.Store(math.Float64bits(0))
	stat.sampleCount.Store(0)
	stat.errorUpdatedNano.Store(0)
	stat.mu.Unlock()
	s.unknownExploreAt.Delete(accountID)
	s.degradedRecoveryAt.Delete(accountID)
}

func (s *accountRuntimeHealthStats) snapshot(accountID int64) (errorRate float64, ttft float64, hasTTFT bool, found bool) {
	errorRate, ttft, hasTTFT, found, _, _, _ = s.snapshotWithMeta(accountID)
	return errorRate, ttft, hasTTFT, found
}

func (s *accountRuntimeHealthStats) snapshotWithMeta(accountID int64) (errorRate float64, ttft float64, hasTTFT bool, found bool, sampleCount int64, ttftSampleCount int64, lastUpdated time.Time) {
	errorRate, ttft, hasTTFT, found, sampleCount, ttftSampleCount, lastUpdated, _, _ = s.snapshotWithFreshnessMeta(accountID)
	return
}

func (s *accountRuntimeHealthStats) snapshotWithFreshnessMeta(accountID int64) (errorRate float64, ttft float64, hasTTFT bool, found bool, sampleCount int64, ttftSampleCount int64, lastUpdated, errorUpdated, ttftUpdated time.Time) {
	if s == nil || accountID <= 0 {
		return 0, 0, false, false, 0, 0, time.Time{}, time.Time{}, time.Time{}
	}
	value, ok := s.accounts.Load(accountID)
	if !ok {
		return 0, 0, false, false, 0, 0, time.Time{}, time.Time{}, time.Time{}
	}
	stat, _ := value.(*accountRuntimeHealthStat)
	if stat == nil {
		return 0, 0, false, false, 0, 0, time.Time{}, time.Time{}, time.Time{}
	}
	errorRate = clamp01(math.Float64frombits(stat.errorRateEWMABits.Load()))
	sampleCount = stat.sampleCount.Load()
	ttftSampleCount = stat.ttftSampleCount.Load()
	if updated := stat.lastUpdatedNano.Load(); updated > 0 {
		lastUpdated = time.Unix(0, updated)
	}
	if updated := stat.errorUpdatedNano.Load(); updated > 0 {
		errorUpdated = time.Unix(0, updated)
	} else {
		errorUpdated = lastUpdated
	}
	if updated := stat.ttftUpdatedNano.Load(); updated > 0 {
		ttftUpdated = time.Unix(0, updated)
	} else if ttftSampleCount > 0 {
		ttftUpdated = lastUpdated
	}
	ttftValue := math.Float64frombits(stat.ttftEWMABits.Load())
	if math.IsNaN(ttftValue) {
		return errorRate, 0, false, true, sampleCount, ttftSampleCount, lastUpdated, errorUpdated, ttftUpdated
	}
	return errorRate, ttftValue, true, true, sampleCount, ttftSampleCount, lastUpdated, errorUpdated, ttftUpdated
}

type accountHealthCandidate struct {
	item            accountWithLoad
	errorRate       float64
	ttft            float64
	ttftP90         float64
	hasTTFT         bool
	found           bool
	sampleCount     int64
	ttftSampleCount int64
	lastUpdated     time.Time
	errorUpdated    time.Time
	ttftUpdated     time.Time
	score           float64
}

func buildAccountHealthCandidates(accounts []accountWithLoad, stats *accountRuntimeHealthStats) []accountHealthCandidate {
	return buildAccountHealthCandidatesWithHistory(accounts, stats, nil)
}

// buildAccountHealthCandidatesWithHistory overlays persisted TTFT only for the
// current adaptive selection. Strict-priority callers use
// buildAccountHealthCandidates and therefore can never observe this seed data.
func buildAccountHealthCandidatesWithHistory(accounts []accountWithLoad, stats *accountRuntimeHealthStats, history map[int64]AccountTTFTHistory) []accountHealthCandidate {
	candidates := make([]accountHealthCandidate, 0, len(accounts))
	minTTFT := 0.0
	maxTTFT := 0.0
	hasAnyTTFT := false

	for _, item := range accounts {
		if item.account == nil {
			candidates = append(candidates, accountHealthCandidate{item: item, score: accountHealthUnknownScore})
			continue
		}
		errorRate, ttft, hasTTFT, found, sampleCount, ttftSampleCount, lastUpdated, errorUpdated, ttftUpdated := stats.snapshotWithFreshnessMeta(item.account.ID)
		ttftP90 := 0.0
		if summary, ok := validAccountTTFTHistory(history[item.account.ID]); ok {
			ttftP90 = summary.P90Ms
			if ttftSampleCount < accountHealthUnknownMinSamples {
				ttft = summary.P50Ms
				hasTTFT = true
				ttftSampleCount = summary.SampleCount
				ttftUpdated = summary.LatestAt
			}
			if lastUpdated.IsZero() || summary.LatestAt.After(lastUpdated) {
				lastUpdated = summary.LatestAt
			}
		}
		candidates = append(candidates, accountHealthCandidate{
			item:            item,
			errorRate:       errorRate,
			ttft:            ttft,
			ttftP90:         ttftP90,
			hasTTFT:         hasTTFT,
			found:           found,
			sampleCount:     sampleCount,
			ttftSampleCount: ttftSampleCount,
			lastUpdated:     lastUpdated,
			errorUpdated:    errorUpdated,
			ttftUpdated:     ttftUpdated,
			score:           accountHealthUnknownScore,
		})
		if hasTTFT {
			if !hasAnyTTFT || ttft < minTTFT {
				minTTFT = ttft
			}
			if !hasAnyTTFT || ttft > maxTTFT {
				maxTTFT = ttft
			}
			hasAnyTTFT = true
		}
	}

	for i := range candidates {
		if candidates[i].found {
			candidates[i].score = accountRuntimeHealthScore(candidates[i].errorRate, candidates[i].ttft, candidates[i].hasTTFT, minTTFT, maxTTFT, hasAnyTTFT)
		}
	}
	return candidates
}

func freshAccountHealthCandidates(candidates []accountHealthCandidate, now time.Time, policy adaptiveTTFTSwitchPolicy) []accountHealthCandidate {
	if len(candidates) == 0 {
		return candidates
	}
	profiles := make([]adaptiveAccountHealthProfile, len(candidates))
	for i, candidate := range candidates {
		profiles[i] = adaptiveAccountHealthProfile{
			account: candidate.item.account, errorRate: candidate.errorRate, errorSamples: candidate.sampleCount,
			p50: candidate.ttft, p90: candidate.ttftP90, hasTTFT: candidate.hasTTFT, ttftSamples: candidate.ttftSampleCount,
			errorUpdated: candidate.errorUpdated, ttftUpdated: candidate.ttftUpdated,
		}
	}
	profiles = freshAdaptiveHealthProfiles(profiles, now, policy)
	minTTFT, maxTTFT, hasAnyTTFT := 0.0, 0.0, false
	for i, profile := range profiles {
		candidates[i].errorRate = profile.errorRate
		candidates[i].sampleCount = profile.errorSamples
		candidates[i].ttft = profile.p50
		candidates[i].ttftP90 = profile.p90
		candidates[i].hasTTFT = profile.hasTTFT
		candidates[i].ttftSampleCount = profile.ttftSamples
		if profile.hasTTFT {
			if !hasAnyTTFT || profile.p50 < minTTFT {
				minTTFT = profile.p50
			}
			if !hasAnyTTFT || profile.p50 > maxTTFT {
				maxTTFT = profile.p50
			}
			hasAnyTTFT = true
		}
	}
	for i := range candidates {
		if candidates[i].sampleCount < accountHealthUnknownMinSamples && candidates[i].ttftSampleCount < accountHealthUnknownMinSamples {
			candidates[i].found = false
			candidates[i].score = accountHealthUnknownScore
			continue
		}
		candidates[i].score = accountRuntimeHealthScore(candidates[i].errorRate, candidates[i].ttft, candidates[i].hasTTFT, minTTFT, maxTTFT, hasAnyTTFT)
	}
	return candidates
}

func validAccountTTFTHistory(summary AccountTTFTHistory) (AccountTTFTHistory, bool) {
	return summary, summary.SampleCount > 0 && summary.P50Ms > 0
}

func accountRuntimeHealthScore(errorRate float64, ttft float64, hasTTFT bool, minTTFT float64, maxTTFT float64, hasTTFTSample bool) float64 {
	errorFactor := 1 - clamp01(errorRate)
	ttftFactor := accountHealthUnknownTTFTScore
	if hasTTFT {
		ttftFactor = 1
		if hasTTFTSample && maxTTFT > minTTFT {
			ttftSpread := minTTFT * 2
			if ttftSpread < 300 {
				ttftSpread = 300
			}
			ttftFactor = 1 - clamp01((ttft-minTTFT)/ttftSpread)
		}
	}
	score := accountHealthErrorWeight*errorFactor + accountHealthTTFTWeight*ttftFactor
	if !hasTTFT && score > accountHealthUnknownScore {
		return accountHealthUnknownScore
	}
	return score
}

func accountHealthHasKnownSamples(sampleCount int64, ttftSampleCount int64, errorRate float64) bool {
	if ttftSampleCount >= accountHealthUnknownMinSamples {
		return true
	}
	return sampleCount >= accountHealthUnknownMinSamples
}

// accountHealthCoverageSamples is deliberately separate from
// accountHealthHasKnownSamples. A request sample is enough to give an account
// coverage credit, while TTFT trust still depends on real first-token samples.
// Coverage must not be blocked just because a route has not produced a TTFT
// measurement yet.
func accountHealthCoverageSamples(sampleCount, ttftSampleCount int64) int64 {
	if ttftSampleCount > sampleCount {
		return ttftSampleCount
	}
	return sampleCount
}

func bestAccountHealthScore(candidates []accountHealthCandidate) float64 {
	bestScore := -1.0
	for _, candidate := range candidates {
		if candidate.score > bestScore {
			bestScore = candidate.score
		}
	}
	return bestScore
}

func (s *accountRuntimeHealthStats) shouldTriggerUnknownExploration() bool {
	if s == nil || accountHealthUnknownExploreEvery == 0 {
		return false
	}
	return s.selectionCounter.Add(1)%accountHealthUnknownExploreEvery == 0
}

// warmingUpTurn returns true at a bounded cadence per group. It is independent
// from unknown/degraded recovery so both adaptive modes give accounts with too
// few samples a fair initial evaluation opportunity.
func (s *accountRuntimeHealthStats) warmingUpTurn(groupID int64) bool {
	if s == nil {
		return false
	}
	value, _ := s.warmingUpCounter.LoadOrStore(groupID, &atomic.Uint64{})
	counter, _ := value.(*atomic.Uint64)
	return counter != nil && counter.Add(1)%10 == 0
}

func (s *accountRuntimeHealthStats) warmingUpDue(groupID, accountID int64, now time.Time) bool {
	if s == nil || accountID <= 0 {
		return false
	}
	key := accountHealthGroupProbeKey{groupID: groupID, accountID: accountID}
	if raw, ok := s.warmingUpAt.Load(key); ok {
		lastNano, _ := raw.(int64)
		if lastNano > 0 && now.Sub(time.Unix(0, lastNano)) < time.Minute {
			return false
		}
	}
	return true
}

func (s *accountRuntimeHealthStats) markWarmingUp(groupID, accountID int64, now time.Time) {
	if s == nil || accountID <= 0 {
		return
	}
	s.warmingUpAt.Store(accountHealthGroupProbeKey{groupID: groupID, accountID: accountID}, now.UnixNano())
}

func accountHealthProbeDue(probes *sync.Map, accountID int64, now time.Time, interval time.Duration) bool {
	if probes == nil || accountID <= 0 {
		return false
	}
	if raw, ok := probes.Load(accountID); ok {
		lastNano, _ := raw.(int64)
		if lastNano > 0 && now.Sub(time.Unix(0, lastNano)) < interval {
			return false
		}
	}
	return true
}

func accountHealthMarkProbe(probes *sync.Map, accountID int64, now time.Time) {
	if probes == nil || accountID <= 0 {
		return
	}
	probes.Store(accountID, now.UnixNano())
}

func (s *accountRuntimeHealthStats) unknownExplorationDue(accountID int64, now time.Time) bool {
	if s == nil {
		return false
	}
	return accountHealthProbeDue(&s.unknownExploreAt, accountID, now, accountHealthUnknownExploreCooldown)
}

func (s *accountRuntimeHealthStats) markUnknownExploration(accountID int64, now time.Time) {
	if s == nil {
		return
	}
	accountHealthMarkProbe(&s.unknownExploreAt, accountID, now)
}

func (s *accountRuntimeHealthStats) degradedRecoveryDue(accountID int64, now time.Time) bool {
	if s == nil {
		return false
	}
	return accountHealthProbeDue(&s.degradedRecoveryAt, accountID, now, accountHealthDegradedRecoveryDelay)
}

func (s *accountRuntimeHealthStats) markDegradedRecovery(accountID int64, now time.Time) {
	if s == nil {
		return
	}
	accountHealthMarkProbe(&s.degradedRecoveryAt, accountID, now)
}

func accountHealthSampleRecentlyUpdated(lastUpdated time.Time, now time.Time, interval time.Duration) bool {
	if lastUpdated.IsZero() || now.IsZero() || interval <= 0 {
		return false
	}
	return now.Sub(lastUpdated) < interval
}

func (s *accountRuntimeHealthStats) healthFirstProbeTurn(groupID int64) bool {
	if s == nil || accountHealthUnknownExploreEvery == 0 {
		return false
	}
	value, _ := s.healthFirstCounter.LoadOrStore(groupID, &atomic.Uint64{})
	counter, _ := value.(*atomic.Uint64)
	return counter != nil && counter.Add(1)%accountHealthUnknownExploreEvery == 0
}

func accountHealthAdaptiveRecoveryDelay(candidate accountHealthCandidate) time.Duration {
	delay := time.Minute
	switch {
	case candidate.errorRate >= 0.60:
		delay = 8 * time.Minute
	case candidate.errorRate >= 0.40:
		delay = 4 * time.Minute
	case candidate.errorRate >= 0.20:
		delay = 2 * time.Minute
	}
	if delay > 10*time.Minute {
		return 10 * time.Minute
	}
	return delay
}

func (s *accountRuntimeHealthStats) healthFirstProbeDue(groupID, accountID int64, now time.Time, delay time.Duration) bool {
	if s == nil || accountID <= 0 {
		return false
	}
	key := accountHealthGroupProbeKey{groupID: groupID, accountID: accountID}
	if raw, ok := s.healthFirstProbeAt.Load(key); ok {
		lastNano, _ := raw.(int64)
		if lastNano > 0 && now.Sub(time.Unix(0, lastNano)) < delay {
			return false
		}
	}
	return true
}

func (s *accountRuntimeHealthStats) markHealthFirstProbe(groupID, accountID int64, now time.Time) {
	if s == nil || accountID <= 0 {
		return
	}
	key := accountHealthGroupProbeKey{groupID: groupID, accountID: accountID}
	s.healthFirstProbeAt.Store(key, now.UnixNano())
}

func filterByAccountHealthBand(accounts []accountWithLoad, stats *accountRuntimeHealthStats) []accountWithLoad {
	if len(accounts) <= 1 || stats == nil {
		return accounts
	}

	candidates := buildAccountHealthCandidates(accounts, stats)
	bestScore := bestAccountHealthScore(candidates)
	if bestScore < 0 {
		return accounts
	}

	cutoff := bestScore - accountHealthScoreBandThreshold
	result := make([]accountWithLoad, 0, len(accounts))
	for _, candidate := range candidates {
		if candidate.item.account == nil || candidate.score >= cutoff {
			result = append(result, candidate.item)
		}
	}
	if len(result) == 0 {
		return accounts
	}
	return result
}
