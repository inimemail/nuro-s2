-- Reinterpret the legacy binding values as account x group overrides. Keep
-- stricter existing limits, but cap values that came from the pre-group policy
-- at the group's ceiling. Bindings for non-OpenAI groups or groups without a
-- configured default remain unrestricted.
UPDATE account_groups ag
SET upstream_billing_guard_max_multiplier = CASE
    WHEN g.platform = 'openai' AND g.upstream_billing_guard_max_multiplier IS NOT NULL
        THEN LEAST(ag.upstream_billing_guard_max_multiplier, g.upstream_billing_guard_max_multiplier)
    ELSE NULL
END
FROM groups g
WHERE g.id = ag.group_id
  AND ag.upstream_billing_guard_max_multiplier IS NOT NULL;

-- Refresh scheduler metadata so every node receives the separate group default
-- and override fields. The NOT EXISTS guard keeps disaster-recovery replay
-- idempotent even if the migration runner is invoked more than once.
INSERT INTO scheduler_outbox (event_type, payload)
SELECT 'full_rebuild', '{"reason":"account_group_billing_guard_overrides_v3","refresh_account_metadata":true}'::jsonb
WHERE NOT EXISTS (
    SELECT 1
    FROM scheduler_outbox
    WHERE event_type = 'full_rebuild'
      AND payload ->> 'reason' = 'account_group_billing_guard_overrides_v3'
);
