package repository

import (
	"context"
	"database/sql"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/redis/go-redis/v9"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
)

type geminiCacheChargeRepository struct {
	db  *sql.DB
	rdb *redis.Client
}

func NewGeminiCacheChargeRepository(db *sql.DB, rdb *redis.Client) service.GeminiCacheChargeRepository {
	return &geminiCacheChargeRepository{db: db, rdb: rdb}
}
func cacheChargeAmount(value string) (decimal.Decimal, error) {
	if len(value) == 0 || len(value) > 64 {
		return decimal.Zero, service.ErrGeminiCacheQuota
	}
	amount, err := decimal.NewFromString(value)
	if err != nil || amount.IsNegative() || amount.GreaterThanOrEqual(decimal.NewFromInt(10000000000)) {
		return decimal.Zero, service.ErrGeminiCacheQuota
	}
	return amount.RoundCeil(8), nil
}
func cacheQuotaAvailable(limit, used, pending, amount string) bool {
	l, e := decimal.NewFromString(limit)
	if e != nil {
		return false
	}
	if l.IsZero() {
		return true
	}
	u, e := decimal.NewFromString(used)
	if e != nil {
		return false
	}
	p, e := decimal.NewFromString(pending)
	if e != nil {
		return false
	}
	a, e := decimal.NewFromString(amount)
	return e == nil && u.Add(p).Add(a).LessThanOrEqual(l)
}

// The financial rows follow ordinary billing's subscription -> user -> key ->
// platform -> account order. Operations are locked last, and no provider call
// occurs inside a transaction. Dedicated pending columns survive every legacy
// usage reset; refunds never subtract historical reservations from new usage.
func (r *geminiCacheChargeRepository) ReserveCacheCharge(ctx context.Context, o *service.GeminiCacheOperation) error {
	return r.reserveCacheCharge(ctx, o, nil, 0, "")
}

// TopUpConfirmedCacheCharge only reserves the verified deficit. It never sends
// a provider mutation or changes the original operation's immutable price card.
func (r *geminiCacheChargeRepository) TopUpConfirmedCacheCharge(ctx context.Context, id string, actor int64, reason, expected string) error {
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	reason = strings.TrimSpace(reason)
	if !service.ValidGeminiCachePublicID(id) || actor <= 0 || len(reason) < 3 || len(reason) > 256 {
		return service.ErrGeminiCacheConflict
	}
	o, err := scanCacheOperation(r.db.QueryRowContext(ctx, `SELECT `+cacheOperationColumns+` FROM gemini_cache_operations WHERE id=$1`, id))
	if err != nil {
		return err
	}
	actual, err := service.ConfirmedGeminiCacheQuote(o)
	expectedCost, expectedErr := cacheChargeAmount(expected)
	actualCost, actualErr := cacheChargeAmount(actual.Amount)
	if err != nil || expectedErr != nil || actualErr != nil || !actualCost.Equal(expectedCost) {
		return service.ErrGeminiCacheConflict
	}
	return r.reserveCacheCharge(ctx, o, &actual, actor, reason)
}

