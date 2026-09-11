package service

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	accountTTFTHistoryWindow       = time.Duration(DefaultAdaptiveHealthSampleFreshnessMinutes) * time.Minute
	accountTTFTHistoryTTL          = 5 * time.Minute
	accountTTFTHistoryStaleTTL     = 30 * time.Minute
	accountTTFTHistoryErrorTTL     = 30 * time.Second
	accountTTFTHistoryMaxErrorTTL  = 5 * time.Minute
	accountTTFTHistoryQueryTimeout = 300 * time.Millisecond
	accountTTFTHistorySampleLimit  = 50
)

// AccountTTFTHistory is an account-wide latency summary. GroupID is
// intentionally absent: an account's recent TTFT remains useful when the same
// account is selected through another group.
type AccountTTFTHistory struct {
	AccountID   int64
	SampleCount int64
	P50Ms       float64
	P90Ms       float64
	LatestAt    time.Time
}

type accountTTFTHistoryBatchReader interface {
	GetAccountTTFTHistoryBatch(ctx context.Context, accountIDs []int64, startTime time.Time, sampleLimit int) (map[int64]AccountTTFTHistory, error)
}

type accountTTFTHistoryCacheEntry struct {
	summary    AccountTTFTHistory
	expiresAt  time.Time
	staleUntil time.Time
	failures   int
}

type accountTTFTHistoryCacheKey struct {
	accountID int64
	window    time.Duration
}

type accountTTFTHistoryCache struct {
	reader  accountTTFTHistoryBatchReader
	mu      sync.RWMutex
	entries map[accountTTFTHistoryCacheKey]accountTTFTHistoryCacheEntry
	flight  singleflight.Group
}

func newAccountTTFTHistoryCache(repo UsageLogRepository) *accountTTFTHistoryCache {
	reader, _ := repo.(accountTTFTHistoryBatchReader)
	if reader == nil {
		return nil
	}
	return &accountTTFTHistoryCache{reader: reader, entries: make(map[accountTTFTHistoryCacheKey]accountTTFTHistoryCacheEntry)}
}

func (c *accountTTFTHistoryCache) load(ctx context.Context, accounts []*Account, now time.Time) map[int64]AccountTTFTHistory {
	return c.loadWithin(ctx, accounts, now, accountTTFTHistoryWindow)
}

func (c *accountTTFTHistoryCache) loadWithin(ctx context.Context, accounts []*Account, now time.Time, window time.Duration) map[int64]AccountTTFTHistory {
	if c == nil || c.reader == nil || len(accounts) == 0 {
		return nil
	}
	if window <= 0 {
		window = accountTTFTHistoryWindow
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ids := uniqueAccountIDs(accounts)
	if len(ids) == 0 {
		return nil
	}

	result := make(map[int64]AccountTTFTHistory, len(ids))
	missing := make([]int64, 0, len(ids))
	c.mu.RLock()
	for _, id := range ids {
		entry, ok := c.entries[accountTTFTHistoryCacheKey{accountID: id, window: window}]
		if ok && entry.expiresAt.After(now) {
			if accountTTFTHistoryIsFresh(entry.summary, now, window) {
				result[id] = entry.summary
			}
			continue
		}
		// Keep serving the last successful summary while it is refreshed. A
		// transient database failure must not make scheduling forget known TTFT.
		if ok && entry.staleUntil.After(now) && accountTTFTHistoryIsFresh(entry.summary, now, window) {
			result[id] = entry.summary
		}
		missing = append(missing, id)
	}
	c.mu.RUnlock()
	if len(missing) == 0 {
		return result
	}

	key := accountTTFTHistoryFlightKey(missing, window)
	loaded, err, _ := c.flight.Do(key, func() (any, error) {
		queryCtx, cancel := context.WithTimeout(ctx, accountTTFTHistoryQueryTimeout)
		defer cancel()
		return c.reader.GetAccountTTFTHistoryBatch(queryCtx, missing, now.Add(-window), accountTTFTHistorySampleLimit)
	})
	if err != nil {
		c.storeRefreshFailure(missing, now, window, result)
		return result
	}
	summaries, _ := loaded.(map[int64]AccountTTFTHistory)
	c.mu.Lock()
	c.pruneExpiredLocked(now)
	for _, id := range missing {
		summary := summaries[id]
		if !accountTTFTHistoryIsFresh(summary, now, window) {
			summary = AccountTTFTHistory{}
		}
		c.entries[accountTTFTHistoryCacheKey{accountID: id, window: window}] = accountTTFTHistoryCacheEntry{
			summary:    summary,
			expiresAt:  now.Add(accountTTFTHistoryTTL),
			staleUntil: now.Add(accountTTFTHistoryStaleTTL),
		}
		if summary.SampleCount > 0 {
			result[id] = summary
		}
	}
	c.mu.Unlock()
	return result
}

func (c *accountTTFTHistoryCache) storeRefreshFailure(ids []int64, now time.Time, window time.Duration, stale map[int64]AccountTTFTHistory) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		key := accountTTFTHistoryCacheKey{accountID: id, window: window}
		entry := c.entries[key]
		entry.failures++
		retryAfter := accountTTFTHistoryRetryDelay(entry.failures)
		if _, ok := stale[id]; ok && entry.staleUntil.After(now) && accountTTFTHistoryIsFresh(entry.summary, now, window) {
			entry.expiresAt = now.Add(retryAfter)
			c.entries[key] = entry
			continue
		}
		c.entries[key] = accountTTFTHistoryCacheEntry{expiresAt: now.Add(retryAfter), failures: entry.failures}
	}
	c.pruneExpiredLocked(now)
}

func (c *accountTTFTHistoryCache) pruneExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if !entry.expiresAt.After(now) && !entry.staleUntil.After(now) {
			delete(c.entries, key)
		}
	}
}

func accountTTFTHistoryIsFresh(summary AccountTTFTHistory, now time.Time, window time.Duration) bool {
	if summary.SampleCount <= 0 || summary.P50Ms <= 0 {
		return false
	}
	// The repository query itself is bounded by startTime. Keep compatibility
	// with readers/tests that do not populate LatestAt while still rejecting a
	// timestamped summary that is outside the requested window.
	if summary.LatestAt.IsZero() {
		return true
	}
	return now.IsZero() || window <= 0 || summary.LatestAt.After(now.Add(-window))
}

func accountTTFTHistoryRetryDelay(failures int) time.Duration {
	if failures <= 1 {
		return accountTTFTHistoryErrorTTL
	}
	delay := accountTTFTHistoryErrorTTL
	for i := 1; i < failures && delay < accountTTFTHistoryMaxErrorTTL; i++ {
		delay *= 2
	}
	if delay > accountTTFTHistoryMaxErrorTTL {
		return accountTTFTHistoryMaxErrorTTL
	}
	return delay
}

func uniqueAccountIDs(accounts []*Account) []int64 {
	seen := make(map[int64]struct{}, len(accounts))
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		if account == nil || account.ID <= 0 {
			continue
		}
		if _, ok := seen[account.ID]; ok {
			continue
		}
		seen[account.ID] = struct{}{}
		ids = append(ids, account.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func accountTTFTHistoryFlightKey(ids []int64, windows ...time.Duration) string {
	var b strings.Builder
	if len(windows) > 0 {
		b.WriteString(strconv.FormatInt(int64(windows[0]/time.Second), 10))
		b.WriteByte(':')
	}
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(id, 10))
	}
	return b.String()
}
