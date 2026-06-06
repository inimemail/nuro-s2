SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '10min';

ALTER TABLE ops_error_logs
    ADD COLUMN IF NOT EXISTS api_key_prefix VARCHAR(32);