func (r *geminiCacheChargeRepository) reserveCacheCharge(ctx context.Context, o *service.GeminiCacheOperation, actual *service.GeminiCacheQuote, actor int64, reason string) error {
	amount, err := cacheChargeAmount(o.Quote.Amount)
	if err != nil {
		return err
	}
	var originalHeld string
	if actual != nil {
		if err = r.db.QueryRowContext(ctx, `SELECT reserved_amount::text FROM gemini_cache_charges WHERE operation_id=$1`, o.ID).Scan(&originalHeld); err != nil {
			return err
		}
		held, e := cacheChargeAmount(originalHeld)
		if e != nil {
			return e
		}
		confirmed, e := cacheChargeAmount(actual.Amount)
		if e != nil {
			return e
		}
		amount = confirmed.Sub(held)
		if !amount.IsPositive() {
			return service.ErrGeminiCacheConflict
		}
	}
	cost := amount.StringFixed(8)
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	var platformUsage [3]decimal.Decimal
	if o.SubscriptionID == nil && amount.IsPositive() {
		if r.rdb == nil {
			return service.ErrGeminiCacheUnavailable
		}
		fields, e := r.rdb.HGetAll(ctx, userPlatformQuotaCacheKey(o.Owner.UserID, "gemini")).Result()
		if e != nil {
			return e
		}
		entry := parseUserPlatformQuotaHash(fields)
		if entry == nil || entry.SchemaVersion != service.UserPlatformQuotaCacheSchemaV1 {
			return service.ErrGeminiCacheUnavailable
		}
		now := time.Now()
		values := []float64{entry.DailyUsageUSD, entry.WeeklyUsageUSD, entry.MonthlyUsageUSD}
		starts := []*time.Time{entry.DailyWindowStart, entry.WeeklyWindowStart, entry.MonthlyWindowStart}
		for i, v := range values {
			expired := starts[i] == nil
			if !expired {
				switch i {
				case 0:
					expired = starts[i].Before(timezone.StartOfDay(now))
				case 1:
					expired = starts[i].Before(timezone.StartOfWeek(now))
				case 2:
					expired = now.Sub(*starts[i]) >= 30*24*time.Hour
				}
			}
			if !expired {
				platformUsage[i] = decimal.NewFromFloat(v)
			}
		}
	}
	tx, err := beginCacheTx(ctx, r.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if amount.IsPositive() {
		if err = lockCacheFinancialOwners(ctx, tx, o); err != nil {
			return err
		}
	}
	if amount.IsPositive() {
		if err = normalizeCacheSubscriptionWindows(ctx, tx, o); err != nil {
			return err
		}
	}
	stored, err := scanCacheOperation(tx.QueryRowContext(ctx, `SELECT `+cacheOperationColumns+` FROM gemini_cache_operations WHERE id=$1 FOR UPDATE`, o.ID))
	if err != nil {
		return err
	}
	if actual == nil && stored.State == "reserved" && stored.Fingerprint == o.Fingerprint {
		return nil
	}
	if actual == nil {
		if stored.State != "prepared" || stored.Generation != o.Generation || stored.LeaseOwner != o.LeaseOwner || stored.LeaseUntil == nil || !stored.LeaseUntil.After(time.Now()) {
			return service.ErrGeminiCacheLease
		}
		if stored.Fingerprint != o.Fingerprint || stored.Quote.Amount != o.Quote.Amount {
			return service.ErrGeminiCacheConflict
		}
	} else {
		if (stored.State != "reconcile_required" && stored.State != "upstream_confirmed") || stored.Generation != o.Generation || (stored.LeaseUntil != nil && stored.LeaseUntil.After(time.Now().Add(-45*time.Second))) {
			return service.ErrGeminiCacheLease
		}
		verified, e := service.ConfirmedGeminiCacheQuote(stored)
		if e != nil || verified.Amount != actual.Amount || !service.SameGeminiCachePricing(verified.Pricing, actual.Pricing) {
			return service.ErrGeminiCacheConflict
		}
		var held, chargeState string
		if e = tx.QueryRowContext(ctx, `SELECT reserved_amount::text,state FROM gemini_cache_charges WHERE operation_id=$1 FOR UPDATE`, o.ID).Scan(&held, &chargeState); e != nil {
			return e
		}
		if held != originalHeld || chargeState != "reconcile_required" {
			return service.ErrGeminiCacheConflict
		}
	}
	if amount.IsZero() {
		if _, err = tx.ExecContext(ctx, `INSERT INTO gemini_cache_charges(operation_id,bill_id,reserved_amount,state) VALUES($1,$2,0,'reserved')`, o.ID, "gc-"+o.ID); err != nil {
			return err
		}
		res, e := tx.ExecContext(ctx, `UPDATE gemini_cache_operations SET state='reserved',updated_at=NOW() WHERE id=$1 AND generation=$2 AND lease_owner=$3 AND lease_until>clock_timestamp()`, o.ID, o.Generation, o.LeaseOwner)
		if e != nil {
			return e
		}
		if e = requireCacheAffected(res); e != nil {
			return e
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		o.State = "reserved"
		return nil
	}
	var n int64
	if o.SubscriptionID == nil {
		res, e := tx.ExecContext(ctx, `UPDATE users SET balance=balance-$1::numeric,frozen_balance=frozen_balance+$1::numeric,gemini_cache_generation=gemini_cache_generation+1,updated_at=NOW() WHERE id=$2 AND status='active' AND deleted_at IS NULL AND balance>=$1::numeric`, cost, o.Owner.UserID)
		if e != nil {
			return e
		}
		n, e = res.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return service.ErrGeminiCacheQuota
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO gemini_cache_charge_scopes(operation_id,scope,owner_id,amount,generation,state) VALUES($1,'balance',$2,$3,0,'pending') ON CONFLICT(operation_id,scope) DO UPDATE SET amount=gemini_cache_charge_scopes.amount+EXCLUDED.amount,generation=EXCLUDED.generation WHERE gemini_cache_charge_scopes.state='pending'`, o.ID, o.Owner.UserID, cost); err != nil {
			return err
		}
	} else {
		var pending, daily, weekly, monthly, dLimit, wLimit, mLimit string
		err = tx.QueryRowContext(ctx, `SELECT s.gemini_cache_pending,s.daily_usage_usd,s.weekly_usage_usd,s.monthly_usage_usd,COALESCE(g.daily_limit_usd,0),COALESCE(g.weekly_limit_usd,0),COALESCE(g.monthly_limit_usd,0) FROM user_subscriptions s JOIN groups g ON g.id=s.group_id WHERE s.id=$1 AND s.user_id=$2 AND s.group_id=$3 AND s.status='active' AND s.deleted_at IS NULL AND s.expires_at>NOW() AND s.starts_at<=NOW() AND g.deleted_at IS NULL AND g.status='active'`, *o.SubscriptionID, o.Owner.UserID, o.Owner.GroupID).Scan(&pending, &daily, &weekly, &monthly, &dLimit, &wLimit, &mLimit)
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrGeminiCacheQuota
		}
		if err != nil {
			return err
		}
		if !cacheQuotaAvailable(dLimit, daily, pending, cost) || !cacheQuotaAvailable(wLimit, weekly, pending, cost) || !cacheQuotaAvailable(mLimit, monthly, pending, cost) {
			return service.ErrGeminiCacheQuota
		}
		var generation int64
		if err = tx.QueryRowContext(ctx, `UPDATE user_subscriptions SET gemini_cache_pending=gemini_cache_pending+$1,gemini_cache_generation=gemini_cache_generation+1,updated_at=NOW() WHERE id=$2 RETURNING gemini_cache_generation`, cost, *o.SubscriptionID).Scan(&generation); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO gemini_cache_charge_scopes(operation_id,scope,owner_id,amount,generation,state) VALUES($1,'subscription',$2,$3,$4,'pending') ON CONFLICT(operation_id,scope) DO UPDATE SET amount=gemini_cache_charge_scopes.amount+EXCLUDED.amount,generation=EXCLUDED.generation WHERE gemini_cache_charge_scopes.state='pending'`, o.ID, *o.SubscriptionID, cost, generation); err != nil {
			return err
		}
	}
	var quota, used, pending string
	var rates [3]string
	var usages [3]string
	err = tx.QueryRowContext(ctx, `SELECT quota,quota_used,gemini_cache_pending,rate_limit_5h,rate_limit_1d,rate_limit_7d,CASE WHEN window_5h_start IS NULL OR window_5h_start+INTERVAL '5 hours'<=NOW() THEN 0 ELSE usage_5h END,CASE WHEN window_1d_start IS NULL OR window_1d_start+INTERVAL '24 hours'<=NOW() THEN 0 ELSE usage_1d END,CASE WHEN window_7d_start IS NULL OR window_7d_start+INTERVAL '7 days'<=NOW() THEN 0 ELSE usage_7d END FROM api_keys WHERE id=$1 AND user_id=$2 AND group_id=$3 AND status='active' AND deleted_at IS NULL AND (expires_at IS NULL OR expires_at>NOW())`, o.Owner.APIKeyID, o.Owner.UserID, o.Owner.GroupID).Scan(&quota, &used, &pending, &rates[0], &rates[1], &rates[2], &usages[0], &usages[1], &usages[2])
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrGeminiCacheQuota
	}
	if err != nil {
		return err
	}
	if !cacheQuotaAvailable(quota, used, pending, cost) {
		return service.ErrGeminiCacheQuota
	}
	for i := range rates {
		if !cacheQuotaAvailable(rates[i], usages[i], pending, cost) {
			return service.ErrGeminiCacheQuota
		}
	}
	var keyGeneration int64
	if err = tx.QueryRowContext(ctx, `UPDATE api_keys SET gemini_cache_pending=gemini_cache_pending+$1,gemini_cache_generation=gemini_cache_generation+1,updated_at=NOW() WHERE id=$2 RETURNING gemini_cache_generation`, cost, o.Owner.APIKeyID).Scan(&keyGeneration); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO gemini_cache_charge_scopes(operation_id,scope,owner_id,amount,generation,state) VALUES($1,'key',$2,$3,$4,'pending') ON CONFLICT(operation_id,scope) DO UPDATE SET amount=gemini_cache_charge_scopes.amount+EXCLUDED.amount,generation=EXCLUDED.generation WHERE gemini_cache_charge_scopes.state='pending'`, o.ID, o.Owner.APIKeyID, cost, keyGeneration); err != nil {
		return err
	}
	if o.SubscriptionID == nil {
		var id int64
		var gen int64
		var limits [3]string
		var usage [3]string
		var held string
		var settled [3]string
		var starts [3]*time.Time
		var ordinaryStarts [3]*time.Time
		err = tx.QueryRowContext(ctx, `SELECT id,gemini_cache_pending,COALESCE(daily_limit_usd,0),COALESCE(weekly_limit_usd,0),COALESCE(monthly_limit_usd,0),daily_usage_usd,weekly_usage_usd,monthly_usage_usd,gemini_cache_daily_usage,gemini_cache_weekly_usage,gemini_cache_monthly_usage,gemini_cache_daily_start,gemini_cache_weekly_start,gemini_cache_monthly_start,daily_window_start,weekly_window_start,monthly_window_start FROM user_platform_quotas WHERE user_id=$1 AND platform='gemini' AND deleted_at IS NULL`, o.Owner.UserID).Scan(&id, &held, &limits[0], &limits[1], &limits[2], &usage[0], &usage[1], &usage[2], &settled[0], &settled[1], &settled[2], &starts[0], &starts[1], &starts[2], &ordinaryStarts[0], &ordinaryStarts[1], &ordinaryStarts[2])
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrGeminiCacheQuota
		}
		if err != nil {
			return err
		}
		now := time.Now()
		for i := range limits {
			current := func(start *time.Time) bool {
				if start == nil {
					return false
				}
				switch i {
				case 0:
					return !start.Before(timezone.StartOfDay(now))
				case 1:
					return !start.Before(timezone.StartOfWeek(now))
				default:
					return now.Sub(*start) < 30*24*time.Hour
				}
			}
			ordinary, e := decimal.NewFromString(usage[i])
			if e != nil {
				return e
			}
			if !current(ordinaryStarts[i]) {
				ordinary = decimal.Zero
			}
			ordinary = decimal.Max(ordinary, platformUsage[i])
			if current(starts[i]) {
				paid, e := decimal.NewFromString(settled[i])
				if e != nil {
					return e
				}
				ordinary = ordinary.Add(paid)
			}
			usage[i] = ordinary.String()
			if !cacheQuotaAvailable(limits[i], usage[i], held, cost) {
				return service.ErrGeminiCacheQuota
			}
		}
		if err = tx.QueryRowContext(ctx, `UPDATE user_platform_quotas SET gemini_cache_pending=gemini_cache_pending+$1,gemini_cache_generation=gemini_cache_generation+1,updated_at=NOW() WHERE id=$2 RETURNING gemini_cache_generation`, cost, id).Scan(&gen); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO gemini_cache_charge_scopes(operation_id,scope,owner_id,amount,generation,state) VALUES($1,'platform',$2,$3,$4,'pending') ON CONFLICT(operation_id,scope) DO UPDATE SET amount=gemini_cache_charge_scopes.amount+EXCLUDED.amount,generation=EXCLUDED.generation WHERE gemini_cache_charge_scopes.state='pending'`, o.ID, id, cost, gen); err != nil {
			return err
		}
	}
	if actual != nil {
		if _, err = tx.ExecContext(ctx, `UPDATE gemini_cache_charges SET reserved_amount=$2,state='reserved',updated_at=NOW() WHERE operation_id=$1`, o.ID, actual.Amount); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE gemini_cache_operations SET state='upstream_confirmed',generation=generation+1,lease_owner=NULL,lease_until=NULL,attempts=0,next_attempt_at=NOW(),updated_at=NOW() WHERE id=$1`, o.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO gemini_cache_admin_actions(operation_id,actor_user_id,action,reason) VALUES($1,$2,'reserve_deficit',$3)`, o.ID, actor, reason); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO gemini_cache_charges(operation_id,bill_id,reserved_amount,state) VALUES($1,$2,$3,'reserved')`, o.ID, "gc-"+o.ID, cost); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE gemini_cache_operations SET state='reserved',updated_at=NOW() WHERE id=$1 AND generation=$2 AND lease_until>clock_timestamp()`, o.ID, o.Generation)
	if err != nil {
		return err
	}
	if err = requireCacheAffected(res); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	o.State = "reserved"
	return nil
}

