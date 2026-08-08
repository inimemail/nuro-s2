CREATE INDEX CONCURRENTLY IF NOT EXISTS usagelog_upstream_model_mismatch
    ON usage_logs (upstream_model_mismatch);
