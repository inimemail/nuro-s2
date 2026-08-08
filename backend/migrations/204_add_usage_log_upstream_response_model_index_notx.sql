CREATE INDEX CONCURRENTLY IF NOT EXISTS usage_logs_upstream_response_model_idx
    ON usage_logs (upstream_response_model)
    WHERE upstream_response_model IS NOT NULL;
