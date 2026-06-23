package service

import (
	"math"
	"sync"
	"sync/atomic"
)

const (
	// accountHealthScoreBandThreshold 是"健康相近视为同带"的容差。带越窄，
	// 速度/错误率差距越容易把更优账号排到前面；带越宽越偏向负载均衡。
	// 健康分满分 1.0，TTFT 维度权重见 accountHealthTTFTWeight，因此约 2x 的
	// 首 token 速度差即可跨带胜出，10%~30% 的抖动仍判为同带走均衡。
	accountHealthScoreBandThreshold = 0.12
	accountHealthUnknownScore       = 0.82
	accountHealthUnknownTTFTScore   = 0.78
	// 健康分权重：错误率 + 首 token(TTFT)。提高 TTFT 权重让"更丝滑/回复更快"
	// 的账号在同优先级里更容易被优先选中（仅作用于无粘性的 load-balance 选号层）。
	accountHealthErrorWeight = 0.55
	accountHealthTTFTWeight  = 0.45
)

type accountRuntimeHealthStats struct {
	accounts sync.Map
}

type accountRuntimeHealthStat struct {
	errorRateEWMABits atomic.Uint64
	ttftEWMABits      atomic.Uint64
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

	errorSample := 1.0
	if success {
		errorSample = 0.0
	}
	updateEWMAAtomic(&stat.errorRateEWMABits, errorSample, alpha)

	if firstTokenMs == nil || *firstTokenMs <= 0 {
		return
	}
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
	if s == nil || accountID <= 0 {
		return 0, 0, false, false
	}
	value, ok := s.accounts.Load(accountID)
	if !ok {
		return 0, 0, false, false
	}
	stat, _ := value.(*accountRuntimeHealthStat)
	if stat == nil {
		return 0, 0, false, false
	}
	errorRate = clamp01(math.Float64frombits(stat.errorRateEWMABits.Load()))
	ttftValue := math.Float64frombits(stat.ttftEWMABits.Load())
	if math.IsNaN(ttftValue) {
		return errorRate, 0, false, true
	}
	return errorRate, ttftValue, true, true
}

func filterByAccountHealthBand(accounts []accountWithLoad, stats *accountRuntimeHealthStats) []accountWithLoad {
	if len(accounts) <= 1 || stats == nil {
		return accounts
	}

	type healthCandidate struct {
		item      accountWithLoad
		errorRate float64
		ttft      float64
		hasTTFT   bool
		found     bool
		score     float64
	}

	candidates := make([]healthCandidate, 0, len(accounts))
	minTTFT := 0.0
	maxTTFT := 0.0
	hasAnyTTFT := false

	for _, item := range accounts {
		if item.account == nil {
			candidates = append(candidates, healthCandidate{item: item, score: accountHealthUnknownScore})
			continue
		}
		errorRate, ttft, hasTTFT, found := stats.snapshot(item.account.ID)
		candidates = append(candidates, healthCandidate{
			item:      item,
			errorRate: errorRate,
			ttft:      ttft,
			hasTTFT:   hasTTFT,
			found:     found,
			score:     accountHealthUnknownScore,
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

	bestScore := -1.0
	for i := range candidates {
		if candidates[i].found {
			errorFactor := 1 - clamp01(candidates[i].errorRate)
			ttftFactor := accountHealthUnknownTTFTScore
			if candidates[i].hasTTFT {
				ttftFactor = 1
				if hasAnyTTFT && maxTTFT > minTTFT {
					ttftSpread := minTTFT * 2
					if ttftSpread < 300 {
						ttftSpread = 300
					}
					ttftFactor = 1 - clamp01((candidates[i].ttft-minTTFT)/ttftSpread)
				}
			}
			candidates[i].score = accountHealthErrorWeight*errorFactor + accountHealthTTFTWeight*ttftFactor
		}
		if candidates[i].score > bestScore {
			bestScore = candidates[i].score
		}
	}
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
