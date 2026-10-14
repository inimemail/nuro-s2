package service

import (
	"context"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"
)

const geminiCacheViewShards = 64

type geminiCacheViewShard struct{ entries map[string]GeminiCacheBinding }

// Entries never contain prompts, credentials or money. Updates replace only
// the affected shard, readers take no locks or remote calls.
type GeminiCacheResourceView struct {
	shards                [geminiCacheViewShards]atomic.Pointer[geminiCacheViewShard]
	mu                    sync.Mutex
	refreshMu             sync.Mutex
	revision              atomic.Int64
	deadline              atomic.Int64
	authorizationDeadline atomic.Int64
	maxEntries            int
	count                 int
	started               time.Time
}

func NewGeminiCacheResourceView(maxEntries int) *GeminiCacheResourceView {
	if maxEntries <= 0 || maxEntries > 100000 {
		maxEntries = 10000
	}
	v := &GeminiCacheResourceView{maxEntries: maxEntries, started: time.Now()}
	for i := range v.shards {
		v.shards[i].Store(&geminiCacheViewShard{entries: map[string]GeminiCacheBinding{}})
	}
	return v
}
func cacheShardIndex(id string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return h.Sum32() % geminiCacheViewShards
}
func (v *GeminiCacheResourceView) Ready() bool {
	return v != nil && v.deadline.Load() > time.Since(v.started).Nanoseconds()
}
func (v *GeminiCacheResourceView) Invalidate() {
	if v != nil {
		v.deadline.Store(0)
	}
}
func (v *GeminiCacheResourceView) Lookup(owner GeminiCacheOwner, id, model string) (GeminiCacheBinding, error) {
	if !v.Ready() {
		return GeminiCacheBinding{}, ErrGeminiCacheUnavailable
	}
	if !ValidGeminiCachePublicID(id) {
		return GeminiCacheBinding{}, ErrGeminiCacheNotFound
	}
	entry, exists := v.shards[cacheShardIndex(id)].Load().entries[id]
	if exists && entry.Owner == owner && entry.State == "unavailable" {
		return GeminiCacheBinding{}, ErrGeminiCachePermission
	}
	if !exists || entry.Owner != owner || entry.State != "active" || entry.RequestedModel != model || !entry.ExpiresAt.After(time.Now()) || entry.UpstreamName == "" {
		return GeminiCacheBinding{}, ErrGeminiCacheNotFound
	}
	return entry, nil
}
func (v *GeminiCacheResourceView) Apply(batch *GeminiCacheViewBatch, bootstrap bool) error {
	if batch == nil || !batch.Complete {
		v.Invalidate()
		return ErrGeminiCacheUnavailable
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if batch.Revision < v.revision.Load() {
		return ErrGeminiCacheUnavailable
	}
	bootstrap = bootstrap || batch.Reset
	changed := map[uint32]map[string]GeminiCacheBinding{}
	if bootstrap {
		for i := uint32(0); i < geminiCacheViewShards; i++ {
			changed[i] = map[string]GeminiCacheBinding{}
		}
	}
	count := v.count
	if bootstrap {
		count = 0
	}
	now := time.Now()
	for _, entry := range batch.Bindings {
		idx := cacheShardIndex(entry.ID)
		entries, ok := changed[idx]
		if !ok {
			entries = map[string]GeminiCacheBinding{}
			for id, old := range v.shards[idx].Load().entries {
				entries[id] = old
			}
			changed[idx] = entries
		}
		old, exists := entries[entry.ID]
		if exists && old.Revision > entry.Revision {
			continue
		}
		if entry.State == "deleted" || (!entry.ExpiresAt.After(now) && (entry.State == "active" || entry.State == "unavailable")) {
			if exists {
				delete(entries, entry.ID)
				count--
			}
			continue
		}
		if !exists {
			count++
		}
		entries[entry.ID] = entry
	}
	// Prune expiry on every background update, even if there were no events.
	for idx := uint32(0); idx < geminiCacheViewShards; idx++ {
		entries, changedShard := changed[idx]
		if !changedShard {
			entries = v.shards[idx].Load().entries
		}
		for id, old := range entries {
			if (old.State != "active" && old.State != "unavailable") || old.ExpiresAt.After(now) {
				continue
			}
			if !changedShard {
				copyEntries := make(map[string]GeminiCacheBinding, len(entries))
				for k, val := range entries {
					copyEntries[k] = val
				}
				entries = copyEntries
				changed[idx] = entries
				changedShard = true
			}
			delete(entries, id)
			count--
		}
	}
	if count > v.maxEntries {
		v.Invalidate()
		return ErrGeminiCacheUnavailable
	}
	for idx, entries := range changed {
		v.shards[idx].Store(&geminiCacheViewShard{entries: entries})
	}
	v.count = count
	v.revision.Store(batch.Revision)
	return nil
}

// Use the local request start to bound validity. A delayed DB response never
// grants a new ten-second window from its arrival time.
func (v *GeminiCacheResourceView) Refresh(ctx context.Context, repo GeminiCachedContentRepository, node string, bootstrap bool) error {
	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()
	started := time.Now()
	// Natural subscription start/expiry has no database UPDATE and therefore
	// no outbox event. Stop using the old projection at that time and rebuild
	// it on the existing background path, keeping references entirely local.
	boundary := v.authorizationDeadline.Load()
	if boundary > 0 && time.Since(v.started).Nanoseconds() >= boundary {
		bootstrap = true
	}
	revision := v.revision.Load()
	batch, err := repo.ReadView(ctx, revision, v.maxEntries, bootstrap)
	if err != nil {
		v.Invalidate()
		return err
	}
	if err = v.Apply(batch, bootstrap); err != nil {
		return err
	}
	if bootstrap || batch.Reset || len(batch.Bindings) > 0 {
		boundary = 0
		if batch.NextAuthorizationChange != nil {
			boundary = batch.NextAuthorizationChange.Sub(v.started).Nanoseconds()
		}
		v.authorizationDeadline.Store(boundary)
	}
	if err = repo.LeaseView(ctx, node, batch.Revision, 10*time.Second); err != nil {
		v.Invalidate()
		return err
	}
	deadline := started.Add(8 * time.Second)
	if boundary > 0 && boundary < deadline.Sub(v.started).Nanoseconds() {
		deadline = v.started.Add(time.Duration(boundary))
	}
	if !deadline.After(time.Now()) {
		v.Invalidate()
		return ErrGeminiCacheUnavailable
	}
	v.deadline.Store(deadline.Sub(v.started).Nanoseconds())
	return nil
}

func WaitGeminiCacheBarrier(ctx context.Context, repo GeminiCachedContentRepository, revision int64) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	for {
		ready, err := repo.Barrier(ctx, revision)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