func lockCacheFinancialOwners(ctx context.Context, tx *sql.Tx, o *service.GeminiCacheOperation) error {
	lock := func(query string, args ...any) error {
		var id int64
		err := tx.QueryRowContext(ctx, query, args...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrGeminiCacheQuota
		}
		return err
	}
	if o.SubscriptionID != nil {
		if err := lock(`SELECT id FROM user_subscriptions WHERE id=$1 FOR UPDATE`, *o.SubscriptionID); err != nil {
			return err
		}
	}
	if err := lock(`SELECT id FROM users WHERE id=$1 FOR UPDATE`, o.Owner.UserID); err != nil {
		return err
	}
	if err := lock(`SELECT id FROM api_keys WHERE id=$1 FOR UPDATE`, o.Owner.APIKeyID); err != nil {
		return err
	}
	if o.SubscriptionID == nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_platform_quotas(user_id,platform) VALUES($1,'gemini') ON CONFLICT(user_id,platform) WHERE deleted_at IS NULL DO NOTHING`, o.Owner.UserID); err != nil {
			return err
		}
		if err := lock(`SELECT id FROM user_platform_quotas WHERE user_id=$1 AND platform='gemini' AND deleted_at IS NULL FOR UPDATE`, o.Owner.UserID); err != nil {
			return err
		}
	}
	return lock(`SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, o.AccountID)
}

