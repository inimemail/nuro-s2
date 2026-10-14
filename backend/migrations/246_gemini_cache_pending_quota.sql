SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

-- Pending funds live outside consumed usage. Existing reset writers cannot
-- erase them, and pending amounts carry across windows until resolved.
ALTER TABLE api_keys ADD COLUMN gemini_cache_pending NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK(gemini_cache_pending >= 0);
ALTER TABLE api_keys ADD COLUMN gemini_cache_generation BIGINT NOT NULL DEFAULT 0;
ALTER TABLE user_subscriptions ADD COLUMN gemini_cache_pending NUMERIC(20,10) NOT NULL DEFAULT 0 CHECK(gemini_cache_pending >= 0);
ALTER TABLE user_subscriptions ADD COLUMN gemini_cache_generation BIGINT NOT NULL DEFAULT 0;
ALTER TABLE user_platform_quotas ADD COLUMN gemini_cache_pending NUMERIC(20,10) NOT NULL DEFAULT 0 CHECK(gemini_cache_pending >= 0);
ALTER TABLE user_platform_quotas ADD COLUMN gemini_cache_generation BIGINT NOT NULL DEFAULT 0;

ALTER TABLE users ADD COLUMN gemini_cache_generation BIGINT NOT NULL DEFAULT 0;
ALTER TABLE user_platform_quotas ADD COLUMN gemini_cache_daily_usage NUMERIC(20,10) NOT NULL DEFAULT 0 CHECK(gemini_cache_daily_usage >= 0);
ALTER TABLE user_platform_quotas ADD COLUMN gemini_cache_daily_start TIMESTAMPTZ;
ALTER TABLE user_platform_quotas ADD COLUMN gemini_cache_weekly_usage NUMERIC(20,10) NOT NULL DEFAULT 0 CHECK(gemini_cache_weekly_usage >= 0);
ALTER TABLE user_platform_quotas ADD COLUMN gemini_cache_weekly_start TIMESTAMPTZ;
ALTER TABLE user_platform_quotas ADD COLUMN gemini_cache_monthly_usage NUMERIC(20,10) NOT NULL DEFAULT 0 CHECK(gemini_cache_monthly_usage >= 0);
ALTER TABLE user_platform_quotas ADD COLUMN gemini_cache_monthly_start TIMESTAMPTZ;

CREATE FUNCTION enqueue_gemini_cache_quota_auth_invalidation() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.gemini_cache_generation IS DISTINCT FROM NEW.gemini_cache_generation THEN
        PERFORM enqueue_auth_cache_invalidation(NEW.key);
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_gemini_cache_quota_auth_invalidation AFTER UPDATE OF gemini_cache_generation ON api_keys FOR EACH ROW EXECUTE FUNCTION enqueue_gemini_cache_quota_auth_invalidation();
