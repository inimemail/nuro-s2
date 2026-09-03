ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS requested_reasoning_effort VARCHAR(20);

COMMENT ON COLUMN usage_logs.requested_reasoning_effort IS
    'Client-requested reasoning effort before group policy and model-family normalization';
