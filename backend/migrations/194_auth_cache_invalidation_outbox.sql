-- Durable, transactionally-enqueued API-key auth cache invalidation.
-- cache_key is always SHA-256 hex; plaintext credentials never leave api_keys.

CREATE TABLE IF NOT EXISTS auth_cache_invalidation_outbox (
    id              BIGSERIAL PRIMARY KEY,
    cache_key       CHAR(64) NOT NULL CHECK (cache_key ~ '^[0-9a-f]{64}$'),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    available_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivery_stage  SMALLINT NOT NULL DEFAULT 0 CHECK (delivery_stage IN (0, 1)),
    attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error      TEXT,
    claimed_at      TIMESTAMPTZ,
    claimed_by      TEXT
);

CREATE INDEX IF NOT EXISTS idx_auth_cache_invalidation_outbox_available
    ON auth_cache_invalidation_outbox (available_at, id)
    WHERE claimed_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_auth_cache_invalidation_outbox_lease
    ON auth_cache_invalidation_outbox (claimed_at)
    WHERE claimed_at IS NOT NULL;
-- Keep at most one pending invalidation per credential. This cleanup makes the
-- migration safe to re-run after an earlier non-unique version was deployed.
DELETE FROM auth_cache_invalidation_outbox AS older
USING auth_cache_invalidation_outbox AS newer
WHERE older.cache_key = newer.cache_key
  AND older.id < newer.id;
DROP INDEX IF EXISTS idx_auth_cache_invalidation_outbox_cache_key;
CREATE UNIQUE INDEX IF NOT EXISTS idx_auth_cache_invalidation_outbox_cache_key_unique
    ON auth_cache_invalidation_outbox (cache_key);
CREATE INDEX IF NOT EXISTS idx_auth_cache_invalidation_outbox_created_at
    ON auth_cache_invalidation_outbox (created_at);

CREATE OR REPLACE FUNCTION enqueue_auth_cache_invalidation(raw_key TEXT)
RETURNS VOID
LANGUAGE plpgsql
AS $$
BEGIN
    IF raw_key IS NULL OR raw_key = '' THEN
        RETURN;
    END IF;
    INSERT INTO auth_cache_invalidation_outbox (cache_key)
    VALUES (encode(sha256(convert_to(raw_key, 'UTF8')), 'hex'))
    ON CONFLICT (cache_key) DO UPDATE
    SET created_at = NOW(),
        available_at = NOW(),
        delivery_stage = 0,
        attempts = 0,
        last_error = NULL,
        claimed_at = NULL,
        claimed_by = NULL;
END;
$$;

CREATE OR REPLACE FUNCTION enqueue_api_key_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM enqueue_auth_cache_invalidation(OLD.key);
        RETURN OLD;
    END IF;

    IF OLD.key IS DISTINCT FROM NEW.key
	   OR OLD.name IS DISTINCT FROM NEW.name
       OR OLD.status IS DISTINCT FROM NEW.status
       OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
       OR OLD.user_id IS DISTINCT FROM NEW.user_id
       OR OLD.group_id IS DISTINCT FROM NEW.group_id
       OR OLD.ip_whitelist IS DISTINCT FROM NEW.ip_whitelist
       OR OLD.ip_blacklist IS DISTINCT FROM NEW.ip_blacklist
	   OR OLD.quota IS DISTINCT FROM NEW.quota
	   OR OLD.quota_used IS DISTINCT FROM NEW.quota_used
	   OR OLD.expires_at IS DISTINCT FROM NEW.expires_at
	   OR OLD.rate_limit_5h IS DISTINCT FROM NEW.rate_limit_5h
	   OR OLD.rate_limit_1d IS DISTINCT FROM NEW.rate_limit_1d
	   OR OLD.rate_limit_7d IS DISTINCT FROM NEW.rate_limit_7d THEN
        PERFORM enqueue_auth_cache_invalidation(OLD.key);
        IF NEW.deleted_at IS NULL AND NEW.key IS DISTINCT FROM OLD.key THEN
            PERFORM enqueue_auth_cache_invalidation(NEW.key);
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_api_keys_auth_cache_invalidation ON api_keys;
CREATE TRIGGER trg_api_keys_auth_cache_invalidation
AFTER UPDATE OR DELETE ON api_keys
FOR EACH ROW EXECUTE FUNCTION enqueue_api_key_auth_cache_invalidation();

