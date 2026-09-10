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
	accountTTFTHistoryWindow       = 24 * time.Hour
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

type accountTTFTHistoryCache struct {
	reader  accountTTFTHistoryBatchReader
	mu      sync.RWMutex
	entries map[int64]accountTTFTHistoryCacheEntry
	flight  singleflight.Group
}

func newAccountTTFTHistoryCache(repo UsageLogRepository) *accountTTFTHistoryCache {
	reader, _ := repo.(accountTTFTHistoryBatchReader)
	if reader == nil {
		return nil
	}
	return &accountTTFTHistoryCache{reader: reader, entries: make(map[int64]accountTTFTHistoryCacheEntry)}
}

func (c *accountTTFTHistoryCache) load(ctx context.Context, accounts []*Account, now time.Time) map[int64]AccountTTFTHistory {
	if c == nil || c.reader == nil || len(accounts) == 0 {
		return nil
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
		entry, ok := c.entries[id]
		if ok && entry.expiresAt.After(now) {
			if entry.summary.SampleCount > 0 {
				result[id] = entry.summary
			}
			continue
		}
		// Keep serving the last successful summary while it is refreshed. A
		// transient database failure must not make scheduling forget known TTFT.
		if ok && entry.staleUntil.After(now) && entry.summary.SampleCount > 0 {
			result[id] = entry.summary
		}
		missing = append(missing, id)
	}
	c.mu.RUnlock()
	if len(missing) == 0 {
		return result
	}

	key := accountTTFTHistoryFlightKey(missing)
	loaded, err, _ := c.flight.Do(key, func() (any, error) {
		queryCtx, cancel := context.WithTimeout(ctx, accountTTFTHistoryQueryTimeout)
		defer cancel()
		return c.reader.GetAccountTTFTHistoryBatch(queryCtx, missing, now.Add(-accountTTFTHistoryWindow), accountTTFTHistorySampleLimit)
	})
	if err != nil {
		c.storeRefreshFailure(missing, now, result)
		return result
	}
	summaries, _ := loaded.(map[int64]AccountTTFTHistory)
	c.mu.Lock()
	for _, id := range missing {
		summary := summaries[id]
		c.entries[id] = accountTTFTHistoryCacheEntry{
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

func (c *accountTTFTHistoryCache) storeRefreshFailure(ids []int64, now time.Time, stale map[int64]AccountTTFTHistory) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		entry := c.entries[id]
		entry.failures++
		retryAfter := accountTTFTHistoryRetryDelay(entry.failures)
		if _, ok := stale[id]; ok && entry.staleUntil.After(now) && entry.summary.SampleCount > 0 {
			entry.expiresAt = now.Add(retryAfter)
			c.entries[id] = entry
			continue
		}
		c.entries[id] = accountTTFTHistoryCacheEntry{expiresAt: now.Add(retryAfter), failures: entry.failures}
	}
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

func accountTTFTHistoryFlightKey(ids []int64) string {
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(id, 10))
	}
	return b.String()
}
