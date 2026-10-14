SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE INDEX IF NOT EXISTS idx_gemini_cache_cancelled_intents_created
    ON gemini_cache_cancelled_intents(created_at);
