package service

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/runtimeops"
)

type schedulerLocalSnapshotEntry struct {
	accounts  []Account
	expiresAt time.Time
	version   uint64
}

type schedulerLocalSnapshotOrderEntry struct {
	key     string
	version uint64
}

type SchedulerLocalSnapshot struct {
	enabled              bool
	ttl                  time.Duration
	maxKeys              int
	targetedInvalidation bool

	mu             sync.RWMutex
	buckets        map[string]schedulerLocalSnapshotEntry
	order          []schedulerLocalSnapshotOrderEntry
	accountBuckets map[int64]map[string]struct{}
	version        uint64

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
		enabled:              cfg.LocalSnapshotEnabled,
		ttl:                  ttl,
		maxKeys:              maxKeys,
		targetedInvalidation: cfg.SnapshotTargetedInvalidationEnabled,
		buckets:              make(map[string]schedulerLocalSnapshotEntry),
		accountBuckets:       make(map[int64]map[string]struct{}),
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
		runtimeops.ObserveSchedulerSnapshotMiss()
		if ok {
			s.Delete(bucket)
		}
		return nil, false
	}
	s.hits.Add(1)
	runtimeops.ObserveSchedulerSnapshotHit()
	// Set owns a fully detached immutable snapshot. Request paths only need their
	// own Account values for scalar overlays (for example the group-scoped guard
	// result); nested maps and slices are read-only on scheduler paths.
	return cloneSchedulerAccountViews(entry.accounts), true
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
	s.version++
	entry.version = s.version
	if s.accountBuckets == nil {
		s.accountBuckets = make(map[int64]map[string]struct{})
	}
	if previous, ok := s.buckets[key]; ok {
		s.removeAccountIndexLocked(key, previous.accounts)
	}
	s.buckets[key] = entry
	for _, account := range entry.accounts {
		if account.ID <= 0 {
			continue
		}
		keys := s.accountBuckets[account.ID]
		if keys == nil {
			keys = make(map[string]struct{})
			s.accountBuckets[account.ID] = keys
		}
		keys[key] = struct{}{}
	}
	s.order = append(s.order, schedulerLocalSnapshotOrderEntry{key: key, version: entry.version})
	s.evictLocked()
	s.mu.Unlock()
}

func (s *SchedulerLocalSnapshot) Delete(bucket SchedulerBucket) {
	if s == nil {
		return
	}
	key := bucket.String()
	s.mu.Lock()
	if entry, ok := s.buckets[key]; ok {
		s.removeAccountIndexLocked(key, entry.accounts)
		delete(s.buckets, key)
	}
	s.compactOrderLocked()
	s.mu.Unlock()
}

func (s *SchedulerLocalSnapshot) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.buckets = make(map[string]schedulerLocalSnapshotEntry)
	s.order = nil
	s.accountBuckets = make(map[int64]map[string]struct{})
	s.mu.Unlock()
}

// InvalidateAccount removes only buckets that currently contain accountID.
// It returns false when the reverse index cannot prove the affected buckets,
// allowing callers to fall back to a full clear for correctness.
func (s *SchedulerLocalSnapshot) InvalidateAccount(accountID int64) bool {
	if s == nil || accountID <= 0 {
		return false
	}
	s.mu.Lock()
	keys, ok := s.accountBuckets[accountID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	complete := true
	for key := range keys {
		if entry, exists := s.buckets[key]; exists {
			s.removeAccountIndexLocked(key, entry.accounts)
			delete(s.buckets, key)
		} else {
			complete = false
		}
	}
	if !complete {
		// A stale reverse index cannot prove that all affected buckets were
		// removed. Fall back to the conservative full invalidation path.
		s.buckets = make(map[string]schedulerLocalSnapshotEntry)
		s.order = nil
		s.accountBuckets = make(map[int64]map[string]struct{})
		s.mu.Unlock()
		return false
	}
	s.compactOrderLocked()
	s.mu.Unlock()
	return true
}

func (s *SchedulerLocalSnapshot) removeAccountIndexLocked(key string, accounts []Account) {
	for _, account := range accounts {
		keys := s.accountBuckets[account.ID]
		if keys == nil {
			continue
		}
		delete(keys, key)
		if len(keys) == 0 {
			delete(s.accountBuckets, account.ID)
		}
	}
}

func (s *SchedulerLocalSnapshot) ApplyEvent(_ context.Context, event SchedulerEvent) {
	if !s.Enabled() {
		return
	}
	switch event.Type {
	case SchedulerEventSnapshotUpdated, SchedulerEventSnapshotDeleted:
		s.Delete(event.Bucket)
	case SchedulerEventAccountRuntimeCleared, SchedulerEventAccountRuntimeOnlyCleared, SchedulerEventAccountDeleted:
		if s.targetedInvalidation && event.AccountID > 0 && s.InvalidateAccount(event.AccountID) {
			return
		}
		s.Clear()
	case SchedulerEventAccountUpdated:
		// Ordinary account updates may change group membership or model support;
		// their event does not carry the new bucket set, so retain the conservative
		// full clear. Runtime-only updates have stable membership and can target
		// the reverse index safely.
		if s.targetedInvalidation && event.AccountID > 0 && isSchedulerRuntimeAccountUpdateReason(event.Reason) && s.InvalidateAccount(event.AccountID) {
			return
		}
		s.Clear()
	}
}

func isSchedulerRuntimeAccountUpdateReason(reason string) bool {
	switch reason {
	case "runtime_block", "soft_cooldown", "probe_backoff", "grok_rate_limit_recovered":
		return true
	default:
		return false
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
		s.accountBuckets = make(map[int64]map[string]struct{})
		s.order = nil
		return
	}
	for len(s.buckets) > s.maxKeys && len(s.order) > 0 {
		ordered := s.order[0]
		s.order = s.order[1:]
		if current, ok := s.buckets[ordered.key]; ok && current.version == ordered.version {
			s.removeAccountIndexLocked(ordered.key, current.accounts)
			delete(s.buckets, ordered.key)
		}
	}
	s.compactOrderLocked()
}

func (s *SchedulerLocalSnapshot) compactOrderLocked() {
	limit := s.maxKeys * 2
	if limit < 64 {
		limit = 64
	}
	if len(s.order) <= limit {
		return
	}
	compacted := make([]schedulerLocalSnapshotOrderEntry, 0, len(s.buckets))
	for _, ordered := range s.order {
		if current, ok := s.buckets[ordered.key]; ok && current.version == ordered.version {
			compacted = append(compacted, ordered)
		}
	}
	s.order = compacted
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

// cloneSchedulerAccountViews creates the request-level COW view. It copies the
// compact Account structs while retaining immutable nested snapshot data. Any
// future request path that needs to mutate Credentials, Extra, or a nested
// collection must explicitly use cloneSchedulerAccount first.
func cloneSchedulerAccountViews(accounts []Account) []Account {
	if len(accounts) == 0 {
		return []Account{}
	}
	return append([]Account(nil), accounts...)
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
		if in[i].UpstreamBillingGuardOverrideMaxMultiplier != nil {
			value := *in[i].UpstreamBillingGuardOverrideMaxMultiplier
			out[i].UpstreamBillingGuardOverrideMaxMultiplier = &value
		}
		if in[i].GroupUpstreamBillingGuardMaxMultiplier != nil {
			value := *in[i].GroupUpstreamBillingGuardMaxMultiplier
			out[i].GroupUpstreamBillingGuardMaxMultiplier = &value
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
