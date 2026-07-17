package service

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

type schedulerLocalSnapshotEntry struct {
	accounts  []Account
	expiresAt time.Time
}

type SchedulerLocalSnapshot struct {
	enabled bool
	ttl     time.Duration
	maxKeys int

	mu      sync.RWMutex
	buckets map[string]schedulerLocalSnapshotEntry
	order   []string

	hits   atomic.Int64
	misses atomic.Int64
}

type SchedulerLocalSnapshotStats struct {
	Enabled bool
	Keys    int
	Hits    int64
	Misses  int64
}

func NewSchedulerLocalSnapshot(cfg config.GatewaySchedulingConfig) *SchedulerLocalSnapshot {
	ttl := time.Duration(cfg.LocalSnapshotTTLMS) * time.Millisecond
	if ttl < 0 {
		ttl = 0
	}
	maxKeys := cfg.LocalSnapshotMaxKeys
	if maxKeys < 0 {
		maxKeys = 0
	}
	return &SchedulerLocalSnapshot{
		enabled: cfg.LocalSnapshotEnabled,
		ttl:     ttl,
		maxKeys: maxKeys,
		buckets: make(map[string]schedulerLocalSnapshotEntry),
	}
}

func (s *SchedulerLocalSnapshot) Enabled() bool {
	return s != nil && s.enabled && s.ttl > 0 && s.maxKeys != 0
}

func (s *SchedulerLocalSnapshot) Get(bucket SchedulerBucket, now time.Time) ([]Account, bool) {
	if !s.Enabled() {
		return nil, false
	}
	key := bucket.String()
	s.mu.RLock()
	entry, ok := s.buckets[key]
	s.mu.RUnlock()
	if !ok || (!entry.expiresAt.IsZero() && !now.Before(entry.expiresAt)) {
		s.misses.Add(1)
		if ok {
			s.Delete(bucket)
		}
		return nil, false
	}
	s.hits.Add(1)
	return cloneAccounts(entry.accounts), true
}

func (s *SchedulerLocalSnapshot) Set(bucket SchedulerBucket, accounts []Account, now time.Time) {
	if !s.Enabled() {
		return
	}
	key := bucket.String()
	entry := schedulerLocalSnapshotEntry{
		accounts:  cloneAccounts(accounts),
		expiresAt: now.Add(s.ttl),
	}

	s.mu.Lock()
	if _, exists := s.buckets[key]; !exists {
		s.order = append(s.order, key)
	}
	s.buckets[key] = entry
	s.evictLocked()
	s.mu.Unlock()
}

func (s *SchedulerLocalSnapshot) Delete(bucket SchedulerBucket) {
	if s == nil {
		return
	}
	key := bucket.String()
	s.mu.Lock()
	delete(s.buckets, key)
	for i, orderedKey := range s.order {
		if orderedKey == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
}

func (s *SchedulerLocalSnapshot) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.buckets = make(map[string]schedulerLocalSnapshotEntry)
	s.order = nil
	s.mu.Unlock()
}

func (s *SchedulerLocalSnapshot) ApplyEvent(_ context.Context, event SchedulerEvent) {
	if !s.Enabled() {
		return
	}
	switch event.Type {
	case SchedulerEventSnapshotUpdated, SchedulerEventSnapshotDeleted:
		s.Delete(event.Bucket)
	case SchedulerEventAccountUpdated, SchedulerEventAccountDeleted:
		s.Clear()
	}
}

func (s *SchedulerLocalSnapshot) Stats() SchedulerLocalSnapshotStats {
	stats := SchedulerLocalSnapshotStats{}
	if s == nil {
		return stats
	}
	s.mu.RLock()
	keys := len(s.buckets)
	s.mu.RUnlock()
	stats.Enabled = s.Enabled()
	stats.Keys = keys
	stats.Hits = s.hits.Load()
	stats.Misses = s.misses.Load()
	return stats
}

func (s *SchedulerLocalSnapshot) evictLocked() {
	if s.maxKeys <= 0 {
		for key := range s.buckets {
			delete(s.buckets, key)
		}
		s.order = nil
		return
	}
	for len(s.buckets) > s.maxKeys && len(s.order) > 0 {
		key := s.order[0]
		s.order = s.order[1:]
		if _, ok := s.buckets[key]; ok {
			delete(s.buckets, key)
		}
	}
}

