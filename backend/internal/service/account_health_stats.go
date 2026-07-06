package service

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// accountHealthScoreBandThreshold 是"健康相近视为同带"的容差。带越窄，
	// 速度/错误率差距越容易把更优账号排到前面；带越宽越偏向负载均衡。
	// 健康分满分 1.0，TTFT 维度权重见 accountHealthTTFTWeight，因此约 2x 的
	// 首 token 速度差即可跨带胜出，秒级账号之间约 20%~35% 的抖动仍判为同带走均衡。
	accountHealthScoreBandThreshold           = 0.10
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

type accountRuntimeHealthStats struct {
	accounts           sync.Map
	selectionCounter   atomic.Uint64
	unknownExploreAt   sync.Map
	degradedRecoveryAt sync.Map
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
	hasTTFT         bool
	found           bool
	sampleCount     int64
	ttftSampleCount int64
	lastUpdated     time.Time
	score           float64
}

func buildAccountHealthCandidates(accounts []accountWithLoad, stats *accountRuntimeHealthStats) []accountHealthCandidate {
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
		candidates = append(candidates, accountHealthCandidate{
			item:            item,
			errorRate:       errorRate,
			ttft:            ttft,
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
	return sampleCount >= accountHealthUnknownMinSamples && errorRate > 0
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
