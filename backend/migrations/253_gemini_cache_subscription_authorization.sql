SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

-- Subscription revocation must invalidate leased resource views just like
-- user/key/group revocation. Only owners with live caches take the clock lock.
CREATE FUNCTION nuro_gemini_cache_subscription_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE rev BIGINT; changed BOOLEAN;
BEGIN
  IF TG_OP='INSERT' THEN
    SELECT EXISTS(SELECT 1 FROM gemini_cached_contents WHERE user_id=NEW.user_id AND group_id=NEW.group_id AND state<>'deleted') INTO changed;
  ELSIF TG_OP='DELETE' THEN
    SELECT EXISTS(SELECT 1 FROM gemini_cached_contents WHERE user_id=OLD.user_id AND group_id=OLD.group_id AND state<>'deleted') INTO changed;
  ELSE
    SELECT EXISTS(SELECT 1 FROM gemini_cached_contents WHERE state<>'deleted' AND
      ((user_id=OLD.user_id AND group_id=OLD.group_id) OR (user_id=NEW.user_id AND group_id=NEW.group_id))) INTO changed;
  END IF;
  IF NOT changed THEN RETURN NULL; END IF;
  UPDATE gemini_cache_view_clock SET revision=revision+1 WHERE id=1 RETURNING revision INTO rev;
  INSERT INTO gemini_cache_events(revision,resource_id) VALUES(rev,'authorization');
  RETURN NULL;
END $$;

CREATE TRIGGER gemini_cache_subscription_authorization AFTER UPDATE OF status,deleted_at,user_id,group_id,expires_at ON user_subscriptions
FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at OR OLD.user_id IS DISTINCT FROM NEW.user_id OR OLD.group_id IS DISTINCT FROM NEW.group_id OR OLD.expires_at IS DISTINCT FROM NEW.expires_at)
EXECUTE FUNCTION nuro_gemini_cache_subscription_event();
CREATE TRIGGER gemini_cache_subscription_insert AFTER INSERT ON user_subscriptions FOR EACH ROW EXECUTE FUNCTION nuro_gemini_cache_subscription_event();
CREATE TRIGGER gemini_cache_subscription_delete AFTER DELETE ON user_subscriptions FOR EACH ROW EXECUTE FUNCTION nuro_gemini_cache_subscription_event();

-- Retain pending amounts while preventing delayed snapshots from undoing a
-- usage reset. Ordinary owners with no cache financial generation are no-op.
CREATE FUNCTION nuro_gemini_cache_usage_reset_generation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.gemini_cache_generation=0 OR NEW.gemini_cache_generation<>OLD.gemini_cache_generation THEN RETURN NEW; END IF;
  IF TG_TABLE_NAME='api_keys' THEN
    IF NEW.quota_used<OLD.quota_used OR NEW.usage_5h<OLD.usage_5h OR NEW.usage_1d<OLD.usage_1d OR NEW.usage_7d<OLD.usage_7d THEN
      NEW.gemini_cache_generation := OLD.gemini_cache_generation+1;
    END IF;
  ELSE
    IF NEW.daily_usage_usd<OLD.daily_usage_usd OR NEW.weekly_usage_usd<OLD.weekly_usage_usd OR NEW.monthly_usage_usd<OLD.monthly_usage_usd THEN
      NEW.gemini_cache_generation := OLD.gemini_cache_generation+1;
    END IF;
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER gemini_cache_key_usage_reset BEFORE UPDATE OF quota_used,usage_5h,usage_1d,usage_7d ON api_keys
FOR EACH ROW WHEN (OLD.gemini_cache_generation>0 AND NEW.gemini_cache_generation=OLD.gemini_cache_generation AND
 (NEW.quota_used<OLD.quota_used OR NEW.usage_5h<OLD.usage_5h OR NEW.usage_1d<OLD.usage_1d OR NEW.usage_7d<OLD.usage_7d))
EXECUTE FUNCTION nuro_gemini_cache_usage_reset_generation();
CREATE TRIGGER gemini_cache_subscription_usage_reset BEFORE UPDATE OF daily_usage_usd,weekly_usage_usd,monthly_usage_usd ON user_subscriptions
FOR EACH ROW WHEN (OLD.gemini_cache_generation>0 AND NEW.gemini_cache_generation=OLD.gemini_cache_generation AND
 (NEW.daily_usage_usd<OLD.daily_usage_usd OR NEW.weekly_usage_usd<OLD.weekly_usage_usd OR NEW.monthly_usage_usd<OLD.monthly_usage_usd))
EXECUTE FUNCTION nuro_gemini_cache_usage_reset_generation();
CREATE TRIGGER gemini_cache_platform_usage_reset BEFORE UPDATE OF daily_usage_usd,weekly_usage_usd,monthly_usage_usd ON user_platform_quotas
FOR EACH ROW WHEN (OLD.gemini_cache_generation>0 AND NEW.gemini_cache_generation=OLD.gemini_cache_generation AND
 (NEW.daily_usage_usd<OLD.daily_usage_usd OR NEW.weekly_usage_usd<OLD.weekly_usage_usd OR NEW.monthly_usage_usd<OLD.monthly_usage_usd))
EXECUTE FUNCTION nuro_gemini_cache_usage_reset_generation();
