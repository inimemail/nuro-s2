SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

-- A durable tombstone cancels only intents never recorded as operations.
-- It shares Prepare's advisory lock so delayed requests cannot create later.
CREATE TABLE gemini_cache_cancelled_intents (
 user_id BIGINT NOT NULL,
 api_key_id BIGINT NOT NULL,
 kind VARCHAR(16) NOT NULL CHECK(kind IN ('create','patch','delete')),
 idempotency_key_hash VARCHAR(64) NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY(user_id,api_key_id,kind,idempotency_key_hash)
);
