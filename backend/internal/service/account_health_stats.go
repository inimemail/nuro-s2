package service

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const (
	accountHealthErrorCeiling             = 0.20
	accountHealthErrorGap                 = 0.15
	accountHealthFirstCostRatio           = 1.25
	accountHealthFirstCostAbsoluteGap     = 0.20
	accountHealthCostTierEpsilon          = 1e-9
	accountHealthSeriousP50Ratio          = 1.80
	accountHealthSeriousP90Ratio          = 2.0
	accountHealthSeriousTTFTAbsoluteGapMs = 1500.0
	accountHealthSeriousP90AbsoluteGapMs  = 2000.0
	accountHealthTTFTTieRatio             = 1.20
	accountHealthTTFTTieAbsoluteMs        = 100.0
	accountHealthP90TieRatio              = 1.25
	accountHealthP90TieAbsoluteMs         = 200.0
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
}

func adaptiveAccountPolicyIndexes(profiles []adaptiveAccountHealthProfile, costBalanced bool, now time.Time) []int {
	if len(profiles) == 0 {
		return nil
	}
	errorQualified := adaptiveErrorQualifiedIndexes(profiles)
	if len(errorQualified) == 0 {
		for i := range profiles {
			errorQualified = append(errorQualified, i)
		}
	}
	if costBalanced {
		return adaptiveLowestUsableCostTier(profiles, errorQualified, now)
	}
	return adaptiveHealthFirstCostBand(profiles, errorQualified, now)
}

func filterAdaptiveAccountsToPolicyTier(accounts, available []accountWithLoad, stats *accountRuntimeHealthStats, strategy string, now time.Time, history map[int64]AccountTTFTHistory) []accountWithLoad {
	if len(accounts) == 0 || len(available) == 0 || !IsAdaptiveHealthSchedulingStrategy(strategy) {
		return available
	}
	candidates := buildAccountHealthCandidatesWithHistory(accounts, stats, history)
	profiles := make([]adaptiveAccountHealthProfile, len(candidates))
	for i, candidate := range candidates {
		profiles[i] = adaptiveAccountHealthProfile{account: candidate.item.account, errorRate: candidate.errorRate, errorSamples: candidate.sampleCount, p50: candidate.ttft, p90: candidate.ttftP90, hasTTFT: candidate.hasTTFT, ttftSamples: candidate.ttftSampleCount}
	}
	indexes := adaptiveAccountPolicyIndexes(profiles, IsHealthCostBalancedSchedulingStrategy(strategy), now)
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
	remaining := append([]int(nil), indexes...)
	for len(remaining) > 0 {
		lowest := adaptiveLowestMultiplier(profiles, remaining, now)
		if lowest == math.MaxFloat64 {
			return remaining
		}
		band := make([]int, 0, len(remaining))
		next := make([]int, 0, len(remaining))
		for _, index := range remaining {
			rate, declared := accountEffectiveUpstreamMultiplier(profiles[index].account, now)
			if declared && rate <= lowest*accountHealthFirstCostRatio && rate-lowest <= accountHealthFirstCostAbsoluteGap {
				band = append(band, index)
			} else {
				next = append(next, index)
			}
		}
		if len(next) == 0 || !adaptiveTierIsSeriouslySlow(profiles, band, remaining) {
			return band
		}
		remaining = next
	}
	return indexes
}

