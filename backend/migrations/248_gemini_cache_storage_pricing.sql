SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE channel_model_pricing
    ADD COLUMN cache_storage_cost_per_token_per_hour NUMERIC(20,10)
    CHECK (cache_storage_cost_per_token_per_hour >= 0 AND cache_storage_cost_per_token_per_hour <> 'NaN'::numeric);
COMMENT ON COLUMN channel_model_pricing.cache_storage_cost_per_token_per_hour IS
    'Gemini explicit cache storage USD per token-hour; NULL uses an identified provider price card';
