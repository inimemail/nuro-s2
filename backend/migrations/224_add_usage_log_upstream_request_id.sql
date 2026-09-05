-- Optional direct upstream correlation identifier. Historical and WS rows remain NULL.
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS upstream_request_id VARCHAR(128);
