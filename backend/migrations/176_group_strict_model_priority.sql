ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS strict_model_priority_on_model_mismatch BOOLEAN NOT NULL DEFAULT false;
