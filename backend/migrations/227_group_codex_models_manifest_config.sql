ALTER TABLE groups
  ADD COLUMN IF NOT EXISTS codex_models_manifest_config JSONB NOT NULL DEFAULT '{}'::jsonb;
