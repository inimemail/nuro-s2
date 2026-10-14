SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

-- Control-plane changes invalidate the same durable view clock. Usage/balance
-- updates never fire these triggers, and the generation hot path stays local.
CREATE FUNCTION nuro_gemini_cache_authorization_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE target_id BIGINT; rev BIGINT; changed BOOLEAN;
BEGIN
  IF TG_TABLE_NAME IN ('account_groups','user_allowed_groups') THEN
    target_id := CASE WHEN TG_OP='DELETE' THEN OLD.group_id ELSE NEW.group_id END;
  ELSE
    target_id := CASE WHEN TG_OP='DELETE' THEN OLD.id ELSE NEW.id END;
  END IF;
  SELECT EXISTS(SELECT 1 FROM gemini_cached_contents c WHERE c.state<>'deleted' AND
    CASE TG_TABLE_NAME
      WHEN 'accounts' THEN c.account_id=target_id
      WHEN 'users' THEN c.user_id=target_id
      WHEN 'api_keys' THEN c.api_key_id=target_id
      ELSE c.group_id=target_id END) INTO changed;
  IF NOT changed THEN RETURN NULL; END IF;
  UPDATE gemini_cache_view_clock SET revision=revision+1 WHERE id=1 RETURNING revision INTO rev;
  INSERT INTO gemini_cache_events(revision,resource_id) VALUES(rev,'authorization');
  RETURN NULL;
END $$;

CREATE TRIGGER gemini_cache_account_authorization AFTER UPDATE OF status,schedulable,deleted_at,credentials,platform,type ON accounts
FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status OR OLD.schedulable IS DISTINCT FROM NEW.schedulable OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at OR OLD.credentials IS DISTINCT FROM NEW.credentials OR OLD.platform IS DISTINCT FROM NEW.platform OR OLD.type IS DISTINCT FROM NEW.type)
EXECUTE FUNCTION nuro_gemini_cache_authorization_event();
CREATE TRIGGER gemini_cache_key_authorization AFTER UPDATE OF status,deleted_at,user_id,group_id,ip_whitelist,ip_blacklist ON api_keys
FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at OR OLD.user_id IS DISTINCT FROM NEW.user_id OR OLD.group_id IS DISTINCT FROM NEW.group_id OR OLD.ip_whitelist IS DISTINCT FROM NEW.ip_whitelist OR OLD.ip_blacklist IS DISTINCT FROM NEW.ip_blacklist)
EXECUTE FUNCTION nuro_gemini_cache_authorization_event();
CREATE TRIGGER gemini_cache_user_authorization AFTER UPDATE OF status,deleted_at,restrict_public_groups ON users
FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at OR OLD.restrict_public_groups IS DISTINCT FROM NEW.restrict_public_groups)
EXECUTE FUNCTION nuro_gemini_cache_authorization_event();
CREATE TRIGGER gemini_cache_group_authorization AFTER UPDATE OF status,deleted_at,platform,is_exclusive,subscription_type ON groups
FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at OR OLD.platform IS DISTINCT FROM NEW.platform OR OLD.is_exclusive IS DISTINCT FROM NEW.is_exclusive OR OLD.subscription_type IS DISTINCT FROM NEW.subscription_type)
EXECUTE FUNCTION nuro_gemini_cache_authorization_event();
CREATE TRIGGER gemini_cache_account_membership AFTER INSERT OR DELETE ON account_groups FOR EACH ROW EXECUTE FUNCTION nuro_gemini_cache_authorization_event();
CREATE TRIGGER gemini_cache_user_membership AFTER INSERT OR DELETE ON user_allowed_groups FOR EACH ROW EXECUTE FUNCTION nuro_gemini_cache_authorization_event();

-- Bound reads/maintenance of durable metadata and billing summaries.
CREATE INDEX idx_gemini_cache_revision ON gemini_cached_contents(revision);
CREATE INDEX idx_gemini_cache_resource_operations ON gemini_cache_operations(resource_id,created_at DESC,id DESC);
CREATE INDEX idx_gemini_cache_events_created ON gemini_cache_events(created_at);

CREATE TABLE gemini_cache_admin_actions (
 id BIGSERIAL PRIMARY KEY,
 operation_id VARCHAR(48) NOT NULL REFERENCES gemini_cache_operations(id),
 actor_user_id BIGINT NOT NULL,
 action VARCHAR(32) NOT NULL,
 reason VARCHAR(256) NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
