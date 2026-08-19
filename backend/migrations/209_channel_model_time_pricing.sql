-- Optional timezone-aware channel model pricing windows.
ALTER TABLE channel_model_pricing
    ADD COLUMN IF NOT EXISTS time_pricing JSONB;
