-- Optional lower bound for the upstream billing multiplier guard.
-- NULL means unrestricted and preserves all existing maximum-only behavior.
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS upstream_billing_guard_min_multiplier DOUBLE PRECISION NULL;

ALTER TABLE account_groups
    ADD COLUMN IF NOT EXISTS upstream_billing_guard_min_multiplier DOUBLE PRECISION NULL;

ALTER TABLE groups
    DROP CONSTRAINT IF EXISTS groups_upstream_billing_guard_min_multiplier_nonnegative;
ALTER TABLE groups
    ADD CONSTRAINT groups_upstream_billing_guard_min_multiplier_nonnegative
    CHECK (
        upstream_billing_guard_min_multiplier IS NULL
        OR (
            upstream_billing_guard_min_multiplier >= 0
            AND upstream_billing_guard_min_multiplier < 'Infinity'::double precision
        )
    );

-- Migration 194's broad trigger predates this column and intentionally skips
-- updates when all fields it knows are unchanged. Cover lower-bound-only edits
-- so API-key auth snapshots cannot retain stale group metadata.
CREATE OR REPLACE FUNCTION enqueue_group_billing_guard_min_auth_cache_invalidation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    PERFORM enqueue_auth_cache_invalidation(k.key)
    FROM api_keys AS k
    WHERE k.group_id = NEW.id
      AND k.deleted_at IS NULL
      AND k.key <> '';
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_groups_billing_guard_min_auth_cache_invalidation ON groups;
CREATE TRIGGER trg_groups_billing_guard_min_auth_cache_invalidation
AFTER UPDATE OF upstream_billing_guard_min_multiplier ON groups
FOR EACH ROW
WHEN (OLD.upstream_billing_guard_min_multiplier IS DISTINCT FROM NEW.upstream_billing_guard_min_multiplier)
EXECUTE FUNCTION enqueue_group_billing_guard_min_auth_cache_invalidation();

ALTER TABLE account_groups
    DROP CONSTRAINT IF EXISTS account_groups_upstream_billing_guard_min_multiplier_nonnegative;
ALTER TABLE account_groups
    ADD CONSTRAINT account_groups_upstream_billing_guard_min_multiplier_nonnegative
    CHECK (
        upstream_billing_guard_min_multiplier IS NULL
        OR (
            upstream_billing_guard_min_multiplier >= 0
            AND upstream_billing_guard_min_multiplier < 'Infinity'::double precision
        )
    );

-- A configured interval must be strictly ordered. Existing rows are
-- maximum-only, so this constraint is inert until a lower bound is supplied.
ALTER TABLE groups
    DROP CONSTRAINT IF EXISTS groups_upstream_billing_guard_range;
ALTER TABLE groups
    ADD CONSTRAINT groups_upstream_billing_guard_range
    CHECK (
        upstream_billing_guard_min_multiplier IS NULL
        OR upstream_billing_guard_max_multiplier IS NULL
        OR upstream_billing_guard_min_multiplier < upstream_billing_guard_max_multiplier
    );

ALTER TABLE account_groups
    DROP CONSTRAINT IF EXISTS account_groups_upstream_billing_guard_range;
ALTER TABLE account_groups
    ADD CONSTRAINT account_groups_upstream_billing_guard_range
    CHECK (
        upstream_billing_guard_min_multiplier IS NULL
        OR upstream_billing_guard_max_multiplier IS NULL
        OR upstream_billing_guard_min_multiplier < upstream_billing_guard_max_multiplier
    );

INSERT INTO scheduler_outbox (event_type, payload)
SELECT 'full_rebuild', '{"reason":"upstream_billing_guard_min_multiplier_v1","refresh_account_metadata":true}'::jsonb
WHERE NOT EXISTS (
    SELECT 1 FROM scheduler_outbox
    WHERE event_type = 'full_rebuild'
      AND payload ->> 'reason' = 'upstream_billing_guard_min_multiplier_v1'
);
