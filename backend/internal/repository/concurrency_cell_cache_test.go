package repository

import (
	"context"
	"fmt"
	"os"
	"strings"
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

func TestCellAwareConcurrencyCacheMigratesCooldownSources(t *testing.T) {
	for _, tc := range []struct {
		name              string
		runtime           bool
		pool              bool
		removeLegacy      bool
		legacyValue       string
		runtimeGeneration int64
		poolTTL           time.Duration
	}{
		{name: "pool_only", pool: true},
		{name: "runtime_and_pool", runtime: true, pool: true, runtimeGeneration: 7},
		{name: "runtime_hidden_by_longer_pool", runtime: true, pool: true, runtimeGeneration: 7, poolTTL: 3 * time.Minute},
		{name: "runtime_without_legacy_mirror", runtime: true, removeLegacy: true, runtimeGeneration: 7},
		{name: "legacy_runtime", runtime: true, legacyValue: "1"},
		{name: "legacy_unprefixed_generation", runtime: true, legacyValue: "g:7", runtimeGeneration: 7},
		{name: "legacy_runtime_generation", runtime: true, legacyValue: "r:g:7", runtimeGeneration: 7},
		{name: "legacy_pool_marker", pool: true, legacyValue: "p:g:9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := miniredis.RunT(t)
			openAI := miniredis.RunT(t)
			anthropic := miniredis.RunT(t)
			controlClient := redis.NewClient(&redis.Options{Addr: control.Addr()})
			t.Cleanup(func() { require.NoError(t, controlClient.Close()) })
			legacy := NewConcurrencyCache(controlClient, 1, 60).(*concurrencyCache)
			cfg := admissionTestConfig(openAI.Addr(), anthropic.Addr())
			cfg.Gateway.Admission.EscrowEnabled = false
			cacheInterface, err := newCellAwareConcurrencyCache(controlClient, cfg, legacy)
			require.NoError(t, err)
			cache := cacheInterface.(*cellAwareConcurrencyCache)
			t.Cleanup(func() { require.NoError(t, cache.Close()) })
			ctx := context.Background()
			const accountID int64 = 811
			if tc.legacyValue != "" {
				require.NoError(t, controlClient.Set(ctx, accountCooldownKey(accountID), tc.legacyValue, 2*time.Minute).Err())
			} else if tc.runtime {
				require.NoError(t, legacy.SetAccountCooldownGeneration(ctx, accountID, 2*time.Minute, tc.runtimeGeneration))
			}
			if tc.pool && !strings.HasPrefix(tc.legacyValue, "p:") {
				ttl := tc.poolTTL
				if ttl == 0 {
					ttl = time.Minute
				}
				require.NoError(t, legacy.SetOpenAIPoolCooldownGeneration(ctx, accountID, ttl, 9))
			}
			if tc.removeLegacy {
				require.NoError(t, controlClient.Del(ctx, accountCooldownKey(accountID)).Err())
			}

			acquired, err := cache.AcquireAccountSlotForPlatform(ctx, "openai", accountID, 1, "first-request")
			require.NoError(t, err)
			require.False(t, acquired, "an active source must survive the first Cell assignment")
			require.Equal(t, tc.runtime, openAI.Exists(accountRuntimeCooldownKey(accountID)))
			if tc.runtime {
				value, err := openAI.Get(accountRuntimeCooldownKey(accountID))
				require.NoError(t, err)
				require.Equal(t, tc.runtimeGeneration, parseCooldownGeneration(value))
			}
			require.Equal(t, tc.pool, openAI.Exists(openAIPoolCooldownKey(accountID)))
			if tc.pool {
				_, generation, active, err := cache.GetOpenAIPoolCooldown(ctx, accountID)
				require.NoError(t, err)
				require.True(t, active)
				require.Equal(t, int64(9), generation)
			}

			require.NoError(t, cache.ClearOpenAIPoolCooldown(ctx, accountID))
			acquired, err = cache.AcquireAccountSlotForPlatform(ctx, "openai", accountID, 1, "after-pool-clear")
			require.NoError(t, err)
			require.Equal(t, !tc.runtime, acquired, "clearing the pool must preserve only genuine runtime blocks")
			if tc.runtime {
				require.NoError(t, cache.ClearAccountCooldown(ctx, accountID))
				acquired, err = cache.AcquireAccountSlotForPlatform(ctx, "openai", accountID, 1, "after-runtime-clear")
				require.NoError(t, err)
				require.True(t, acquired)
				require.NoError(t, cache.ReleaseAccountSlot(ctx, accountID, "after-runtime-clear"))
			} else {
				require.NoError(t, cache.ReleaseAccountSlot(ctx, accountID, "after-pool-clear"))
			}

			// A replacement replica must use the Cell's current state, rather
			// than re-importing a cleared snapshot still present in control Redis.
			cacheBInterface, err := newCellAwareConcurrencyCache(controlClient, cfg, legacy)
			require.NoError(t, err)
			cacheB := cacheBInterface.(*cellAwareConcurrencyCache)
			t.Cleanup(func() { require.NoError(t, cacheB.Close()) })
			acquired, err = cacheB.AcquireAccountSlotForPlatform(ctx, "openai", accountID, 1, "new-replica-request")
			require.NoError(t, err)
			require.True(t, acquired, "a new replica must not restore an already-cleared cooldown")
		})
	}
}

