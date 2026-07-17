ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS upstream_billing_guard_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS upstream_billing_guard_max_multiplier DOUBLE PRECISION NOT NULL DEFAULT 1.0,
    ADD COLUMN IF NOT EXISTS upstream_billing_guard_blocked BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS upstream_billing_guard_observed_multiplier DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS upstream_billing_guard_evaluated_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_accounts_upstream_billing_guard_blocked
    ON accounts (upstream_billing_guard_blocked)
    WHERE deleted_at IS NULL AND upstream_billing_guard_blocked = TRUE;
