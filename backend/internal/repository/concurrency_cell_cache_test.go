package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestCellAwareConcurrencyCacheIsolatesPlatformsAndReleasesOwnerCell(t *testing.T) {
	control := miniredis.RunT(t)
	openAI := miniredis.RunT(t)
	anthropic := miniredis.RunT(t)
	controlClient := redis.NewClient(&redis.Options{Addr: control.Addr()})
	openAIClient := redis.NewClient(&redis.Options{Addr: openAI.Addr()})
	anthropicClient := redis.NewClient(&redis.Options{Addr: anthropic.Addr()})
	legacy := NewConcurrencyCache(controlClient, 1, 60).(*concurrencyCache)
	cfg := admissionTestConfig(openAI.Addr(), anthropic.Addr())

	cacheInterface, err := newCellAwareConcurrencyCache(controlClient, cfg, legacy)
	require.NoError(t, err)
	cache := cacheInterface.(*cellAwareConcurrencyCache)
	t.Cleanup(func() { require.NoError(t, cache.Close()) })

	ctx := context.Background()
	acquired, err := cache.AcquireAccountSlotForPlatform(ctx, "openai", 101, 1, "openai-request")
	require.NoError(t, err)
	require.True(t, acquired)
	acquired, err = cache.AcquireAccountSlotForPlatform(ctx, "anthropic", 202, 1, "anthropic-request")
	require.NoError(t, err)
	require.True(t, acquired)

	require.Equal(t, int64(1), openAIClient.ZCard(ctx, accountSlotKey(101)).Val())
	require.Equal(t, int64(0), openAIClient.ZCard(ctx, accountSlotKey(202)).Val())
	require.Equal(t, int64(1), anthropicClient.ZCard(ctx, accountSlotKey(202)).Val())
	require.Equal(t, int64(0), controlClient.ZCard(ctx, accountSlotKey(101)).Val())

	require.NoError(t, cache.ReleaseAccountSlot(ctx, 101, "openai-request"))
	require.NoError(t, cache.ReleaseAccountSlot(ctx, 202, "anthropic-request"))
	require.Equal(t, int64(0), openAIClient.ZCard(ctx, accountSlotKey(101)).Val())
	require.Equal(t, int64(0), anthropicClient.ZCard(ctx, accountSlotKey(202)).Val())
	require.NoError(t, cache.SetAccountCooldown(ctx, 101, time.Minute))
	acquired, err = cache.AcquireAccountSlotForPlatform(ctx, "openai", 101, 1, "cooled-request")
	require.NoError(t, err)
	require.False(t, acquired)
}

func TestCellAwareConcurrencyCacheEscrowRollsBackFailedAccountClaim(t *testing.T) {
	control := miniredis.RunT(t)
	openAI := miniredis.RunT(t)
	anthropic := miniredis.RunT(t)
	controlClient := redis.NewClient(&redis.Options{Addr: control.Addr()})
	openAIClient := redis.NewClient(&redis.Options{Addr: openAI.Addr()})
	legacy := NewConcurrencyCache(controlClient, 1, 60).(*concurrencyCache)
	cfg := admissionTestConfig(openAI.Addr(), anthropic.Addr())

	cacheInterface, err := newCellAwareConcurrencyCache(controlClient, cfg, legacy)
	require.NoError(t, err)
	cache := cacheInterface.(*cellAwareConcurrencyCache)
	t.Cleanup(func() { require.NoError(t, cache.Close()) })

	ctx := context.Background()
	require.NoError(t, openAIClient.ZAdd(ctx, accountSlotKey(303), redis.Z{Score: float64(time.Now().Unix()), Member: "busy"}).Err())
	accountID, acquired, err := cache.AcquireFirstAvailableUserAccountSlots(ctx, 77, 1, []service.AccountSlotCandidate{{AccountID: 303, MaxConcurrency: 1, Platform: "openai"}}, "user-1", "account-1")
	require.NoError(t, err)
	require.False(t, acquired)
	require.Zero(t, accountID)
	require.Equal(t, 0, cache.escrow.InUse("user:77"))

	require.NoError(t, openAIClient.Del(ctx, accountSlotKey(303)).Err())
	accountID, acquired, err = cache.AcquireFirstAvailableUserAccountSlots(ctx, 77, 1, []service.AccountSlotCandidate{{AccountID: 303, MaxConcurrency: 1, Platform: "openai"}}, "user-2", "account-2")
	require.NoError(t, err)
	require.True(t, acquired)
	require.Equal(t, int64(303), accountID)
	require.Equal(t, 1, cache.escrow.InUse("user:77"))
	require.NoError(t, cache.ReleaseUserSlot(ctx, 77, "user-2"))
	require.NoError(t, cache.ReleaseAccountSlot(ctx, 303, "account-2"))
}

