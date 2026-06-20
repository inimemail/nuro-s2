ALTER TABLE usage_logs
  ADD COLUMN IF NOT EXISTS edge_prepare_ms INT,
  ADD COLUMN IF NOT EXISTS edge_queue_wait_ms INT,
  ADD COLUMN IF NOT EXISTS edge_relay_start_ms INT,
  ADD COLUMN IF NOT EXISTS edge_fallback_reason TEXT,
  ADD COLUMN IF NOT EXISTS edge_retry_count INT;

COMMENT ON COLUMN usage_logs.edge_prepare_ms IS 'OpenAI edge-rs latency for Go /internal/edge/openai/prepare.';
COMMENT ON COLUMN usage_logs.edge_queue_wait_ms IS 'OpenAI edge-rs relay queue wait latency before upstream send.';
COMMENT ON COLUMN usage_logs.edge_relay_start_ms IS 'OpenAI edge-rs request-entry to upstream relay start latency.';
COMMENT ON COLUMN usage_logs.edge_fallback_reason IS 'OpenAI edge-rs fallback_to_go reason when available.';
COMMENT ON COLUMN usage_logs.edge_retry_count IS 'OpenAI edge-rs retry/account-switch count before completion.';
