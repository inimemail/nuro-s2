-- Migrations 187 and 190 copied legacy account-level guard limits into
-- account_groups, where current code interprets every non-NULL value as an
-- explicit account override. Those copied values prevent later group limit
-- increases from reaching existing accounts. Reset existing OpenAI bindings to
-- inheritance; overrides explicitly configured after this migration remain
-- supported and are stored by the normal account update path.
UPDATE account_groups ag
SET upstream_billing_guard_max_multiplier = NULL
FROM groups g
WHERE g.id = ag.group_id
  AND g.deleted_at IS NULL
  AND g.platform = 'openai'
  AND ag.upstream_billing_guard_max_multiplier IS NOT NULL;

-- Refresh both per-account metadata and group snapshots on every running node.
-- The guard makes disaster-recovery replay idempotent.
INSERT INTO scheduler_outbox (event_type, payload)
SELECT 'full_rebuild', '{"reason":"reset_legacy_account_group_billing_guard_overrides_v5","refresh_account_metadata":true}'::jsonb
WHERE NOT EXISTS (
    SELECT 1
    FROM scheduler_outbox
    WHERE event_type = 'full_rebuild'
      AND payload ->> 'reason' = 'reset_legacy_account_group_billing_guard_overrides_v5'
);
