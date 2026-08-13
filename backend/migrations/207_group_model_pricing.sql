ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS long_context_pricing_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN IF NOT EXISTS model_pricing JSONB;

COMMENT ON COLUMN groups.long_context_pricing_enabled IS
    'Whether official long-context pricing tiers are enabled for this group';
COMMENT ON COLUMN groups.model_pricing IS
    'Per-model group pricing overrides';
