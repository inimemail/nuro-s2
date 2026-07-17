-- Upstream billing protection is configured once per OpenAI group and is
-- enabled per OpenAI API-key account. The legacy account_groups column is
-- intentionally retained for rollback/data reference; new code uses the group
-- column as the sole source so NULL always means unrestricted.
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS upstream_billing_guard_max_multiplier DOUBLE PRECISION NULL;

ALTER TABLE groups
    DROP CONSTRAINT IF EXISTS groups_upstream_billing_guard_max_multiplier_nonnegative;
ALTER TABLE groups
    ADD CONSTRAINT groups_upstream_billing_guard_max_multiplier_nonnegative
    CHECK (
        upstream_billing_guard_max_multiplier IS NULL
        OR (
            upstream_billing_guard_max_multiplier >= 0
            AND upstream_billing_guard_max_multiplier < 'Infinity'::double precision
        )
    );

-- A group can previously contain different binding-level limits. Choosing the
-- minimum preserves the strongest existing protection instead of silently
-- allowing traffic that an administrator had already capped.
WITH migrated_limits AS (
    SELECT ag.group_id, MIN(ag.upstream_billing_guard_max_multiplier) AS max_multiplier
    FROM account_groups ag
    JOIN groups g ON g.id = ag.group_id
    WHERE g.deleted_at IS NULL
      AND g.platform = 'openai'
      AND ag.upstream_billing_guard_max_multiplier IS NOT NULL
    GROUP BY ag.group_id
)
UPDATE groups g
SET upstream_billing_guard_max_multiplier = migrated_limits.max_multiplier,
    updated_at = NOW()
FROM migrated_limits
WHERE g.id = migrated_limits.group_id
  AND g.upstream_billing_guard_max_multiplier IS NULL;

-- Migration 187 disabled the legacy account-global switch. Restore it as the
-- master switch for accounts that had at least one protected binding, and
-- ensure the required automatic observation remains enabled.
UPDATE accounts a
SET upstream_billing_guard_enabled = TRUE,
    extra = COALESCE(a.extra, '{}'::jsonb) || '{"upstream_billing_probe_enabled":true}'::jsonb,
    updated_at = NOW()
WHERE a.deleted_at IS NULL
  AND a.platform = 'openai'
  AND a.type = 'apikey'
  AND EXISTS (
      SELECT 1
      FROM account_groups ag
      JOIN groups g ON g.id = ag.group_id
      WHERE ag.account_id = a.id
        AND g.deleted_at IS NULL
        AND g.platform = 'openai'
        AND (
            g.upstream_billing_guard_max_multiplier IS NOT NULL
            OR ag.upstream_billing_guard_max_multiplier IS NOT NULL
        )
  );

-- Force every running scheduler worker to rebuild after the policy source and
-- account master switches have changed. Startup also performs a synchronous
-- rebuild before serving traffic.
INSERT INTO scheduler_outbox (event_type, payload)
SELECT 'full_rebuild', '{"reason":"group_upstream_billing_guard_v2","refresh_account_metadata":true}'::jsonb
WHERE NOT EXISTS (
    SELECT 1
    FROM scheduler_outbox
    WHERE event_type = 'full_rebuild'
      AND payload ->> 'reason' = 'group_upstream_billing_guard_v2'
);