func TestCellAwareConcurrencyCacheRetriesFailedCooldownMigration(t *testing.T) {
	control := miniredis.RunT(t)
	openAI := miniredis.RunT(t)
	anthropic := miniredis.RunT(t)
	controlClient := redis.NewClient(&redis.Options{Addr: control.Addr()})
	t.Cleanup(func() { require.NoError(t, controlClient.Close()) })
	legacy := NewConcurrencyCache(controlClient, 1, 60).(*concurrencyCache)
	cfg := admissionTestConfig(openAI.Addr(), anthropic.Addr())
	cfg.Gateway.Admission.EscrowEnabled = false
	cacheInterface, err := newCellAwareConcurrencyCache(controlClient, cfg, legacy)
	require.NoError(t, err)
	cache := cacheInterface.(*cellAwareConcurrencyCache)
	t.Cleanup(func() { require.NoError(t, cache.Close()) })
	ctx := context.Background()
	const accountID int64 = 812
	require.NoError(t, legacy.SetAccountCooldownGeneration(ctx, accountID, time.Minute, 7))
	openAI.SetError("ERR migration unavailable")
	acquired, err := cache.AcquireAccountSlotForPlatform(ctx, "openai", accountID, 1, "failed-migration")
	require.ErrorContains(t, err, "migrate account")
	require.False(t, acquired)
	require.NotContains(t, cache.routes, accountID, "an incomplete route must not become visible")

	openAI.SetError("")
	acquired, err = cache.AcquireAccountSlotForPlatform(ctx, "openai", accountID, 1, "retried-migration")
	require.NoError(t, err)
	require.False(t, acquired, "retry must preserve the active runtime block")
	require.True(t, openAI.Exists(accountRuntimeCooldownKey(accountID)))
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

func TestCellAwareConcurrencyCacheReplacementAdoptsEscrowWithoutLegacyFallback(t *testing.T) {
	control := miniredis.RunT(t)
	openAI := miniredis.RunT(t)
	anthropic := miniredis.RunT(t)
	controlClientA := redis.NewClient(&redis.Options{Addr: control.Addr()})
	controlClientB := redis.NewClient(&redis.Options{Addr: control.Addr()})
	cfgA := admissionTestConfig(openAI.Addr(), anthropic.Addr())
	cfgB := admissionTestConfig(openAI.Addr(), anthropic.Addr())
	cfgA.Gateway.Admission.NodeID = "restarted-node"
	cfgB.Gateway.Admission.NodeID = "restarted-node"
	cfgA.Gateway.Admission.EscrowGrantSize = 1
	cfgB.Gateway.Admission.EscrowGrantSize = 1

	cacheAInterface, err := newCellAwareConcurrencyCache(controlClientA, cfgA, NewConcurrencyCache(controlClientA, 1, 60).(*concurrencyCache))
	require.NoError(t, err)
	cacheA := cacheAInterface.(*cellAwareConcurrencyCache)
	t.Cleanup(func() { require.NoError(t, cacheA.Close()) })

	ctx := context.Background()
	acquired, err := cacheA.AcquireUserSlot(ctx, 88, 1, "old-request")
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, cacheA.ReleaseUserSlot(ctx, 88, "old-request"))

	// Simulate a replacement process with the same stable node identity. Its
	// epoch is current, but the previous epoch still owns the tenant holder.
	cacheBInterface, err := newCellAwareConcurrencyCache(controlClientB, cfgB, NewConcurrencyCache(controlClientB, 1, 60).(*concurrencyCache))
	require.NoError(t, err)
	cacheB := cacheBInterface.(*cellAwareConcurrencyCache)
	t.Cleanup(func() { require.NoError(t, cacheB.Close()) })

	accountID, acquired, err := cacheB.AcquireFirstAvailableUserAccountSlots(ctx, 88, 1, []service.AccountSlotCandidate{
		{AccountID: 909, MaxConcurrency: 1, Platform: "openai"},
	}, "new-user-request", "new-account-request")
	require.NoError(t, err)
	require.True(t, acquired)
	require.Equal(t, int64(909), accountID)
	require.False(t, cacheB.escrowDegraded.Load())
	require.Equal(t, int64(0), controlClientB.ZCard(ctx, userSlotKey(88)).Val())
	require.Equal(t, int64(1), redis.NewClient(&redis.Options{Addr: openAI.Addr()}).ZCard(ctx, accountSlotKey(909)).Val())

	current, err := cacheB.GetUserConcurrency(ctx, 88)
	require.NoError(t, err)
	require.Equal(t, 1, current)
	require.NoError(t, cacheB.ReleaseUserSlot(ctx, 88, "new-user-request"))
	require.NoError(t, cacheB.ReleaseAccountSlot(ctx, 909, "new-account-request"))
	require.Equal(t, int64(0), controlClientB.ZCard(ctx, userSlotKey(88)).Val())

	acquired, err = cacheB.AcquireUserSlot(ctx, 88, 1, "direct-user-request")
	require.NoError(t, err)
	require.True(t, acquired)
	require.Equal(t, int64(0), controlClientB.ZCard(ctx, userSlotKey(88)).Val())
	require.Equal(t, 1, cacheB.escrow.InUse("user:88"))
	require.NoError(t, cacheB.ReleaseUserSlot(ctx, 88, "direct-user-request"))
}

