-- Do not rewrite account-group values here. The legacy schema does not record
-- whether a value was copied from the group or explicitly chosen by an
-- administrator; clearing equal values could silently remove a deliberate
-- account-level restriction. Effective policy is already computed as
-- min(account override, current group default), so a metadata rebuild is the
-- only safe automatic repair for existing data.
INSERT INTO scheduler_outbox (event_type, payload)
SELECT 'full_rebuild', '{"reason":"reconcile_inherited_group_billing_guard_overrides_v4","refresh_account_metadata":true}'::jsonb
WHERE NOT EXISTS (
    SELECT 1
    FROM scheduler_outbox
    WHERE event_type = 'full_rebuild'
      AND payload ->> 'reason' = 'reconcile_inherited_group_billing_guard_overrides_v4'
);
