package service

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const invalidAuthAbuseShardCount = 16

type invalidAuthAbuseEntry struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
}

type invalidAuthAbuseShard struct {
	mu      sync.Mutex
	entries map[string]*invalidAuthAbuseEntry
}

type invalidAuthAbuseLimiter struct {
	threshold     int
	window        time.Duration
	block         time.Duration
	capacity      int64
	shards        [invalidAuthAbuseShardCount]invalidAuthAbuseShard
	now           func() time.Time
	tracked       atomic.Int64
	recorded      atomic.Uint64
	blocks        atomic.Uint64
	rejected      atomic.Uint64
	expiredCount  atomic.Uint64
	overflowed    atomic.Uint64
	cleanupNext   atomic.Int64
	cleanupCursor atomic.Uint32
}

type InvalidAuthAbuseHealth struct {
	Enabled       bool   `json:"enabled"`
	Tracked       int64  `json:"tracked"`
	Capacity      int64  `json:"capacity"`
	Recorded      uint64 `json:"recorded"`
	Blocks        uint64 `json:"blocks"`
	Rejected      uint64 `json:"rejected"`
	Expired       uint64 `json:"expired"`
	Overflowed    uint64 `json:"overflowed"`
	GlobalBlocked uint64 `json:"global_blocked"` // Deprecated: always zero.
}

func newInvalidAuthAbuseLimiter(cfg *config.Config) *invalidAuthAbuseLimiter {
	if cfg == nil || !cfg.APIKeyAuth.InvalidAbuse.Enabled {
		return nil
	}
	c := cfg.APIKeyAuth.InvalidAbuse
	if c.Threshold <= 0 || c.WindowSeconds <= 0 || c.BlockSeconds <= 0 || c.Capacity <= 0 {
		return nil
	}
	l := &invalidAuthAbuseLimiter{
		threshold: c.Threshold,
		window:    time.Duration(c.WindowSeconds) * time.Second,
		block:     time.Duration(c.BlockSeconds) * time.Second,
		capacity:  int64(c.Capacity),
		now:       time.Now,
	}
	for i := range l.shards {
		l.shards[i].entries = make(map[string]*invalidAuthAbuseEntry)
	}
	return l
}

func (s *APIKeyService) CheckInvalidAuthAbuse(clientKey string) (time.Duration, bool) {
	if s == nil || s.invalidAuthAbuse == nil || clientKey == "" {
		return 0, false
	}
	now := s.invalidAuthAbuse.now()
	s.invalidAuthAbuse.maybeCleanupAtCapacity(now)
	shard := s.invalidAuthAbuse.shard(clientKey)
	shard.mu.Lock()
	e := shard.entries[clientKey]
	if s.invalidAuthAbuse.expired(e, now) {
		delete(shard.entries, clientKey)
		s.invalidAuthAbuse.tracked.Add(-1)
		s.invalidAuthAbuse.expiredCount.Add(1)
		e = nil
	}
	if e != nil && e.blockedUntil.After(now) {
		retry := e.blockedUntil.Sub(now)
		shard.mu.Unlock()
		s.invalidAuthAbuse.rejected.Add(1)
		return retry, true
	}
	shard.mu.Unlock()
	return 0, false
}

func (s *APIKeyService) RecordInvalidAuthFailure(clientKey string) {
	if s == nil || s.invalidAuthAbuse == nil || clientKey == "" {
		return
	}
	l := s.invalidAuthAbuse
	l.recorded.Add(1)
	now := l.now()
	l.maybeCleanupAtCapacity(now)
	shard := l.shard(clientKey)
	shard.mu.Lock()
	e := shard.entries[clientKey]
	if e != nil && l.expired(e, now) {
		delete(shard.entries, clientKey)
		l.tracked.Add(-1)
		l.expiredCount.Add(1)
		e = nil
	}
	if e == nil {
		if !l.reserveEntry() {
			shard.mu.Unlock()
			l.overflowed.Add(1)
			return
		}
		e = &invalidAuthAbuseEntry{windowStart: now}
		shard.entries[clientKey] = e
	}
	if e.blockedUntil.After(now) {
		shard.mu.Unlock()
		return
	}
	if !now.Before(e.windowStart.Add(l.window)) {
		e.windowStart = now
		e.failures = 0
	}
	e.failures++
	if e.failures >= l.threshold {
		e.failures = 0
		e.blockedUntil = now.Add(l.block)
		e.windowStart = e.blockedUntil
		l.blocks.Add(1)
	}
	shard.mu.Unlock()
}

func (l *invalidAuthAbuseLimiter) reserveEntry() bool {
	for {
		current := l.tracked.Load()
		if current >= l.capacity {
			return false
		}
		if l.tracked.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (l *invalidAuthAbuseLimiter) maybeCleanupAtCapacity(now time.Time) {
	if l.tracked.Load() < l.capacity {
		return
	}
	nowUnixNano := now.UnixNano()
	for {
		next := l.cleanupNext.Load()
		if nowUnixNano < next {
			return
		}
		if l.cleanupNext.CompareAndSwap(next, now.Add(100*time.Millisecond).UnixNano()) {
			break
		}
	}
	index := l.cleanupCursor.Add(1) - 1
	shard := &l.shards[index%invalidAuthAbuseShardCount]
	shard.mu.Lock()
	for key, entry := range shard.entries {
		if l.expired(entry, now) {
			delete(shard.entries, key)
			l.tracked.Add(-1)
			l.expiredCount.Add(1)
		}
	}
	shard.mu.Unlock()
}

func (l *invalidAuthAbuseLimiter) expired(e *invalidAuthAbuseEntry, now time.Time) bool {
	return e != nil && !e.blockedUntil.After(now) && !e.windowStart.After(now) && !now.Before(e.windowStart.Add(l.window))
}

func (l *invalidAuthAbuseLimiter) shard(key string) *invalidAuthAbuseShard {
	h := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return &l.shards[h%invalidAuthAbuseShardCount]
}

func (s *APIKeyService) InvalidAuthAbuseHealth() InvalidAuthAbuseHealth {
	if s == nil || s.invalidAuthAbuse == nil {
		return InvalidAuthAbuseHealth{}
	}
	l := s.invalidAuthAbuse
	return InvalidAuthAbuseHealth{Enabled: true, Tracked: l.tracked.Load(), Capacity: l.capacity, Recorded: l.recorded.Load(), Blocks: l.blocks.Load(), Rejected: l.rejected.Load(), Expired: l.expiredCount.Load(), Overflowed: l.overflowed.Load()}
}
