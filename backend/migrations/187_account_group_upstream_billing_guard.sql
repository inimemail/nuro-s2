-- Upstream rate protection belongs to one account-to-group binding. A NULL
-- limit means unrestricted. Deleting the binding therefore deletes its guard
-- automatically and cannot leave orphaned policy state.
ALTER TABLE account_groups
    ADD COLUMN IF NOT EXISTS upstream_billing_guard_max_multiplier DOUBLE PRECISION NULL;

ALTER TABLE account_groups
    DROP CONSTRAINT IF EXISTS account_groups_upstream_billing_guard_max_multiplier_nonnegative;
ALTER TABLE account_groups
    ADD CONSTRAINT account_groups_upstream_billing_guard_max_multiplier_nonnegative
    CHECK (
        upstream_billing_guard_max_multiplier IS NULL
        OR (
            upstream_billing_guard_max_multiplier >= 0
            AND upstream_billing_guard_max_multiplier < 'Infinity'::double precision
        )
    );

-- Preserve already-configured account-level protection by applying the same
-- threshold to every current binding of that account.
UPDATE account_groups ag
SET upstream_billing_guard_max_multiplier = a.upstream_billing_guard_max_multiplier
FROM accounts a
WHERE a.id = ag.account_id
  AND a.deleted_at IS NULL
  AND a.upstream_billing_guard_enabled = TRUE
  AND ag.upstream_billing_guard_max_multiplier IS NULL;

-- Legacy columns remain for rolling-upgrade/API compatibility but no longer
-- have global scheduling semantics.
UPDATE accounts
SET upstream_billing_guard_enabled = FALSE,
    upstream_billing_guard_blocked = FALSE,
    updated_at = NOW()
WHERE upstream_billing_guard_enabled = TRUE
   OR upstream_billing_guard_blocked = TRUE;
