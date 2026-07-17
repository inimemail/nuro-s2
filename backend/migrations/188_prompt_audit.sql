-- Async prompt audit metadata. Raw prompts and endpoint credentials are never
-- persisted in PostgreSQL; transient payloads live encrypted in Redis with TTL.

CREATE TABLE IF NOT EXISTS prompt_audit_jobs (
    id                    BIGSERIAL PRIMARY KEY,
    request_id            VARCHAR(128) NOT NULL DEFAULT '',
    user_id               BIGINT REFERENCES users(id) ON DELETE SET NULL,
    user_email_snapshot   VARCHAR(320) NOT NULL DEFAULT '',
    api_key_id            BIGINT REFERENCES api_keys(id) ON DELETE SET NULL,
    api_key_name_snapshot VARCHAR(255) NOT NULL DEFAULT '',
    group_id              BIGINT REFERENCES groups(id) ON DELETE SET NULL,
    group_name            VARCHAR(255) NOT NULL DEFAULT '',
    provider              VARCHAR(64) NOT NULL DEFAULT '',
    endpoint              VARCHAR(128) NOT NULL DEFAULT '',
    protocol              VARCHAR(64) NOT NULL DEFAULT '',
    model                 VARCHAR(255) NOT NULL DEFAULT '',
    prompt_hash           VARCHAR(64) NOT NULL DEFAULT '',
    redacted_preview      TEXT NOT NULL DEFAULT '',
    prompt_length         INT NOT NULL DEFAULT 0,
    message_count         INT NOT NULL DEFAULT 0,
    stage                 VARCHAR(32) NOT NULL DEFAULT 'http',
    config_version        BIGINT NOT NULL DEFAULT 1,
    status                VARCHAR(32) NOT NULL DEFAULT 'staging',
    attempts              INT NOT NULL DEFAULT 0,
    processing_started_at TIMESTAMPTZ,
    processed_at          TIMESTAMPTZ,
    last_error_code       VARCHAR(64) NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_prompt_audit_jobs_status
        CHECK (status IN ('staging', 'queued', 'processing', 'retry', 'done', 'failed')),
    CONSTRAINT chk_prompt_audit_jobs_nonnegative
        CHECK (attempts >= 0 AND prompt_length >= 0 AND message_count >= 0 AND config_version >= 1)
);

CREATE TABLE IF NOT EXISTS prompt_audit_events (
    id                    BIGSERIAL PRIMARY KEY,
    job_id                BIGINT NOT NULL REFERENCES prompt_audit_jobs(id) ON DELETE CASCADE,
    request_id            VARCHAR(128) NOT NULL DEFAULT '',
    user_id               BIGINT REFERENCES users(id) ON DELETE SET NULL,
    user_email_snapshot   VARCHAR(320) NOT NULL DEFAULT '',
    api_key_id            BIGINT REFERENCES api_keys(id) ON DELETE SET NULL,
    api_key_name_snapshot VARCHAR(255) NOT NULL DEFAULT '',
    group_id              BIGINT REFERENCES groups(id) ON DELETE SET NULL,
    group_name            VARCHAR(255) NOT NULL DEFAULT '',
    provider              VARCHAR(64) NOT NULL DEFAULT '',
    endpoint              VARCHAR(128) NOT NULL DEFAULT '',
    protocol              VARCHAR(64) NOT NULL DEFAULT '',
    model                 VARCHAR(255) NOT NULL DEFAULT '',
    prompt_hash           VARCHAR(64) NOT NULL DEFAULT '',
    redacted_preview      TEXT NOT NULL DEFAULT '',
    prompt_length         INT NOT NULL DEFAULT 0,
    message_count         INT NOT NULL DEFAULT 0,
    stage                 VARCHAR(32) NOT NULL DEFAULT 'http',
    decision              VARCHAR(32) NOT NULL DEFAULT 'pass',
    risk_level            VARCHAR(32) NOT NULL DEFAULT 'low',
    action                VARCHAR(32) NOT NULL DEFAULT 'Allow',
    categories            JSONB NOT NULL DEFAULT '[]'::jsonb,
    scanner_backend       VARCHAR(64) NOT NULL DEFAULT 'qwen3guard-openai',
    scanner_version       VARCHAR(128) NOT NULL DEFAULT '',
    guard_endpoint_id     VARCHAR(128) NOT NULL DEFAULT '',
    config_version        BIGINT NOT NULL DEFAULT 1,
    latency_ms            INT NOT NULL DEFAULT 0,
    error_code            VARCHAR(64) NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_prompt_audit_events_decision
        CHECK (decision IN ('pass', 'flag', 'critical', 'unavailable')),
    CONSTRAINT chk_prompt_audit_events_risk
        CHECK (risk_level IN ('low', 'medium', 'high', 'critical', 'unknown')),
    CONSTRAINT chk_prompt_audit_events_categories
        CHECK (jsonb_typeof(categories) = 'array'),
    CONSTRAINT chk_prompt_audit_events_nonnegative
        CHECK (prompt_length >= 0 AND message_count >= 0 AND config_version >= 1 AND latency_ms >= 0)
);

CREATE INDEX IF NOT EXISTS idx_prompt_audit_jobs_status
    ON prompt_audit_jobs(status, updated_at, id);
CREATE INDEX IF NOT EXISTS idx_prompt_audit_jobs_created
    ON prompt_audit_jobs(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_prompt_audit_jobs_user
    ON prompt_audit_jobs(user_id) WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_prompt_audit_jobs_api_key
    ON prompt_audit_jobs(api_key_id) WHERE api_key_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_prompt_audit_jobs_group
    ON prompt_audit_jobs(group_id) WHERE group_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_prompt_audit_events_created
    ON prompt_audit_events(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_prompt_audit_events_decision
    ON prompt_audit_events(decision, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_prompt_audit_events_risk
    ON prompt_audit_events(risk_level, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_prompt_audit_events_group
    ON prompt_audit_events(group_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_prompt_audit_events_user
    ON prompt_audit_events(user_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_prompt_audit_events_api_key
    ON prompt_audit_events(api_key_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_prompt_audit_events_hash
    ON prompt_audit_events(prompt_hash);
CREATE UNIQUE INDEX IF NOT EXISTS idx_prompt_audit_events_job
    ON prompt_audit_events(job_id);
