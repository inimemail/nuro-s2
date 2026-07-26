-- Persist only explicit client-provided correlation identifiers. These values
-- are never derived from prompt_cache_key, content hashes, or sticky bindings.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS session_id VARCHAR(255);
ALTER TABLE batch_image_jobs ADD COLUMN IF NOT EXISTS session_id VARCHAR(255);