func (r *geminiCacheChargeRepository) CaptureCacheCharge(ctx context.Context, o *service.GeminiCacheOperation, quote service.GeminiCacheQuote) error {
	return r.finishCharge(ctx, o, &quote)
}
func (r *geminiCacheChargeRepository) ReleaseCacheCharge(ctx context.Context, o *service.GeminiCacheOperation) error {
	return r.finishCharge(ctx, o, nil)
}
func (r *geminiCacheChargeRepository) finishCharge(ctx context.Context, o *service.GeminiCacheOperation, actual *service.GeminiCacheQuote) error {
	quoted, err := cacheChargeAmount(o.Quote.Amount)
	if err != nil {
		return err
	}
	ctx, cancel := cacheDBTimeout(ctx)
	defer cancel()
	tx, err := beginCacheTx(ctx, r.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if quoted.IsPositive() || (actual != nil && actual.Amount != "0.00000000") {
		if err = lockCacheFinancialOwners(ctx, tx, o); err != nil {
			return err
		}
	}
	if quoted.IsZero() && actual != nil && actual.Amount == "0.00000000" && actual.AccountAmount != "0.00000000" {
		var accountID int64
		if err = tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, o.AccountID).Scan(&accountID); err != nil {
			return err
		}
	}
	if o.Quote.Amount != "0.00000000" {
		if err = normalizeCacheSubscriptionWindows(ctx, tx, o); err != nil {
			return err
		}
	}
	stored, err := scanCacheOperation(tx.QueryRowContext(ctx, `SELECT `+cacheOperationColumns+` FROM gemini_cache_operations WHERE id=$1 FOR UPDATE`, o.ID))
	if err != nil {
		return err
	}
	if stored.Generation != o.Generation || stored.LeaseOwner != o.LeaseOwner || stored.LeaseUntil == nil || !stored.LeaseUntil.After(time.Now()) {
		return service.ErrGeminiCacheLease
	}
	var held, state string
	err = tx.QueryRowContext(ctx, `SELECT reserved_amount,state FROM gemini_cache_charges WHERE operation_id=$1 FOR UPDATE`, o.ID).Scan(&held, &state)
	if errors.Is(err, sql.ErrNoRows) && stored.State == "prepared" && actual == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO gemini_cache_charges(operation_id,bill_id,reserved_amount,state) VALUES($1,$2,0,'released')`, o.ID, "gc-"+o.ID)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	target := "settled"
	if actual == nil {
		target = "released"
	}
	if state == target {
		return nil
	}
	if state == "settled" || state == "released" {
		return service.ErrGeminiCacheConflict
	}
	reserved, err := cacheChargeAmount(held)
	if err != nil {
		return err
	}
	amount := decimal.Zero
	input, storage := "0", "0"
	if actual != nil {
		if stored.State != "upstream_confirmed" {
			return service.ErrGeminiCacheConflict
		}
		if stored.ConfirmedTokens == nil || stored.ConfirmedExpiry == nil || actual.Tokens != *stored.ConfirmedTokens || !actual.PaidFrom.Equal(stored.Quote.PaidFrom) || !actual.PaidUntil.Equal(maxCacheTime(stored.Quote.PaidFrom, *stored.ConfirmedExpiry)) || !service.SameGeminiCachePricing(actual.Pricing, stored.Quote.Pricing) {
			return service.ErrGeminiCacheConflict
		}
		amount, err = cacheChargeAmount(actual.Amount)
		if err != nil {
			return err
		}
		input, storage = actual.InputCost, actual.StorageCost
		if amount.GreaterThan(reserved) {
			res, e := tx.ExecContext(ctx, `UPDATE gemini_cache_charges SET actual_amount=$2,input_cost=$3,storage_cost=$4,state='reconcile_required',updated_at=NOW() WHERE operation_id=$1`, o.ID, amount.StringFixed(8), input, storage)
			if e != nil {
				return e
			}
			if e = requireCacheAffected(res); e != nil {
				return e
			}
			if e = tx.Commit(); e != nil {
				return e
			}
			return service.ErrGeminiCacheQuota
		}
	} else if stored.State == "send_started" || stored.State == "reconcile_required" || stored.State == "upstream_confirmed" {
		return service.ErrGeminiCacheConflict
	}
	if actual != nil {
		billing := usageBillingRepository{db: r.db}
		claimed, e := billing.claimUsageBillingRequest(ctx, tx, "gc-"+o.ID, o.Owner.APIKeyID, o.Fingerprint)
		if e != nil {
			return e
		}
		if !claimed {
			return service.ErrGeminiCacheConflict
		}
	}
	if actual != nil {
		accountCost, e := cacheChargeAmount(actual.AccountAmount)
		if e != nil {
			return e
		}
		var accountType, accountPlatform string
		if e = tx.QueryRowContext(ctx, `SELECT type, platform FROM accounts WHERE id=$1`, o.AccountID).Scan(&accountType, &accountPlatform); e != nil {
			return e
		}
		if accountCost.IsPositive() && service.AccountSupportsQuota(accountPlatform, accountType) {
			if _, e = incrementUsageBillingAccountQuota(ctx, tx, o.AccountID, accountCost.StringFixed(8)); e != nil {
				return e
			}
		}
	}
	var res sql.Result
	if reserved.IsPositive() {
		refund := reserved.Sub(amount).StringFixed(8)
		if o.SubscriptionID == nil {
			res, e := tx.ExecContext(ctx, `UPDATE users SET frozen_balance=frozen_balance-$1::numeric,balance=balance+$2::numeric,gemini_cache_generation=gemini_cache_generation+1,updated_at=NOW() WHERE id=$3 AND frozen_balance>=$1::numeric`, reserved.StringFixed(8), refund, o.Owner.UserID)
			if e != nil {
				return e
			}
			if e = requireCacheAffected(res); e != nil {
				return e
			}
		} else {
			res, e := tx.ExecContext(ctx, `UPDATE user_subscriptions SET gemini_cache_pending=gemini_cache_pending-$1::numeric,daily_usage_usd=daily_usage_usd+$2::numeric,weekly_usage_usd=weekly_usage_usd+$2::numeric,monthly_usage_usd=monthly_usage_usd+$2::numeric,gemini_cache_generation=gemini_cache_generation+1,updated_at=NOW() WHERE id=$3 AND gemini_cache_pending>=$1::numeric`, reserved.StringFixed(8), amount.StringFixed(8), *o.SubscriptionID)
			if e != nil {
				return e
			}
			if e = requireCacheAffected(res); e != nil {
				return e
			}
		}
		res, err = tx.ExecContext(ctx, `UPDATE api_keys SET gemini_cache_pending=gemini_cache_pending-$1::numeric,quota_used=quota_used+$2::numeric,gemini_cache_generation=gemini_cache_generation+1,updated_at=NOW() WHERE id=$3 AND gemini_cache_pending>=$1::numeric`, reserved.StringFixed(8), amount.StringFixed(8), o.Owner.APIKeyID)
		if err != nil {
			return err
		}
		if err = requireCacheAffected(res); err != nil {
			return err
		}
		if actual != nil && amount.IsPositive() {
			if err = incrementUsageBillingAPIKeyRateLimit(ctx, tx, o.Owner.APIKeyID, amount.StringFixed(8)); err != nil {
				return err
			}
		}
		if o.SubscriptionID == nil {
			res, err = tx.ExecContext(ctx, `UPDATE user_platform_quotas SET gemini_cache_pending=gemini_cache_pending-$1::numeric,
 gemini_cache_daily_usage=CASE WHEN gemini_cache_daily_start=$4 THEN gemini_cache_daily_usage+$2::numeric ELSE $2::numeric END,
 gemini_cache_weekly_usage=CASE WHEN gemini_cache_weekly_start=$5 THEN gemini_cache_weekly_usage+$2::numeric ELSE $2::numeric END,
 gemini_cache_monthly_usage=CASE WHEN gemini_cache_monthly_start IS NOT NULL AND gemini_cache_monthly_start+INTERVAL '30 days'>NOW() THEN gemini_cache_monthly_usage+$2::numeric ELSE $2::numeric END,
 gemini_cache_daily_start=$4,gemini_cache_weekly_start=$5,gemini_cache_monthly_start=CASE WHEN gemini_cache_monthly_start IS NOT NULL AND gemini_cache_monthly_start+INTERVAL '30 days'>NOW() THEN gemini_cache_monthly_start ELSE NOW() END,
 gemini_cache_generation=gemini_cache_generation+1,updated_at=NOW() WHERE user_id=$3 AND platform='gemini' AND deleted_at IS NULL AND gemini_cache_pending>=$1::numeric`, reserved.StringFixed(8), amount.StringFixed(8), o.Owner.UserID, timezone.StartOfDay(time.Now()), timezone.StartOfWeek(time.Now()))
			if err != nil {
				return err
			}
			if err = requireCacheAffected(res); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE gemini_cache_charge_scopes SET state=$2 WHERE operation_id=$1 AND state='pending'`, o.ID, target); err != nil {
			return err
		}
	}
	res, err = tx.ExecContext(ctx, `UPDATE gemini_cache_charges SET state=$2,actual_amount=$3,input_cost=$4,storage_cost=$5,updated_at=NOW() WHERE operation_id=$1`, o.ID, target, amount.StringFixed(8), input, storage)
	if err != nil {
		return err
	}
	if err = requireCacheAffected(res); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE gemini_cache_operations SET lease_until=lease_until WHERE id=$1 AND generation=$2 AND lease_until>clock_timestamp()`, o.ID, o.Generation)
	if err != nil {
		return err
	}
	if err = requireCacheAffected(res); err != nil {
		return err
	}
	return tx.Commit()
}

func maxCacheTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func normalizeCacheSubscriptionWindows(ctx context.Context, tx *sql.Tx, o *service.GeminiCacheOperation) error {
	if o.SubscriptionID == nil {
		return nil
	}
	var sub service.UserSubscription
	if err := tx.QueryRowContext(ctx, `SELECT starts_at,expires_at,daily_window_start,weekly_window_start,monthly_window_start FROM user_subscriptions WHERE id=$1`, *o.SubscriptionID).Scan(&sub.StartsAt, &sub.ExpiresAt, &sub.DailyWindowStart, &sub.WeeklyWindowStart, &sub.MonthlyWindowStart); err != nil {
		return err
	}
	starts := sub.CacheOperationWindowStarts(time.Now())
	previous := []*time.Time{sub.DailyWindowStart, sub.WeeklyWindowStart, sub.MonthlyWindowStart}
	fields := []string{"daily", "weekly", "monthly"}
	for i, newStart := range starts {
		if newStart == nil || (previous[i] != nil && newStart.Equal(*previous[i])) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE user_subscriptions SET `+fields[i]+`_usage_usd=0,`+fields[i]+`_window_start=$1 WHERE id=$2`, *newStart, *o.SubscriptionID); err != nil {
			return err
		}
	}
	return nil
}
