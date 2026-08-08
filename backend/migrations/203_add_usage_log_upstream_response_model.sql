ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS upstream_response_model VARCHAR(200),
    ADD COLUMN IF NOT EXISTS upstream_model_mismatch BOOLEAN;

COMMENT ON COLUMN usage_logs.upstream_response_model IS
    'Model name reported by the upstream response; nullable observational audit metadata';
