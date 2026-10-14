package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// Opt-in real PostgreSQL checks, independent of the Docker harness. Deliberately
// require a temporary database name to keep these fixture mutations isolated.
func TestGeminiCachePostgresTransactions(t *testing.T) {
	dsn := os.Getenv("NURO_CACHE_TEST_DSN")
	if dsn == "" {
		t.Skip("set NURO_CACHE_TEST_DSN to an isolated nuro_upgrade_verify database")
	}
	require.Contains(t, dsn, "dbname=nuro_upgrade_verify")
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.Ping())
	ctx := context.Background()
	require.NoError(t, timezone.Init("UTC"))
	require.NoError(t, ApplyMigrations(ctx, db))
	require.NoError(t, ApplyMigrations(ctx, db), "all migrations are idempotent")
	t.Run("channel storage pricing roundtrip", func(t *testing.T) {
		var channelID int64
		require.NoError(t, db.QueryRow(`INSERT INTO channels(name) VALUES($1) RETURNING id`, "cache-price-"+uuid.NewString()).Scan(&channelID))
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id=$1`, channelID) })
		repo := &channelRepository{db: db}
		zero, paid := 0.0, 0.00000125
		for _, storage := range []*float64{&paid, &zero, nil} {
			pricing := []service.ChannelModelPricing{{Models: []string{"gemini-3.6-flash"}, Platform: "gemini", InputPrice: &zero, CacheStoragePrice: storage}}
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			require.NoError(t, replaceModelPricingTx(ctx, tx, channelID, pricing))
			require.NoError(t, replaceAccountStatsPricingRulesTx(ctx, tx, channelID, []service.AccountStatsPricingRule{{Name: "storage", GroupIDs: []int64{}, AccountIDs: []int64{}, Pricing: pricing}}))
			require.NoError(t, tx.Commit())
			loaded, err := repo.ListModelPricing(ctx, channelID)
			require.NoError(t, err)
			require.Len(t, loaded, 1)
			rules, err := repo.loadAccountStatsPricingRules(ctx, channelID)
			require.NoError(t, err)
			require.Len(t, rules, 1)
			require.Len(t, rules[0].Pricing, 1)
			for _, p := range []service.ChannelModelPricing{loaded[0], rules[0].Pricing[0]} {
				require.Equal(t, &zero, p.InputPrice, "explicit zero survives alongside storage pricing")
				require.Equal(t, storage, p.CacheStoragePrice, "null, zero and paid storage prices remain distinct")
			}
		}
	})
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	resourceRepo := NewGeminiCachedContentRepository(db)
	chargeRepo := NewGeminiCacheChargeRepository(db, rdb)
	publisher := NewGeminiCacheAdmissionPublisher(db, NewBillingCache(rdb), NewAPIKeyCache(rdb))
	for _, subscription := range []bool{false, true} {
		t.Run(fmt.Sprintf("subscription=%t", subscription), func(t *testing.T) {
			stamp := strings.ReplaceAll(uuid.NewString(), "-", "")
			var userID, groupID, keyID, accountID, subID int64
			require.NoError(t, db.QueryRow(`INSERT INTO users(email,password_hash,balance) VALUES($1,'test',100) RETURNING id`, stamp+"@example.test").Scan(&userID))
			require.NoError(t, db.QueryRow(`INSERT INTO groups(name,platform,subscription_type,daily_limit_usd,weekly_limit_usd,monthly_limit_usd) VALUES($1,'gemini',$2,100,100,100) RETURNING id`, stamp, map[bool]string{true: "subscription", false: "standard"}[subscription]).Scan(&groupID))
			require.NoError(t, db.QueryRow(`INSERT INTO accounts(name,platform,type) VALUES($1,'gemini','apikey') RETURNING id`, stamp).Scan(&accountID))
			require.NoError(t, db.QueryRow(`INSERT INTO api_keys(user_id,group_id,key,name,quota) VALUES($1,$2,$3,'test',100) RETURNING id`, userID, groupID, "sk-"+stamp).Scan(&keyID))
			if subscription {
				require.NoError(t, db.QueryRow(`INSERT INTO user_subscriptions(user_id,group_id,starts_at,expires_at,status) VALUES($1,$2,NOW()-INTERVAL '1 day',NOW()+INTERVAL '30 days','active') RETURNING id`, userID, groupID).Scan(&subID))
			}
			t.Cleanup(func() {
				_, _ = db.Exec(`DELETE FROM gemini_cache_cancelled_intents WHERE user_id=$1`, userID)
				_, _ = db.Exec(`DELETE FROM gemini_cache_admin_actions WHERE operation_id IN (SELECT id FROM gemini_cache_operations WHERE user_id=$1)`, userID)
				_, _ = db.Exec(`DELETE FROM gemini_cache_charge_scopes WHERE operation_id IN (SELECT id FROM gemini_cache_operations WHERE user_id=$1)`, userID)
				_, _ = db.Exec(`DELETE FROM gemini_cache_charges WHERE operation_id IN (SELECT id FROM gemini_cache_operations WHERE user_id=$1)`, userID)
				_, _ = db.Exec(`DELETE FROM gemini_cache_operations WHERE user_id=$1`, userID)
				_, _ = db.Exec(`DELETE FROM gemini_cached_contents WHERE user_id=$1`, userID)
				_, _ = db.Exec(`DELETE FROM users WHERE id=$1`, userID)
				_, _ = db.Exec(`DELETE FROM groups WHERE id=$1`, groupID)
				_, _ = db.Exec(`DELETE FROM accounts WHERE id=$1`, accountID)
			})
			_, err = db.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, accountID, groupID)
			require.NoError(t, err)
			now := time.Now().UTC().Truncate(time.Microsecond)
			price := service.GeminiCachePricingContext{Version: 1, InputPerToken: "0.000001", StoragePerTokenHour: "0.000001", UserMultiplier: "1", AccountMultiplier: "1", PricingAt: now}
			quote, e := service.QuoteGeminiCache(price, 100000, now, now.Add(time.Hour), true)
			require.NoError(t, e)
			owner := service.GeminiCacheOwner{UserID: userID, APIKeyID: keyID, GroupID: groupID}
			// Cancellation and Prepare share the durable fence, including after
			// maintenance. Existing operations can never be cancelled as unsent.
			cancelRepo := resourceRepo.(interface {
				CancelUnrecordedOperation(context.Context, service.GeminiCacheOwner, string, string) error
				PruneCacheMetadata(context.Context) error
			})
			cancelHash, e := service.GeminiCacheIdempotencyHash("cancel-" + stamp)
			require.NoError(t, e)
			require.NoError(t, cancelRepo.CancelUnrecordedOperation(ctx, owner, "create", cancelHash))
			require.NoError(t, cancelRepo.CancelUnrecordedOperation(ctx, owner, "create", cancelHash))
			_, e = db.Exec(`UPDATE gemini_cache_cancelled_intents SET created_at=NOW()-INTERVAL '2 days' WHERE user_id=$1`, userID)
			require.NoError(t, e)
			require.NoError(t, cancelRepo.PruneCacheMetadata(ctx))
			cancelledResource := &service.GeminiCachedContent{ID: "nuro_" + stamp, Owner: owner, AccountID: accountID}
			cancelledOp := &service.GeminiCacheOperation{ID: "nuro_" + stamp, Owner: owner, AccountID: accountID, Kind: "create", IdempotencyKeyHash: cancelHash, LeaseOwner: "nuro_test_worker"}
			_, e = resourceRepo.Prepare(ctx, cancelledResource, cancelledOp, 10000)
			require.ErrorIs(t, e, service.ErrGeminiCacheRejected)
			_, e = db.Exec(`INSERT INTO gemini_cache_cancelled_intents(user_id,api_key_id,kind,idempotency_key_hash) SELECT $1,$2,'create',md5(i::text) FROM generate_series(1,999) i`, userID, keyID)
			require.NoError(t, e)
			nextHash, e := service.GeminiCacheIdempotencyHash("overflow-" + stamp)
			require.NoError(t, e)
			require.ErrorIs(t, cancelRepo.CancelUnrecordedOperation(ctx, owner, "create", nextHash), service.ErrGeminiCacheQuota)
			require.NoError(t, cancelRepo.CancelUnrecordedOperation(ctx, owner, "create", cancelHash), "same cancellation works at capacity")
			_, e = db.Exec(`DELETE FROM gemini_cache_cancelled_intents WHERE user_id=$1 AND idempotency_key_hash<>$2`, userID, cancelHash)
			require.NoError(t, e)
			prepare := func(kind string, resource *service.GeminiCachedContent, q service.GeminiCacheQuote) *service.GeminiCacheOperation {
				o := &service.GeminiCacheOperation{ID: "nuro_" + strings.ReplaceAll(uuid.NewString(), "-", ""), ResourceID: resource.ID, Owner: owner, AccountID: accountID, Kind: kind, Fingerprint: service.GeminiCacheFingerprint(owner, kind, resource.ID, []byte(kind)), Quote: q, TargetExpiry: q.PaidUntil, LeaseOwner: "nuro_test_worker"}
				if subscription {
					o.SubscriptionID = &subID
				}
				if kind != "create" {
					o.BaselineExpiry = &resource.ExpiresAt
				}
				_, e = resourceRepo.Prepare(ctx, resource, o, 10000)
				require.NoError(t, e)
				return o
			}
			makeResource := func() *service.GeminiCachedContent {
				return &service.GeminiCachedContent{ID: "nuro_" + strings.ReplaceAll(uuid.NewString(), "-", ""), Owner: owner, AccountID: accountID, RequestedModel: "gemini-3.6-flash", UpstreamModel: "gemini-3.6-flash", BillingModel: "gemini-3.6-flash", TokenCount: 100000, Pricing: price, ExpiresAt: quote.PaidUntil, PaidUntil: now}
			}
			if !subscription {
				require.NoError(t, NewBillingCache(rdb).SetUserPlatformQuotaCache(ctx, userID, "gemini", &service.UserPlatformQuotaCacheEntry{SchemaVersion: service.UserPlatformQuotaCacheSchemaV1}, time.Hour))
			}
			resource := makeResource()
			o := prepare("create", resource, quote)
			recordedHash, e := service.GeminiCacheIdempotencyHash("recorded-" + stamp)
			require.NoError(t, e)
			_, e = db.Exec(`UPDATE gemini_cache_operations SET idempotency_key_hash=$2 WHERE id=$1`, o.ID, recordedHash)
			require.NoError(t, e)
			require.ErrorIs(t, cancelRepo.CancelUnrecordedOperation(ctx, owner, "create", recordedHash), service.ErrGeminiCacheConflict)
			require.NoError(t, chargeRepo.ReserveCacheCharge(ctx, o))
			require.NoError(t, chargeRepo.ReserveCacheCharge(ctx, o), "reserve idempotent")
			require.NoError(t, publisher.PublishCacheChargeAdmission(ctx, o))
			var pending string
			require.NoError(t, db.QueryRow(`SELECT gemini_cache_pending::text FROM api_keys WHERE id=$1`, keyID).Scan(&pending))
			require.Equal(t, "0.20000000", pending)
			if !subscription {
				var balance, frozen float64
				require.NoError(t, db.QueryRow(`SELECT balance,frozen_balance FROM users WHERE id=$1`, userID).Scan(&balance, &frozen))
				require.InDelta(t, 99.8, balance, 1e-9)
				require.InDelta(t, .2, frozen, 1e-9)
			}
			require.NoError(t, resourceRepo.MarkSendStarted(ctx, o))
			require.ErrorIs(t, chargeRepo.ReleaseCacheCharge(ctx, o), service.ErrGeminiCacheConflict, "unknown execution cannot be refunded")
			require.NoError(t, resourceRepo.Confirm(ctx, o, "cachedContents/test", 100000, quote.PaidUntil))
			require.NoError(t, chargeRepo.CaptureCacheCharge(ctx, o, quote))
			require.NoError(t, chargeRepo.CaptureCacheCharge(ctx, o, quote), "capture idempotent")
			require.NoError(t, publisher.PublishCacheChargeAdmission(ctx, o))
			require.NoError(t, resourceRepo.Finish(ctx, o, "settled"))
			resource, e = resourceRepo.GetForOwner(ctx, owner, resource.ID)
			require.NoError(t, e)
			require.Equal(t, "active", resource.State)
			require.Equal(t, "0.2000000000", resource.Billing.SettledAmount)
			_, e = resourceRepo.GetForOwner(ctx, service.GeminiCacheOwner{UserID: userID + 1, APIKeyID: keyID, GroupID: groupID}, resource.ID)
			require.ErrorIs(t, e, service.ErrGeminiCacheNotFound)
			list, _, e := resourceRepo.ListForOwner(ctx, owner, "", 50)
			require.NoError(t, e)
			require.Len(t, list, 1)
			// An unsent hold survives old usage windows and can be released once only.
			unsent := makeResource()
			held := prepare("create", unsent, quote)
			require.NoError(t, chargeRepo.ReserveCacheCharge(ctx, held))
			_, e = db.Exec(`UPDATE api_keys SET window_5h_start=NOW()-INTERVAL '6 hours',usage_5h=99 WHERE id=$1`, keyID)
			require.NoError(t, e)
			require.NoError(t, chargeRepo.ReleaseCacheCharge(ctx, held))
			require.NoError(t, chargeRepo.ReleaseCacheCharge(ctx, held))
			require.NoError(t, resourceRepo.Finish(ctx, held, "released"))
			// Confirmed costs over the reservation retain funds for reconciliation.
			excess := makeResource()
			large := prepare("create", excess, quote)
			require.NoError(t, chargeRepo.ReserveCacheCharge(ctx, large))
			require.NoError(t, resourceRepo.MarkSendStarted(ctx, large))
			require.NoError(t, resourceRepo.Confirm(ctx, large, "cachedContents/excess", 200000, quote.PaidUntil))
			larger, e := service.QuoteGeminiCache(price, 200000, now, quote.PaidUntil, true)
			require.NoError(t, e)
			require.ErrorIs(t, chargeRepo.CaptureCacheCharge(ctx, large, larger), service.ErrGeminiCacheQuota)
			var chargeState string
			require.NoError(t, db.QueryRow(`SELECT state FROM gemini_cache_charges WHERE operation_id=$1`, large.ID).Scan(&chargeState))
			require.Equal(t, "reconcile_required", chargeState)
			_, e = db.Exec(`UPDATE gemini_cache_operations SET lease_owner=NULL,lease_until=NULL WHERE id=$1`, large.ID)
			require.NoError(t, e)
			topup := chargeRepo.(interface {
				TopUpConfirmedCacheCharge(context.Context, string, int64, string, string) error
			})
			require.ErrorIs(t, topup.TopUpConfirmedCacheCharge(ctx, large.ID, userID, "verify provider", "999.00000000"), service.ErrGeminiCacheConflict)
			_, e = db.Exec(`UPDATE api_keys SET quota=0.3 WHERE id=$1`, keyID)
			require.NoError(t, e)
			require.ErrorIs(t, topup.TopUpConfirmedCacheCharge(ctx, large.ID, userID, "verify provider", larger.Amount), service.ErrGeminiCacheQuota, "deficit must pass every original scope")
			require.NoError(t, db.QueryRow(`SELECT gemini_cache_pending::text FROM api_keys WHERE id=$1`, keyID).Scan(&pending))
			require.Equal(t, "0.20000000", pending, "failed topup rolls back all scopes")
			_, e = db.Exec(`UPDATE api_keys SET quota=100 WHERE id=$1`, keyID)
			require.NoError(t, e)
			require.NoError(t, topup.TopUpConfirmedCacheCharge(ctx, large.ID, userID, "verified provider count", larger.Amount))
			require.Error(t, topup.TopUpConfirmedCacheCharge(ctx, large.ID, userID, "duplicate topup", larger.Amount))
			claims, e := resourceRepo.ClaimRecovery(ctx, "nuro_test_reconcile", 50, 120*time.Second)
			require.NoError(t, e)
			for i := range claims {
				if claims[i].ID == large.ID {
					large = &claims[i]
				}
			}
			require.Equal(t, "upstream_confirmed", large.State)
			require.NoError(t, chargeRepo.CaptureCacheCharge(ctx, large, larger))
			require.NoError(t, chargeRepo.CaptureCacheCharge(ctx, large, larger), "one logical bill after deficit")
			require.NoError(t, publisher.PublishCacheChargeAdmission(ctx, large))
			require.NoError(t, resourceRepo.Finish(ctx, large, "settled"))
			require.NoError(t, db.QueryRow(`SELECT gemini_cache_pending::text FROM api_keys WHERE id=$1`, keyID).Scan(&pending))
			require.Equal(t, "0.00000000", pending)

			// Cross-node revocation resets the projection and blocks a stale grant.
			if subscription {
				before, err := resourceRepo.ReadView(ctx, 0, 10000, true)
				require.NoError(t, err)
				var subscriptionExpiry time.Time
				require.NoError(t, db.QueryRow(`SELECT expires_at FROM user_subscriptions WHERE id=$1`, subID).Scan(&subscriptionExpiry))
				require.NotNil(t, before.NextAuthorizationChange, "subscription natural expiry bounds the local view")
				require.True(t, before.NextAuthorizationChange.Equal(subscriptionExpiry))
				free := prepare("create", makeResource(), service.GeminiCacheQuote{Amount: "0.00000000"})
				incremental, err := resourceRepo.ReadView(ctx, before.Revision, 10000, false)
				require.NoError(t, err)
				require.False(t, incremental.Reset, "ordinary resource events use the incremental projection")
				require.NotEmpty(t, incremental.Bindings)
				require.NotNil(t, incremental.NextAuthorizationChange, "new resources recalculate natural authorization boundaries")
				require.True(t, incremental.NextAuthorizationChange.Equal(subscriptionExpiry))
				require.NoError(t, chargeRepo.ReserveCacheCharge(ctx, free))
				_, err = db.Exec(`UPDATE user_subscriptions SET starts_at=NOW()+INTERVAL '1 day' WHERE id=$1`, subID)
				require.NoError(t, err)
				require.ErrorIs(t, resourceRepo.MarkSendStarted(ctx, free), service.ErrGeminiCacheLease, "future subscription cannot authorize a free creation")
				future, err := resourceRepo.ReadView(ctx, before.Revision, 10000, false)
				require.NoError(t, err)
				require.True(t, future.Reset, "start-time edits invalidate every node")
				for _, binding := range future.Bindings {
					if binding.ID == excess.ID {
						require.Equal(t, "unavailable", binding.State)
					}
				}
				_, err = db.Exec(`UPDATE user_subscriptions SET starts_at=NOW()-INTERVAL '1 day' WHERE id=$1`, subID)
				require.NoError(t, err)
				_, err = db.Exec(`UPDATE user_subscriptions SET status='expired' WHERE id=$1`, subID)
				require.NoError(t, err)
				require.ErrorIs(t, resourceRepo.MarkSendStarted(ctx, free), service.ErrGeminiCacheLease, "revoked subscription blocks zero-priced creation")
				revoked, err := resourceRepo.ReadView(ctx, before.Revision, 10000, false)
				require.NoError(t, err)
				require.True(t, revoked.Reset, "subscription authorization changes invalidate every node")
				for _, binding := range revoked.Bindings {
					if binding.ID == excess.ID {
						require.Equal(t, "unavailable", binding.State)
					}
				}
				require.NoError(t, chargeRepo.ReleaseCacheCharge(ctx, free))
				require.NoError(t, resourceRepo.Finish(ctx, free, "released"))
				_, err = db.Exec(`UPDATE user_subscriptions SET status='active' WHERE id=$1`, subID)
				require.NoError(t, err)
			}
			baselineView, e := resourceRepo.ReadView(ctx, 0, 10000, true)
			require.NoError(t, e)
			require.NoError(t, resourceRepo.LeaseView(ctx, "nuro_test_authorization", baselineView.Revision, 10*time.Second))
			_, e = db.Exec(`UPDATE accounts SET schedulable=false WHERE id=$1`, accountID)
			require.NoError(t, e)
			revoked, e := resourceRepo.ReadView(ctx, baselineView.Revision, 10000, false)
			require.NoError(t, e)
			require.True(t, revoked.Reset)
			var unavailable bool
			for _, binding := range revoked.Bindings {
				if binding.ID == excess.ID {
					unavailable = binding.State == "unavailable"
				}
			}
			require.True(t, unavailable)
			require.ErrorIs(t, resourceRepo.LeaseView(ctx, "nuro_test_authorization", baselineView.Revision, 10*time.Second), service.ErrGeminiCacheUnavailable)
			_, e = db.Exec(`UPDATE accounts SET schedulable=true WHERE id=$1`, accountID)
			require.NoError(t, e)
			_, e = db.Exec(`DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, accountID, groupID)
			require.NoError(t, e)
			membership, e := resourceRepo.ReadView(ctx, revoked.Revision, 10000, false)
			require.NoError(t, e)
			require.True(t, membership.Reset)
			_, _ = db.Exec(`DELETE FROM gemini_cache_view_nodes WHERE node_id='nuro_test_authorization'`)
			// Delayed free mutations must revalidate permission before sending.
			blocked := prepare("create", makeResource(), service.GeminiCacheQuote{Amount: "0.00000000"})
			require.NoError(t, chargeRepo.ReserveCacheCharge(ctx, blocked))
			require.ErrorIs(t, resourceRepo.MarkSendStarted(ctx, blocked), service.ErrGeminiCacheLease, "removed account membership blocks sending")
			_, e = db.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, accountID, groupID)
			require.NoError(t, e)
			_, e = db.Exec(`UPDATE api_keys SET status='disabled' WHERE id=$1`, keyID)
			require.NoError(t, e)
			require.ErrorIs(t, resourceRepo.MarkSendStarted(ctx, blocked), service.ErrGeminiCacheLease, "disabled key blocks sending")
			_, e = db.Exec(`UPDATE api_keys SET status='active',expires_at=NOW()-INTERVAL '1 day' WHERE id=$1`, keyID)
			require.NoError(t, e)
			require.ErrorIs(t, resourceRepo.MarkSendStarted(ctx, blocked), service.ErrGeminiCacheLease, "expired key cannot create even at zero price")
			require.NoError(t, chargeRepo.ReleaseCacheCharge(ctx, blocked))
			require.NoError(t, resourceRepo.Finish(ctx, blocked, "released"))
			// Zero-cost deletion remains possible even when the key is exhausted.
			_, e = db.Exec(`UPDATE api_keys SET quota_used=100,status='quota_exhausted' WHERE id=$1`, keyID)
			require.NoError(t, e)
			zero, e := service.QuoteGeminiCache(price, 100000, resource.PaidUntil, resource.PaidUntil, false)
			require.NoError(t, e)
			remove := prepare("delete", resource, zero)
			require.NoError(t, chargeRepo.ReserveCacheCharge(ctx, remove))
			require.NoError(t, resourceRepo.MarkSendStarted(ctx, remove))
			require.NoError(t, resourceRepo.Confirm(ctx, remove, resource.UpstreamName, resource.TokenCount, resource.ExpiresAt))
			require.NoError(t, chargeRepo.CaptureCacheCharge(ctx, remove, zero))
			require.NoError(t, resourceRepo.Finish(ctx, remove, "settled"))
			// A stale node cannot extend permission after a newer resource revision.
			batch, e := resourceRepo.ReadView(ctx, 0, 10000, true)
			require.NoError(t, e)
			require.True(t, batch.Complete)
			require.NoError(t, resourceRepo.LeaseView(ctx, "nuro_test_node", batch.Revision, time.Second))
			_, e = db.Exec(`UPDATE gemini_cache_view_clock SET revision=revision+1 WHERE id=1`)
			require.NoError(t, e)
			require.ErrorIs(t, resourceRepo.LeaseView(ctx, "nuro_test_node", batch.Revision, time.Second), service.ErrGeminiCacheUnavailable)
			ok, e := resourceRepo.Barrier(ctx, batch.Revision+1)
			require.NoError(t, e)
			require.False(t, ok)
			_, _ = db.Exec(`DELETE FROM gemini_cache_view_nodes WHERE node_id='nuro_test_node'`)
			// Flusher may not overwrite a newer generation or the separate settled fields.
			var oldGeneration, newGeneration int64
			require.NoError(t, db.QueryRow(`SELECT gemini_cache_generation FROM api_keys WHERE id=$1`, keyID).Scan(&oldGeneration))
			_, e = db.Exec(`UPDATE api_keys SET quota_used=0,usage_5h=0,usage_1d=0,usage_7d=0 WHERE id=$1`, keyID)
			require.NoError(t, e)
			require.NoError(t, db.QueryRow(`SELECT gemini_cache_generation FROM api_keys WHERE id=$1`, keyID).Scan(&newGeneration))
			require.Greater(t, newGeneration, oldGeneration, "manual reset fences delayed financial snapshots")
			if subscription {
				require.NoError(t, db.QueryRow(`SELECT gemini_cache_generation FROM user_subscriptions WHERE id=$1`, subID).Scan(&oldGeneration))
				_, e = db.Exec(`UPDATE user_subscriptions SET daily_usage_usd=0,weekly_usage_usd=0,monthly_usage_usd=0 WHERE id=$1`, subID)
				require.NoError(t, e)
				require.NoError(t, db.QueryRow(`SELECT gemini_cache_generation FROM user_subscriptions WHERE id=$1`, subID).Scan(&newGeneration))
				require.Greater(t, newGeneration, oldGeneration)
			}
			_, e = db.Exec(`INSERT INTO user_platform_quotas(user_id,platform) VALUES($1,'gemini') ON CONFLICT(user_id,platform) WHERE deleted_at IS NULL DO NOTHING`, userID)
			require.NoError(t, e)
			var gen int64
			require.NoError(t, db.QueryRow(`SELECT gemini_cache_generation FROM user_platform_quotas WHERE user_id=$1 AND platform='gemini' AND deleted_at IS NULL`, userID).Scan(&gen))
			var id int64
			require.NoError(t, db.QueryRow(`INSERT INTO user_platform_quotas(user_id,platform,daily_usage_usd,gemini_cache_generation) VALUES($1,'gemini',3,$2) ON CONFLICT(user_id,platform) WHERE deleted_at IS NULL DO UPDATE SET daily_usage_usd=EXCLUDED.daily_usage_usd WHERE user_platform_quotas.gemini_cache_generation=EXCLUDED.gemini_cache_generation RETURNING id`, userID, gen).Scan(&id))
			// Exercise the real flusher SQL: a normal period rollover must keep
			// accepting later snapshots, while an admin reset fences old ones.
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			platformRepo := NewUserPlatformQuotaRepository(client)
			if !subscription {
				record, err := NewUserPlatformQuotaServiceAdapter(platformRepo).GetByUserPlatform(ctx, userID, "gemini")
				require.NoError(t, err)
				require.NotNil(t, record)
				for _, amount := range []float64{record.GeminiCacheDailyUsage, record.GeminiCacheWeeklyUsage, record.GeminiCacheMonthlyUsage} {
					require.InDelta(t, .6, amount, 1e-9, "DB fallback retains settled cache charges after ordinary resets")
				}
				require.NotNil(t, record.GeminiCacheDailyStart)
				require.NotNil(t, record.GeminiCacheWeeklyStart)
				require.NotNil(t, record.GeminiCacheMonthlyStart)
			}
			snapshot := UserPlatformQuotaSnapshot{UserID: userID, Platform: "gemini", GeminiCacheGeneration: gen, DailyUsageUSD: 1, DailyWindowStart: now, WeeklyWindowStart: now, MonthlyWindowStart: now}
			require.NoError(t, platformRepo.BatchSnapshotUsage(ctx, []UserPlatformQuotaSnapshot{snapshot}, now))
			var usage float64
			require.NoError(t, db.QueryRow(`SELECT gemini_cache_generation,daily_usage_usd FROM user_platform_quotas WHERE id=$1`, id).Scan(&newGeneration, &usage))
			require.Equal(t, gen, newGeneration, "rollover must not strand Redis at an older generation")
			require.Equal(t, 1.0, usage)
			snapshot.DailyUsageUSD = 2
			require.NoError(t, platformRepo.BatchSnapshotUsage(ctx, []UserPlatformQuotaSnapshot{snapshot}, now))
			require.NoError(t, db.QueryRow(`SELECT daily_usage_usd FROM user_platform_quotas WHERE id=$1`, id).Scan(&usage))
			require.Equal(t, 2.0, usage)
			require.NoError(t, platformRepo.ResetExpiredWindow(ctx, userID, "gemini", "daily", now))
			require.NoError(t, db.QueryRow(`SELECT gemini_cache_generation FROM user_platform_quotas WHERE id=$1`, id).Scan(&newGeneration))
			if gen > 0 {
				require.Equal(t, gen+1, newGeneration)
				require.NoError(t, platformRepo.BatchSnapshotUsage(ctx, []UserPlatformQuotaSnapshot{snapshot}, now))
				require.NoError(t, db.QueryRow(`SELECT daily_usage_usd FROM user_platform_quotas WHERE id=$1`, id).Scan(&usage))
				require.Zero(t, usage, "pre-reset snapshot cannot undo an admin reset")
			} else {
				require.Zero(t, newGeneration, "ordinary owners retain generation zero")
			}
			snapshot.GeminiCacheGeneration = newGeneration
			require.NoError(t, platformRepo.BatchSnapshotUsage(ctx, []UserPlatformQuotaSnapshot{snapshot}, now))
			require.NoError(t, db.QueryRow(`SELECT daily_usage_usd FROM user_platform_quotas WHERE id=$1`, id).Scan(&usage))
			require.Equal(t, 2.0, usage, "new-generation snapshots resume normally")
			// Redis fences reject arbitrarily delayed older financial fills.
			cache := NewBillingCache(rdb).(*billingCache)
			require.NoError(t, cache.SetUserBalanceWithGeneration(ctx, userID, 1, 99))
			require.NoError(t, cache.SetUserBalanceWithGeneration(ctx, userID, 100, 98))
			got, e := cache.GetUserBalance(ctx, userID)
			require.NoError(t, e)
			require.Equal(t, 1.0, got)
			public, e := json.Marshal(resource.PublicResponse())
			require.NoError(t, e)
			require.NotContains(t, string(public), "cachedContents/test")
			require.NotContains(t, string(public), "account_id")
		})
	}
}