CREATE OR REPLACE FUNCTION enqueue_user_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_user_id BIGINT;
BEGIN
    target_user_id := OLD.id;
    IF TG_OP = 'UPDATE'
       AND OLD.status IS NOT DISTINCT FROM NEW.status
       AND OLD.role IS NOT DISTINCT FROM NEW.role
	   AND OLD.email IS NOT DISTINCT FROM NEW.email
	   AND OLD.username IS NOT DISTINCT FROM NEW.username
	   AND OLD.balance IS NOT DISTINCT FROM NEW.balance
	   AND OLD.concurrency IS NOT DISTINCT FROM NEW.concurrency
	   AND OLD.balance_notify_enabled IS NOT DISTINCT FROM NEW.balance_notify_enabled
	   AND OLD.balance_notify_threshold_type IS NOT DISTINCT FROM NEW.balance_notify_threshold_type
	   AND OLD.balance_notify_threshold IS NOT DISTINCT FROM NEW.balance_notify_threshold
	   AND OLD.balance_notify_extra_emails IS NOT DISTINCT FROM NEW.balance_notify_extra_emails
	   AND OLD.total_recharged IS NOT DISTINCT FROM NEW.total_recharged
	   AND OLD.rpm_limit IS NOT DISTINCT FROM NEW.rpm_limit
       AND OLD.deleted_at IS NOT DISTINCT FROM NEW.deleted_at THEN
        RETURN NEW;
    END IF;

    PERFORM enqueue_auth_cache_invalidation(k.key)
    FROM api_keys AS k
    WHERE k.user_id = target_user_id
      AND k.deleted_at IS NULL
      AND k.key <> '';
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_users_auth_cache_invalidation ON users;
CREATE TRIGGER trg_users_auth_cache_invalidation
BEFORE UPDATE OR DELETE ON users
FOR EACH ROW EXECUTE FUNCTION enqueue_user_auth_cache_invalidation();

CREATE OR REPLACE FUNCTION enqueue_group_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_group_id BIGINT;
BEGIN
    target_group_id := OLD.id;
    IF TG_OP = 'UPDATE'
       AND OLD.status IS NOT DISTINCT FROM NEW.status
       AND OLD.is_exclusive IS NOT DISTINCT FROM NEW.is_exclusive
	   AND OLD.name IS NOT DISTINCT FROM NEW.name
	   AND OLD.platform IS NOT DISTINCT FROM NEW.platform
	   AND OLD.subscription_type IS NOT DISTINCT FROM NEW.subscription_type
	   AND OLD.rate_multiplier IS NOT DISTINCT FROM NEW.rate_multiplier
	   AND OLD.upstream_billing_guard_max_multiplier IS NOT DISTINCT FROM NEW.upstream_billing_guard_max_multiplier
	   AND OLD.peak_rate_enabled IS NOT DISTINCT FROM NEW.peak_rate_enabled
	   AND OLD.peak_start IS NOT DISTINCT FROM NEW.peak_start
	   AND OLD.peak_end IS NOT DISTINCT FROM NEW.peak_end
	   AND OLD.peak_rate_multiplier IS NOT DISTINCT FROM NEW.peak_rate_multiplier
	   AND OLD.daily_limit_usd IS NOT DISTINCT FROM NEW.daily_limit_usd
	   AND OLD.weekly_limit_usd IS NOT DISTINCT FROM NEW.weekly_limit_usd
	   AND OLD.monthly_limit_usd IS NOT DISTINCT FROM NEW.monthly_limit_usd
	   AND OLD.allow_image_generation IS NOT DISTINCT FROM NEW.allow_image_generation
	   AND OLD.allow_batch_image_generation IS NOT DISTINCT FROM NEW.allow_batch_image_generation
	   AND OLD.image_rate_independent IS NOT DISTINCT FROM NEW.image_rate_independent
	   AND OLD.image_rate_multiplier IS NOT DISTINCT FROM NEW.image_rate_multiplier
	   AND OLD.image_price_1k IS NOT DISTINCT FROM NEW.image_price_1k
	   AND OLD.image_price_2k IS NOT DISTINCT FROM NEW.image_price_2k
	   AND OLD.image_price_4k IS NOT DISTINCT FROM NEW.image_price_4k
	   AND OLD.batch_image_discount_multiplier IS NOT DISTINCT FROM NEW.batch_image_discount_multiplier
	   AND OLD.batch_image_hold_multiplier IS NOT DISTINCT FROM NEW.batch_image_hold_multiplier
	   AND OLD.video_rate_independent IS NOT DISTINCT FROM NEW.video_rate_independent
	   AND OLD.video_rate_multiplier IS NOT DISTINCT FROM NEW.video_rate_multiplier
	   AND OLD.video_price_480p IS NOT DISTINCT FROM NEW.video_price_480p
	   AND OLD.video_price_720p IS NOT DISTINCT FROM NEW.video_price_720p
	   AND OLD.video_price_1080p IS NOT DISTINCT FROM NEW.video_price_1080p
	   AND OLD.web_search_price_per_call IS NOT DISTINCT FROM NEW.web_search_price_per_call
	   AND OLD.claude_code_only IS NOT DISTINCT FROM NEW.claude_code_only
	   AND OLD.fallback_group_id IS NOT DISTINCT FROM NEW.fallback_group_id
	   AND OLD.fallback_group_id_on_invalid_request IS NOT DISTINCT FROM NEW.fallback_group_id_on_invalid_request
	   AND OLD.model_routing IS NOT DISTINCT FROM NEW.model_routing
	   AND OLD.model_routing_enabled IS NOT DISTINCT FROM NEW.model_routing_enabled
	   AND OLD.mcp_xml_inject IS NOT DISTINCT FROM NEW.mcp_xml_inject
	   AND OLD.supported_model_scopes IS NOT DISTINCT FROM NEW.supported_model_scopes
	   AND OLD.allow_messages_dispatch IS NOT DISTINCT FROM NEW.allow_messages_dispatch
	   AND OLD.require_oauth_only IS NOT DISTINCT FROM NEW.require_oauth_only
	   AND OLD.require_privacy_set IS NOT DISTINCT FROM NEW.require_privacy_set
	   AND OLD.default_mapped_model IS NOT DISTINCT FROM NEW.default_mapped_model
	   AND OLD.messages_dispatch_model_config IS NOT DISTINCT FROM NEW.messages_dispatch_model_config
	   AND OLD.models_list_config IS NOT DISTINCT FROM NEW.models_list_config
	   AND OLD.strict_model_priority_on_model_mismatch IS NOT DISTINCT FROM NEW.strict_model_priority_on_model_mismatch
	   AND OLD.rpm_limit IS NOT DISTINCT FROM NEW.rpm_limit
       AND OLD.deleted_at IS NOT DISTINCT FROM NEW.deleted_at THEN
        RETURN NEW;
    END IF;

    PERFORM enqueue_auth_cache_invalidation(k.key)
    FROM api_keys AS k
    WHERE k.group_id = target_group_id
      AND k.deleted_at IS NULL
      AND k.key <> '';
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_groups_auth_cache_invalidation ON groups;
CREATE TRIGGER trg_groups_auth_cache_invalidation
BEFORE UPDATE OR DELETE ON groups
FOR EACH ROW EXECUTE FUNCTION enqueue_group_auth_cache_invalidation();

