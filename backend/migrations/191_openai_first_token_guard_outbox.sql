CREATE TABLE IF NOT EXISTS openai_first_token_guard_outbox (
    guard_key_hash BYTEA PRIMARY KEY,
    guard_key TEXT NOT NULL,
    real_token_ms INTEGER NOT NULL CHECK (real_token_ms > 0),
    recorded_at_ns BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_openai_first_token_guard_outbox_updated_at
    ON openai_first_token_guard_outbox (updated_at);
