package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestTenantEscrowFencesRestartAndReclaimsDeadNode(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	manager := NewTenantEscrowManager(client, 2*time.Second)
	ctx := context.Background()
	epoch, err := manager.RegisterNode(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	granted, err := manager.Grant(ctx, "tenant-a", "node-a", epoch, 3, 5)
	if err != nil || granted != 3 {
		t.Fatalf("grant=%d err=%v", granted, err)
	}
	newEpoch, err := manager.RegisterNode(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Grant(ctx, "tenant-a", "node-a", epoch, 1, 5); err != ErrEscrowFenced {
		t.Fatalf("old epoch was accepted: %v", err)
	}
	if _, err := manager.Grant(ctx, "tenant-a", "node-a", newEpoch, 1, 5); err != ErrEscrowFenced {
		t.Fatalf("replacement node reused live grants: %v", err)
	}
	// Let the old node lease expire; only then can its grants be reclaimed.
	mr.FastForward(3 * time.Second)
	reclaimed, err := manager.ReclaimDeadNode(ctx, "tenant-a", "node-a", epoch)
	if err != nil || !reclaimed {
		t.Fatalf("reclaim=%v err=%v", reclaimed, err)
	}
	if err := manager.Heartbeat(ctx, "node-a", newEpoch); err != nil {
		t.Fatal(err)
	}
	granted, err = manager.Grant(ctx, "tenant-a", "node-a", newEpoch, 1, 5)
	if err != nil || granted != 1 {
		t.Fatalf("replacement grant=%d err=%v", granted, err)
	}
}

func TestTenantEscrowRegistryRetainsOldEpochForAutomaticReclaim(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	manager := NewTenantEscrowManager(client, 2*time.Second)
	ctx := context.Background()
	oldEpoch, err := manager.RegisterNode(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if granted, grantErr := manager.Grant(ctx, "tenant-a", "node-a", oldEpoch, 3, 5); grantErr != nil || granted != 3 {
		t.Fatalf("grant=%d err=%v", granted, grantErr)
	}
	newEpoch, err := manager.RegisterNode(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if fields := client.HLen(ctx, escrowNodeRegistryKey).Val(); fields != 2 {
		t.Fatalf("registry lost an owner epoch: fields=%d", fields)
	}
	mr.FastForward(3 * time.Second)
	if err := manager.Heartbeat(ctx, "node-a", newEpoch); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReclaimExpiredNodes(ctx, 0); err != nil {
		t.Fatal(err)
	}
	// The first pass records dead-since; the second performs the reclaim.
	if err := client.Del(ctx, escrowReclaimLockKey).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReclaimExpiredNodes(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if client.Exists(ctx, manager.holderKey("tenant-a"), manager.allocatedKey("tenant-a")).Val() != 0 {
		t.Fatal("old epoch grants were not reclaimed")
	}
}

func TestStableHolderSetDoesNotFanOutTinyLimits(t *testing.T) {
	nodes := []string{"a", "b", "c", "d", "e", "f", "g"}
	set := StableHolderSet("tenant-a", 5, nodes)
	if len(set) != 5 {
		t.Fatalf("unexpected holder set: %v", set)
	}
	reversed := StableHolderSet("tenant-a", 5, []string{"g", "f", "e", "d", "c", "b", "a", "a"})
	if strings.Join(set, ",") != strings.Join(reversed, ",") {
		t.Fatalf("holder set depends on input order: %v vs %v", set, reversed)
	}
}

func TestTenantEscrowGrantDoesNotExpireWhileNodeIsAlive(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	manager := NewTenantEscrowManager(client, 2*time.Second)
	ctx := context.Background()
	epoch, err := manager.RegisterNode(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if granted, err := manager.Grant(ctx, "tenant-a", "node-a", epoch, 3, 5); err != nil || granted != 3 {
		t.Fatalf("grant=%d err=%v", granted, err)
	}
	if ttl := mr.TTL(manager.holderKey("tenant-a")); ttl != 0 {
		t.Fatalf("escrow holder must be explicitly reclaimed, ttl=%v", ttl)
	}
}

func TestLocalTenantEscrowReturnsExcessGrantAfterLimitDecrease(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	manager := NewTenantEscrowManager(client, 30*time.Second)
	ctx := context.Background()
	escrow, err := newLocalTenantEscrow(ctx, manager, "node-a", 4, 30*time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(escrow.Close)
	if acquired, err := escrow.Acquire(ctx, "tenant-a", 4, "request-1"); err != nil || !acquired {
		t.Fatalf("initial acquire=%v err=%v", acquired, err)
	}
	if !escrow.Release("request-1") {
		t.Fatal("initial request was not released")
	}
	if acquired, err := escrow.Acquire(ctx, "tenant-a", 1, "request-2"); err != nil || !acquired {
		t.Fatalf("reduced-limit acquire=%v err=%v", acquired, err)
	}
	allocated, err := client.Get(ctx, manager.allocatedKey("tenant-a")).Int()
	if err != nil || allocated != 1 {
		t.Fatalf("allocated=%d err=%v, want 1", allocated, err)
	}
}

func TestLocalTenantEscrowEvictsIdleTenantState(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	manager := NewTenantEscrowManager(client, 30*time.Second)
	ctx := context.Background()
	escrow, err := newLocalTenantEscrow(ctx, manager, "node-a", 4, 30*time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(escrow.Close)
	if acquired, err := escrow.Acquire(ctx, "tenant-a", 4, "request-1"); err != nil || !acquired {
		t.Fatalf("acquire=%v err=%v", acquired, err)
	}
	if !escrow.Release("request-1") {
		t.Fatal("request was not released")
	}
	escrow.mu.RLock()
	state := escrow.states["tenant-a"]
	escrow.mu.RUnlock()
	state.mu.Lock()
	state.lastUsed = time.Now().Add(-2 * time.Minute)
	state.mu.Unlock()
	escrow.releaseIdleStates(ctx, time.Now().Add(-time.Minute))
	escrow.mu.RLock()
	_, retained := escrow.states["tenant-a"]
	escrow.mu.RUnlock()
	if retained {
		t.Fatal("idle tenant state was retained")
	}
	if client.Exists(ctx, manager.allocatedKey("tenant-a"), manager.holderKey("tenant-a")).Val() != 0 {
		t.Fatal("idle tenant Redis metadata was retained")
	}
}