func TestCellAwareConcurrencyCacheRecoversRemoteAssignmentForDirectReads(t *testing.T) {
	control := miniredis.RunT(t)
	openAI := miniredis.RunT(t)
	anthropic := miniredis.RunT(t)
	controlClientA := redis.NewClient(&redis.Options{Addr: control.Addr()})
	controlClientB := redis.NewClient(&redis.Options{Addr: control.Addr()})
	cfg := admissionTestConfig(openAI.Addr(), anthropic.Addr())
	cacheAInterface, err := newCellAwareConcurrencyCache(controlClientA, cfg, NewConcurrencyCache(controlClientA, 1, 60).(*concurrencyCache))
	require.NoError(t, err)
	cacheBInterface, err := newCellAwareConcurrencyCache(controlClientB, cfg, NewConcurrencyCache(controlClientB, 1, 60).(*concurrencyCache))
	require.NoError(t, err)
	cacheA := cacheAInterface.(*cellAwareConcurrencyCache)
	cacheB := cacheBInterface.(*cellAwareConcurrencyCache)
	t.Cleanup(func() {
		require.NoError(t, cacheA.Close())
		require.NoError(t, cacheB.Close())
	})
	ctx := context.Background()
	acquired, err := cacheA.AcquireAccountSlotForPlatform(ctx, "openai", 404, 2, "remote-request")
	require.NoError(t, err)
	require.True(t, acquired)
	count, err := cacheB.GetAccountConcurrency(ctx, 404)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestCellAwareConcurrencyCacheDoesNotClaimCellAccountThroughLegacyFallback(t *testing.T) {
	control := miniredis.RunT(t)
	openAI := miniredis.RunT(t)
	anthropic := miniredis.RunT(t)
	controlClient := redis.NewClient(&redis.Options{Addr: control.Addr()})
	legacy := NewConcurrencyCache(controlClient, 1, 60).(*concurrencyCache)
	cacheInterface, err := newCellAwareConcurrencyCache(controlClient, admissionTestConfig(openAI.Addr(), anthropic.Addr()), legacy)
	require.NoError(t, err)
	cache := cacheInterface.(*cellAwareConcurrencyCache)
	t.Cleanup(func() { require.NoError(t, cache.Close()) })
	ctx := context.Background()
	require.NoError(t, controlClient.ZAdd(ctx, accountSlotKey(501), redis.Z{Score: float64(time.Now().Unix()), Member: "busy"}).Err())
	accountID, acquired, err := cache.AcquireFirstAvailableAccountSlot(ctx, []service.AccountSlotCandidate{
		{AccountID: 501, MaxConcurrency: 1, Platform: "grok"},
		{AccountID: 502, MaxConcurrency: 1, Platform: "openai"},
	}, "request")
	require.NoError(t, err)
	require.True(t, acquired)
	require.Equal(t, int64(502), accountID)
	require.Equal(t, int64(0), controlClient.ZCard(ctx, accountSlotKey(502)).Val())
	require.Equal(t, int64(1), redis.NewClient(&redis.Options{Addr: openAI.Addr()}).ZCard(ctx, accountSlotKey(502)).Val())
}

func admissionTestConfig(openAIAddr, anthropicAddr string) *config.Config {
	return &config.Config{
		Redis: config.RedisConfig{PoolSize: 16, MinIdleConns: 1, DialTimeoutSeconds: 1, ReadTimeoutSeconds: 1, WriteTimeoutSeconds: 1},
		Gateway: config.GatewayConfig{
			ConcurrencySlotTTLMinutes: 1,
			Admission: config.GatewayAdmissionConfig{
				Enabled: true, EscrowEnabled: true, EscrowGrantSize: 4, NodeTTLSeconds: 30, DeadNodeGraceSeconds: 60,
				OpenAICells:    fmt.Sprintf("openai-001=redis://%s/0", openAIAddr),
				AnthropicCells: fmt.Sprintf("anthropic-001=redis://%s/0", anthropicAddr),
			},
		},
	}
}

func TestParseAdmissionCellDefinitionsRejectsCrossPlatformCell(t *testing.T) {
	_, err := parseAdmissionCellDefinitions("shared=redis://one:6379/0", "shared=redis://two:6379/0")
	require.ErrorContains(t, err, "shared")
	definitions, err := parseAdmissionCellDefinitions("openai-001=redis://one:6379/0", "anthropic-001=redis://two:6379/0")
	require.NoError(t, err)
	require.Len(t, definitions, 2)
}
