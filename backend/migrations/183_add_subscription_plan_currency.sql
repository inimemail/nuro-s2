-- Display-only ISO 4217 currency label. Empty preserves existing rendering.
ALTER TABLE subscription_plans
    ADD COLUMN IF NOT EXISTS currency VARCHAR(3) NOT NULL DEFAULT '';