func cloneAccounts(accounts []Account) []Account {
	if len(accounts) == 0 {
		return []Account{}
	}
	out := make([]Account, len(accounts))
	for i := range accounts {
		out[i] = cloneSchedulerAccount(accounts[i])
	}
	return out
}

func cloneSchedulerAccount(account Account) Account {
	cloned := account
	cloned.Credentials = cloneStringAnyMap(account.Credentials)
	cloned.Extra = cloneStringAnyMap(account.Extra)
	cloned.GroupIDs = append([]int64(nil), account.GroupIDs...)
	cloned.AccountGroups = cloneSchedulerAccountGroups(account.AccountGroups)
	cloned.Groups = append([]*Group(nil), account.Groups...)
	cloned.LastUsedAt = cloneTimePtr(account.LastUsedAt)
	cloned.ExpiresAt = cloneTimePtr(account.ExpiresAt)
	cloned.RateLimitedAt = cloneTimePtr(account.RateLimitedAt)
	cloned.RateLimitResetAt = cloneTimePtr(account.RateLimitResetAt)
	cloned.OverloadUntil = cloneTimePtr(account.OverloadUntil)
	cloned.UpstreamBillingGuardObservedMultiplier = cloneFloatPtr(account.UpstreamBillingGuardObservedMultiplier)
	cloned.UpstreamBillingGuardEvaluatedAt = cloneTimePtr(account.UpstreamBillingGuardEvaluatedAt)
	cloned.OpenAIPoolSoftCooldownUntil = cloneTimePtr(account.OpenAIPoolSoftCooldownUntil)
	cloned.AnthropicPoolSoftCooldownUntil = cloneTimePtr(account.AnthropicPoolSoftCooldownUntil)
	cloned.SessionWindowStart = cloneTimePtr(account.SessionWindowStart)
	cloned.SessionWindowEnd = cloneTimePtr(account.SessionWindowEnd)
	if account.RateMultiplier != nil {
		value := *account.RateMultiplier
		cloned.RateMultiplier = &value
	}
	if account.LoadFactor != nil {
		value := *account.LoadFactor
		cloned.LoadFactor = &value
	}
	if account.ProxyID != nil {
		value := *account.ProxyID
		cloned.ProxyID = &value
	}
	if account.ProxyFallbackOriginID != nil {
		value := *account.ProxyFallbackOriginID
		cloned.ProxyFallbackOriginID = &value
	}
	if account.Notes != nil {
		value := *account.Notes
		cloned.Notes = &value
	}
	if account.ProxyFallbackOriginName != nil {
		value := *account.ProxyFallbackOriginName
		cloned.ProxyFallbackOriginName = &value
	}
	cloned.modelMappingCache = nil
	cloned.modelMappingCacheReady = false
	cloned.modelMappingCacheCredentialsPtr = 0
	cloned.modelMappingCacheRawPtr = 0
	cloned.modelMappingCacheRawLen = 0
	cloned.modelMappingCacheRawSig = 0
	cloned.headerOverrideCache = nil
	cloned.headerOverrideCacheReady = false
	cloned.headerOverrideCacheCredentialsPtr = 0
	cloned.headerOverrideCacheRawPtr = 0
	cloned.headerOverrideCacheRawLen = 0
	cloned.headerOverrideCacheRawSig = 0
	return cloned
}

func cloneSchedulerAccountGroups(in []AccountGroup) []AccountGroup {
	if len(in) == 0 {
		return nil
	}
	out := make([]AccountGroup, len(in))
	for i := range in {
		out[i] = in[i]
		if in[i].UpstreamBillingGuardMaxMultiplier != nil {
			value := *in[i].UpstreamBillingGuardMaxMultiplier
			out[i].UpstreamBillingGuardMaxMultiplier = &value
		}
	}
	return out
}

func cloneFloatPtr(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneStringAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = cloneAnyValue(value)
	}
	return out
}

func cloneAnyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneStringAnyMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = cloneAnyValue(typed[i])
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	case []int64:
		return append([]int64(nil), typed...)
	default:
		return value
	}
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