func adaptiveLowestUsableCostTier(profiles []adaptiveAccountHealthProfile, indexes []int, now time.Time) []int {
	remaining := append([]int(nil), indexes...)
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
		if len(next) == 0 || !adaptiveTierIsSeriouslySlow(profiles, tier, remaining) {
			return tier
		}
		remaining = next
	}
	return indexes
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
		if !candidate.hasTTFT || candidate.p50 <= 0 {
			return false
		}
		candidateSlow := false
		for _, otherIndex := range all {
			alternative := profiles[otherIndex]
			if alternative.account == nil || !alternative.hasTTFT || alternative.p50 <= 0 {
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
	if candidateP50 <= 0 || alternativeP50 <= 0 || candidateP50 <= alternativeP50*accountHealthSeriousP50Ratio || candidateP50-alternativeP50 <= accountHealthSeriousTTFTAbsoluteGapMs {
		return false
	}
	if candidateP90 > 0 && alternativeP90 > 0 {
		return candidateP90 > alternativeP90*accountHealthSeriousP90Ratio && candidateP90-alternativeP90 > accountHealthSeriousP90AbsoluteGapMs
	}
	return true
}

func adaptiveTTFTMateriallyWorseInAnyDimension(candidateP50, candidateP90, alternativeP50, alternativeP90 float64) bool {
	p50Slow := candidateP50 > 0 && alternativeP50 > 0 && candidateP50 > alternativeP50*accountHealthSeriousP50Ratio && candidateP50-alternativeP50 > accountHealthSeriousTTFTAbsoluteGapMs
	p90Slow := candidateP90 > 0 && alternativeP90 > 0 && candidateP90 > alternativeP90*accountHealthSeriousP90Ratio && candidateP90-alternativeP90 > accountHealthSeriousP90AbsoluteGapMs
	return p50Slow || p90Slow
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

func adaptiveLatencyLess(aP50, aP90 float64, aKnown bool, bP50, bP90 float64, bKnown bool) (bool, bool) {
	if aKnown != bKnown {
		return aKnown, true
	}
	if !aKnown {
		return false, false
	}
	if adaptiveLatencyDifferenceIsMeaningful(aP50, bP50, accountHealthTTFTTieRatio, accountHealthTTFTTieAbsoluteMs) {
		return aP50 < bP50, true
	}
	if aP90 > 0 && bP90 > 0 && adaptiveLatencyDifferenceIsMeaningful(aP90, bP90, accountHealthP90TieRatio, accountHealthP90TieAbsoluteMs) {
		return aP90 < bP90, true
	}
	return false, false
}

func adaptiveLatencyDifferenceIsMeaningful(a, b, ratio, absoluteMs float64) bool {
	if a <= 0 || b <= 0 || a == b {
		return false
	}
	lower, higher := a, b
	if lower > higher {
		lower, higher = higher, lower
	}
	return higher > lower*ratio && higher-lower > absoluteMs
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
	errorRateEWMABits atomic.Uint64
	ttftEWMABits      atomic.Uint64
	sampleCount       atomic.Int64
	ttftSampleCount   atomic.Int64
	lastUpdatedNano   atomic.Int64
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
	stat.sampleCount.Add(1)
	stat.lastUpdatedNano.Store(time.Now().UnixNano())

	errorSample := 1.0
	if success {
		errorSample = 0.0
	}
	updateEWMAAtomic(&stat.errorRateEWMABits, errorSample, alpha)

	if firstTokenMs == nil || *firstTokenMs <= 0 {
		return
	}
	stat.ttftSampleCount.Add(1)
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

func (s *accountRuntimeHealthStats) snapshot(accountID int64) (errorRate float64, ttft float64, hasTTFT bool, found bool) {
	errorRate, ttft, hasTTFT, found, _, _, _ = s.snapshotWithMeta(accountID)
	return errorRate, ttft, hasTTFT, found
}

func (s *accountRuntimeHealthStats) snapshotWithMeta(accountID int64) (errorRate float64, ttft float64, hasTTFT bool, found bool, sampleCount int64, ttftSampleCount int64, lastUpdated time.Time) {
	if s == nil || accountID <= 0 {
		return 0, 0, false, false, 0, 0, time.Time{}
	}
	value, ok := s.accounts.Load(accountID)
	if !ok {
		return 0, 0, false, false, 0, 0, time.Time{}
	}
	stat, _ := value.(*accountRuntimeHealthStat)
	if stat == nil {
		return 0, 0, false, false, 0, 0, time.Time{}
	}
	errorRate = clamp01(math.Float64frombits(stat.errorRateEWMABits.Load()))
	sampleCount = stat.sampleCount.Load()
	ttftSampleCount = stat.ttftSampleCount.Load()
	if updated := stat.lastUpdatedNano.Load(); updated > 0 {
		lastUpdated = time.Unix(0, updated)
	}
	ttftValue := math.Float64frombits(stat.ttftEWMABits.Load())
	if math.IsNaN(ttftValue) {
		return errorRate, 0, false, true, sampleCount, ttftSampleCount, lastUpdated
	}
	return errorRate, ttftValue, true, true, sampleCount, ttftSampleCount, lastUpdated
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
		errorRate, ttft, hasTTFT, found, sampleCount, ttftSampleCount, lastUpdated := stats.snapshotWithMeta(item.account.ID)
		ttftP90 := 0.0
		if summary, ok := validAccountTTFTHistory(history[item.account.ID]); ok {
			ttftP90 = summary.P90Ms
			if ttftSampleCount < accountHealthUnknownMinSamples {
				ttft = summary.P50Ms
				hasTTFT = true
				ttftSampleCount = summary.SampleCount
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
