-- Add optional latency breakdown fields for first-token troubleshooting.

ALTER TABLE ops_error_logs
  ADD COLUMN IF NOT EXISTS slot_wait_ms BIGINT,
  ADD COLUMN IF NOT EXISTS upstream_header_ms BIGINT,
  ADD COLUMN IF NOT EXISTS upstream_first_byte_ms BIGINT,
  ADD COLUMN IF NOT EXISTS first_client_flush_ms BIGINT;

ALTER TABLE usage_logs
  ADD COLUMN IF NOT EXISTS slot_wait_ms INT,
  ADD COLUMN IF NOT EXISTS upstream_header_ms INT,
  ADD COLUMN IF NOT EXISTS upstream_first_byte_ms INT,
  ADD COLUMN IF NOT EXISTS first_client_flush_ms INT;

COMMENT ON COLUMN ops_error_logs.slot_wait_ms IS 'Time spent waiting for user/account concurrency slot before forwarding.';
COMMENT ON COLUMN ops_error_logs.upstream_header_ms IS 'Time from upstream request start until upstream response headers arrive.';
COMMENT ON COLUMN ops_error_logs.upstream_first_byte_ms IS 'Time from upstream request start until first upstream response body bytes are read.';
COMMENT ON COLUMN ops_error_logs.first_client_flush_ms IS 'Time from request forwarding start until first successful downstream flush.';

COMMENT ON COLUMN usage_logs.slot_wait_ms IS 'Time spent waiting for user/account concurrency slot before forwarding.';
COMMENT ON COLUMN usage_logs.upstream_header_ms IS 'Time from upstream request start until upstream response headers arrive.';
COMMENT ON COLUMN usage_logs.upstream_first_byte_ms IS 'Time from upstream request start until first upstream response body bytes are read.';
COMMENT ON COLUMN usage_logs.first_client_flush_ms IS 'Time from request forwarding start until first successful downstream flush.';
