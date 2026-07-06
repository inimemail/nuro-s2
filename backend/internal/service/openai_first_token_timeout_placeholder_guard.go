package service

import (
	"strings"
	"sync"
	"time"
)

const (
	openAIFirstTokenTimeoutPlaceholderGuardStateTTL        = 30 * time.Minute
	openAIFirstTokenTimeoutPlaceholderGuardRecoverySamples = 2
)

type openAIFirstTokenTimeoutPlaceholderGuardKey struct {
	accountID int64
	model     string
}

type openAIFirstTokenTimeoutPlaceholderGuardState struct {
	mu              sync.Mutex
	blocked         bool
	fastStreak      int
	lastRealTokenMS int
	lastLimitMS     int
	lastUpdatedAt   time.Time
}

type openAIFirstTokenTimeoutPlaceholderGuard struct {
	states sync.Map // key: openAIFirstTokenTimeoutPlaceholderGuardKey, value: *openAIFirstTokenTimeoutPlaceholderGuardState
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) allow(accountID int64, model string, limitMS int, now time.Time) bool {
	if g == nil || accountID <= 0 || limitMS <= 0 {
		return true
	}
	key := openAIFirstTokenTimeoutPlaceholderGuardKey{
		accountID: accountID,
		model:     normalizeOpenAIFirstTokenTimeoutPlaceholderGuardModel(model),
	}
	raw, ok := g.states.Load(key)
	if !ok {
		return true
	}
	state, ok := raw.(*openAIFirstTokenTimeoutPlaceholderGuardState)
	if !ok || state == nil {
		g.states.Delete(key)
		return true
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if !state.lastUpdatedAt.IsZero() && now.Sub(state.lastUpdatedAt) > openAIFirstTokenTimeoutPlaceholderGuardStateTTL {
		g.states.Delete(key)
		return true
	}
	if state.lastRealTokenMS > limitMS {
		state.blocked = true
		state.fastStreak = 0
		state.lastLimitMS = limitMS
		state.lastUpdatedAt = now
		return false
	}
	if state.blocked && state.lastLimitMS != limitMS && state.lastRealTokenMS > 0 && state.lastRealTokenMS <= limitMS {
		state.blocked = false
		state.fastStreak = openAIFirstTokenTimeoutPlaceholderGuardRecoverySamples
		state.lastLimitMS = limitMS
		state.lastUpdatedAt = now
		return true
	}
	return !state.blocked
}

func (g *openAIFirstTokenTimeoutPlaceholderGuard) record(accountID int64, model string, realTokenMS int, limitMS int, now time.Time) {
	if g == nil || accountID <= 0 || realTokenMS <= 0 || limitMS <= 0 {
		return
	}
	key := openAIFirstTokenTimeoutPlaceholderGuardKey{
		accountID: accountID,
		model:     normalizeOpenAIFirstTokenTimeoutPlaceholderGuardModel(model),
	}
	raw, _ := g.states.LoadOrStore(key, &openAIFirstTokenTimeoutPlaceholderGuardState{})
	state, ok := raw.(*openAIFirstTokenTimeoutPlaceholderGuardState)
	if !ok || state == nil {
		g.states.Store(key, &openAIFirstTokenTimeoutPlaceholderGuardState{
			lastRealTokenMS: realTokenMS,
			lastLimitMS:     limitMS,
			lastUpdatedAt:   now,
		})
		return
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if !state.lastUpdatedAt.IsZero() && now.Sub(state.lastUpdatedAt) > openAIFirstTokenTimeoutPlaceholderGuardStateTTL {
		state.blocked = false
		state.fastStreak = 0
	}
	state.lastRealTokenMS = realTokenMS
	state.lastLimitMS = limitMS
	state.lastUpdatedAt = now
	if realTokenMS > limitMS {
		state.blocked = true
		state.fastStreak = 0
		return
	}
	if state.blocked {
		state.fastStreak++
		if state.fastStreak >= openAIFirstTokenTimeoutPlaceholderGuardRecoverySamples {
			state.blocked = false
		}
		return
	}
	state.fastStreak = openAIFirstTokenTimeoutPlaceholderGuardRecoverySamples
}

func normalizeOpenAIFirstTokenTimeoutPlaceholderGuardModel(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return "*"
	}
	return normalized
}
