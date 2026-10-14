SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

-- Moving a subscription's start into the future must revoke existing cache
-- grants through the same durable authorization barrier as expiry changes.
DROP TRIGGER gemini_cache_subscription_authorization ON user_subscriptions;
CREATE TRIGGER gemini_cache_subscription_authorization
AFTER UPDATE OF status,deleted_at,user_id,group_id,starts_at,expires_at ON user_subscriptions
FOR EACH ROW WHEN (
  OLD.status IS DISTINCT FROM NEW.status OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
  OR OLD.user_id IS DISTINCT FROM NEW.user_id OR OLD.group_id IS DISTINCT FROM NEW.group_id
  OR OLD.starts_at IS DISTINCT FROM NEW.starts_at OR OLD.expires_at IS DISTINCT FROM NEW.expires_at)
EXECUTE FUNCTION nuro_gemini_cache_subscription_event();

-- Platform snapshots are authoritative Redis values, including normal period
-- rollover decreases. Only the explicit admin reset writer advances this
-- fence; a generic decrease trigger would strand the flusher generation.
DROP TRIGGER gemini_cache_platform_usage_reset ON user_platform_quotas;
