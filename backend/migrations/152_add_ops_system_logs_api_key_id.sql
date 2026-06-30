-- Persist API key database id as a queryable system log column.

ALTER TABLE ops_system_logs
  ADD COLUMN IF NOT EXISTS api_key_id BIGINT;
