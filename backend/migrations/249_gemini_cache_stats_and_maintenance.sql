SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE channel_account_stats_model_pricing
    ADD COLUMN cache_storage_cost_per_token_per_hour NUMERIC(20,10)
    CHECK (cache_storage_cost_per_token_per_hour >= 0 AND cache_storage_cost_per_token_per_hour <> 'NaN'::numeric);
CREATE INDEX idx_gemini_cache_pending_owner ON gemini_cache_operations(user_id,api_key_id,group_id,id)
    WHERE state NOT IN ('settled','released');
CREATE INDEX idx_gemini_cache_authorization_revision ON gemini_cache_events(revision)
    WHERE resource_id='authorization';
CREATE INDEX idx_gemini_cache_nodes_expiry ON gemini_cache_view_nodes(valid_until);