func TestCellAwareConcurrencyCacheFallsBackWhenEscrowCannotOwnEpoch(t *testing.T) {
	control := miniredis.RunT(t)
	openAI := miniredis.RunT(t)
	anthropic := miniredis.RunT(t)
	controlClient := redis.NewClient(&redis.Options{Addr: control.Addr()})
	cacheInterface, err := newCellAwareConcurrencyCache(controlClient, admissionTestConfig(openAI.Addr(), anthropic.Addr()), NewConcurrencyCache(controlClient, 1, 60).(*concurrencyCache))
	require.NoError(t, err)
	cache := cacheInterface.(*cellAwareConcurrencyCache)
	t.Cleanup(func() { require.NoError(t, cache.Close()) })

	cache.escrow.validTo.Store(0)
	acquired, err := cache.AcquireUserSlot(context.Background(), 89, 1, "fallback-request")
	require.NoError(t, err)
	require.True(t, acquired)
	require.True(t, cache.escrowDegraded.Load())
	require.Equal(t, int64(1), controlClient.ZCard(context.Background(), userSlotKey(89)).Val())
	require.NoError(t, cache.ReleaseUserSlot(context.Background(), 89, "fallback-request"))
}

func TestResolveAdmissionEscrowNodeIDScopesConfiguredValueToReplica(t *testing.T) {
	hostname, err := os.Hostname()
	require.NoError(t, err)
	hostname = strings.TrimSpace(hostname)
	require.NotEmpty(t, hostname)

	require.Equal(t, hostname, resolveAdmissionEscrowNodeID(""))
	require.Equal(t, hostname, resolveAdmissionEscrowNodeID(hostname))
	require.Equal(t, "gateway-a:"+hostname, resolveAdmissionEscrowNodeID(" gateway-a "))
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
