package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
	"time"
)

func TestV0222StickyCASPreventsLateWritesAndDeletes(t *testing.T) {
	ctx := context.Background()
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &gatewayCache{rdb: client}
	_, missErr := cache.GetSessionAccountID(ctx, 1, "session")
	require.True(t, cache.IsSessionAccountNotFound(missErr))
	require.False(t, cache.IsSessionAccountNotFound(context.DeadlineExceeded))
	ok, err := cache.CompareAndSetSessionAccountID(ctx, 1, "session", 0, 11, 24*time.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.CompareAndSetSessionAccountID(ctx, 1, "session", 11, 22, 24*time.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.CompareAndSetSessionAccountID(ctx, 1, "session", 11, 33, 24*time.Hour)
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = cache.CompareAndDeleteSessionAccountID(ctx, 1, "session", 11)
	require.NoError(t, err)
	require.False(t, ok)
	id, err := cache.GetSessionAccountID(ctx, 1, "session")
	require.NoError(t, err)
	require.Equal(t, int64(22), id)
	require.Equal(t, 24*time.Hour, mini.TTL(buildSessionKey(1, "session")))
	ok, err = cache.CompareAndDeleteSessionAccountID(ctx, 2, "session", 22)
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = cache.CompareAndDeleteSessionAccountID(ctx, 1, "session", 22)
	require.NoError(t, err)
	require.True(t, ok)
}
func TestV0222SchedulerCapabilityRoundtrip(t *testing.T) {
	account := service.Account{ID: 1, Platform: service.PlatformOpenAI, Credentials: map[string]any{"openai_capabilities": map[string]any{"responses": true}, "unrelated_secret": "never-cache"}, Extra: map[string]any{service.UpstreamBillingProbeExtraKey: map[string]any{"status": service.UpstreamBillingProbeStatusUnsupported}, "manual_upstream_multiplier": 0.0, "adaptive_upstream_multiplier_factor": .5}}
	snapshot := buildSchedulerMetadataAccount(account)
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var decoded service.Account
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, map[string]any{"responses": true}, decoded.Credentials["openai_capabilities"])
	require.NotContains(t, decoded.Credentials, "unrelated_secret")
	require.Equal(t, 0.0, decoded.Extra["manual_upstream_multiplier"])
	require.Equal(t, .5, decoded.Extra["adaptive_upstream_multiplier_factor"])
}
func TestV0222PostgresGroupTransactionsAndOwnedFableRecovery(t *testing.T) {
	dsn := os.Getenv("NURO_CACHE_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	require.Contains(t, dsn, "dbname=nuro_upgrade_verify")
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	require.NoError(t, ApplyMigrations(ctx, db))
	stamp := uuid.NewString()
	var u1, u2, gid, aid int64
	require.NoError(t, db.QueryRow(`INSERT INTO users(email,password_hash) VALUES($1,'fixture') RETURNING id`, stamp+"a@test.invalid").Scan(&u1))
	require.NoError(t, db.QueryRow(`INSERT INTO users(email,password_hash) VALUES($1,'fixture') RETURNING id`, stamp+"b@test.invalid").Scan(&u2))
	require.NoError(t, db.QueryRow(`INSERT INTO groups(name,platform) VALUES($1,'anthropic') RETURNING id`, stamp).Scan(&gid))
	require.NoError(t, db.QueryRow(`INSERT INTO accounts(name,platform,type) VALUES($1,'anthropic','oauth') RETURNING id`, stamp).Scan(&aid))
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM usage_logs WHERE account_id=$1`, aid)
		_, _ = db.Exec(`DELETE FROM api_keys WHERE user_id=$1`, u1)
		_, _ = db.Exec(`DELETE FROM user_group_rate_multipliers WHERE group_id=$1`, gid)
		_, _ = db.Exec(`DELETE FROM scheduler_outbox WHERE account_id=$1 OR group_id=$2`, aid, gid)
		_, _ = db.Exec(`DELETE FROM accounts WHERE id=$1`, aid)
		_, _ = db.Exec(`DELETE FROM groups WHERE id=$1`, gid)
		_, _ = db.Exec(`DELETE FROM users WHERE id IN($1,$2)`, u1, u2)
	})
	repo := NewUserGroupRateRepository(db)
	rpm := 17
	require.NoError(t, repo.SyncGroupRateMultipliers(ctx, gid, []service.GroupRateMultiplierInput{{UserID: u1, RateMultiplier: 2}, {UserID: u1, RateMultiplier: 3}}))
	require.NoError(t, repo.SyncGroupRPMOverrides(ctx, gid, []service.GroupRPMOverrideInput{{UserID: u1, RPMOverride: &rpm}}))
	rate, err := repo.GetByUserAndGroup(ctx, u1, gid)
	require.NoError(t, err)
	require.Equal(t, 3.0, *rate)
	require.Error(t, repo.SyncGroupRateMultipliers(ctx, gid, []service.GroupRateMultiplierInput{{UserID: 9223372036854775806, RateMultiplier: 4}}))
	rate, err = repo.GetByUserAndGroup(ctx, u1, gid)
	require.NoError(t, err)
	require.Equal(t, 3.0, *rate)
	require.NoError(t, repo.SyncGroupRateMultipliers(ctx, gid, nil))
	require.NoError(t, repo.SyncGroupRPMOverrides(ctx, gid, []service.GroupRPMOverrideInput{{UserID: u1, RPMOverride: &rpm}, {UserID: u2, RPMOverride: &rpm}}))
	rate, err = repo.GetByUserAndGroup(ctx, u1, gid)
	require.NoError(t, err)
	require.Nil(t, rate)
	savedRPM, err := repo.GetRPMOverrideByUserAndGroup(ctx, u1, gid)
	require.NoError(t, err)
	require.Equal(t, rpm, *savedRPM)
	require.Error(t, repo.SyncGroupRPMOverrides(ctx, gid, []service.GroupRPMOverrideInput{{UserID: 9223372036854775806, RPMOverride: &rpm}}))
	savedRPM, err = repo.GetRPMOverrideByUserAndGroup(ctx, u1, gid)
	require.NoError(t, err)
	require.Equal(t, rpm, *savedRPM)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	key, err := client.APIKey.Create().SetUserID(u1).SetKey("sk-fixture-" + stamp).SetName("upgrade fixture").Save(ctx)
	require.NoError(t, err)
	created := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	_, err = db.Exec(`INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,input_tokens,output_tokens,total_cost,actual_cost,account_stats_cost,account_rate_multiplier,created_at,inbound_endpoint,upstream_endpoint,billing_type)
 VALUES($1,$2,$3,$4,'fixture-model',10,20,10,3,2,.5,$5,'/v1/messages','/v1/messages',0),
 ($1,$2,$3,$6,'fixture-model',10,20,4,0,NULL,2,$5,'/v1/messages','/v1/messages',1)`, u1, key.ID, aid, stamp+"/a", created, stamp+"/b")
	require.NoError(t, err)
	usage := &usageLogRepository{client: client, sql: db, db: db}
	for _, uid := range []int64{0, u1} {
		models, err := usage.GetModelStatsWithFilters(ctx, created, created.Add(time.Hour), uid, 0, aid, 0, nil, nil, nil)
		require.NoError(t, err)
		require.Len(t, models, 1)
		require.InDelta(t, 3, models[0].ActualCost, 1e-10)
		require.InDelta(t, 9, models[0].AccountCost, 1e-10)
		for _, dimension := range []string{"inbound_endpoint", "upstream_endpoint", "path"} {
			var endpoints []EndpointStat
			if dimension == "path" {
				endpoints, err = usage.getEndpointPathStatsWithFilters(ctx, created, created.Add(time.Hour), uid, 0, aid, 0, "", nil, nil, nil)
			} else {
				endpoints, err = usage.getEndpointStatsByColumnWithFilters(ctx, dimension, created, created.Add(time.Hour), uid, 0, aid, 0, "", nil, nil, nil)
			}
			require.NoError(t, err)
			require.Len(t, endpoints, 1)
			require.InDelta(t, 3, endpoints[0].ActualCost, 1e-10)
			require.InDelta(t, 9, endpoints[0].AccountCost, 1e-10)
			require.InDelta(t, 14, endpoints[0].Cost, 1e-10)
		}
	}
	accounts, ok := NewAccountRepository(client, db, nil).(*accountRepository)
	require.True(t, ok)
	old := `{"reason":"anthropic_7d_oi_window_exhausted","owner_token":"old","reset_at":"2026-12-01T00:00:00Z"}`
	newer := `{"reason":"anthropic_7d_oi_window_exhausted","owner_token":"new","reset_at":"2026-12-01T00:00:00Z"}`
	_, err = db.Exec(`UPDATE accounts SET extra=jsonb_build_object('model_rate_limits',jsonb_build_object('claude-fable-5',$2::jsonb,'other',jsonb_build_object('owner_token','keep'))),status='active',schedulable=true WHERE id=$1`, aid, newer)
	require.NoError(t, err)
	require.NoError(t, accounts.ClearModelRateLimitIfOwned(ctx, aid, "claude-fable-5", []byte(old)))
	var token string
	require.NoError(t, db.QueryRow(`SELECT extra->'model_rate_limits'->'claude-fable-5'->>'owner_token' FROM accounts WHERE id=$1`, aid).Scan(&token))
	require.Equal(t, "new", token)
	require.NoError(t, accounts.ClearModelRateLimitIfOwned(ctx, aid, "claude-fable-5", []byte(newer)))
	var scopeExists, otherExists, schedulable bool
	var status string
	require.NoError(t, db.QueryRow(`SELECT extra->'model_rate_limits' ? 'claude-fable-5',extra->'model_rate_limits' ? 'other',status,schedulable FROM accounts WHERE id=$1`, aid).Scan(&scopeExists, &otherExists, &status, &schedulable))
	require.False(t, scopeExists)
	require.True(t, otherExists)
	require.True(t, schedulable)
	require.Equal(t, "active", status)
}
