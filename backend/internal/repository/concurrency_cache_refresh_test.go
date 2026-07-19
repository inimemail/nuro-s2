package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRefreshSlotsBatchesWithoutResurrectingReleasedMembers(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	cache := NewConcurrencyCache(client, 1, 60).(*concurrencyCache)
	ctx := context.Background()
	if acquired, err := cache.AcquireAccountSlot(ctx, 11, 2, "account-live"); err != nil || !acquired {
		t.Fatalf("acquire account: acquired=%v err=%v", acquired, err)
	}
	if acquired, err := cache.AcquireUserSlot(ctx, 21, 2, "user-live"); err != nil || !acquired {
		t.Fatalf("acquire user: acquired=%v err=%v", acquired, err)
	}
	if err := cache.ReleaseAccountSlot(ctx, 11, "account-live"); err != nil {
		t.Fatal(err)
	}
	if err := cache.RefreshSlots(ctx, []service.ConcurrencySlotRenewal{
		{Kind: "account", EntityID: 11, RequestID: "account-live"},
		{Kind: "user", EntityID: 21, RequestID: "user-live"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ZScore(ctx, accountSlotKey(11), "account-live").Result(); err != redis.Nil {
		t.Fatalf("released account slot was resurrected: %v", err)
	}
	if _, err := client.ZScore(ctx, userSlotKey(21), "user-live").Result(); err != nil {
		t.Fatalf("live user slot was not refreshed: %v", err)
	}
}