CREATE OR REPLACE FUNCTION enqueue_allowed_group_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_user_id BIGINT;
    target_group_id BIGINT;
BEGIN
    IF TG_OP = 'UPDATE'
       AND (OLD.user_id IS DISTINCT FROM NEW.user_id
            OR OLD.group_id IS DISTINCT FROM NEW.group_id) THEN
        IF EXISTS (
            SELECT 1 FROM groups g
            WHERE g.id = OLD.group_id AND g.is_exclusive = TRUE
        ) THEN
            PERFORM enqueue_auth_cache_invalidation(k.key)
            FROM api_keys AS k
            WHERE k.user_id = OLD.user_id
              AND k.group_id = OLD.group_id
              AND k.deleted_at IS NULL
              AND k.key <> '';
        END IF;
        target_user_id := NEW.user_id;
        target_group_id := NEW.group_id;
    ELSIF TG_OP = 'UPDATE' THEN
        RETURN NEW;
    ELSIF TG_OP = 'INSERT' THEN
        target_user_id := NEW.user_id;
        target_group_id := NEW.group_id;
    ELSE
        target_user_id := OLD.user_id;
        target_group_id := OLD.group_id;
    END IF;

    IF EXISTS (
        SELECT 1 FROM groups g
        WHERE g.id = target_group_id AND g.is_exclusive = TRUE
    ) THEN
        PERFORM enqueue_auth_cache_invalidation(k.key)
        FROM api_keys AS k
        WHERE k.user_id = target_user_id
          AND k.group_id = target_group_id
          AND k.deleted_at IS NULL
          AND k.key <> '';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_user_allowed_groups_auth_cache_invalidation ON user_allowed_groups;
CREATE TRIGGER trg_user_allowed_groups_auth_cache_invalidation
AFTER INSERT OR UPDATE OR DELETE ON user_allowed_groups
FOR EACH ROW EXECUTE FUNCTION enqueue_allowed_group_auth_cache_invalidation();

CREATE OR REPLACE FUNCTION enqueue_user_group_rpm_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND OLD.user_id IS NOT DISTINCT FROM NEW.user_id
       AND OLD.group_id IS NOT DISTINCT FROM NEW.group_id
       AND OLD.rpm_override IS NOT DISTINCT FROM NEW.rpm_override THEN
        RETURN NEW;
    END IF;

    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        PERFORM enqueue_auth_cache_invalidation(k.key)
        FROM api_keys AS k
        WHERE k.user_id = OLD.user_id
          AND k.group_id = OLD.group_id
          AND k.deleted_at IS NULL
          AND k.key <> '';
    END IF;
    IF TG_OP IN ('UPDATE', 'INSERT') THEN
        PERFORM enqueue_auth_cache_invalidation(k.key)
        FROM api_keys AS k
        WHERE k.user_id = NEW.user_id
          AND k.group_id = NEW.group_id
          AND k.deleted_at IS NULL
          AND k.key <> '';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_user_group_rpm_auth_cache_invalidation ON user_group_rate_multipliers;
CREATE TRIGGER trg_user_group_rpm_auth_cache_invalidation
AFTER INSERT OR UPDATE OR DELETE ON user_group_rate_multipliers
FOR EACH ROW EXECUTE FUNCTION enqueue_user_group_rpm_auth_cache_invalidation();

COMMENT ON TABLE auth_cache_invalidation_outbox IS
    'Durable cross-instance auth cache invalidations; cache_key is SHA-256 hex, never plaintext API key';
