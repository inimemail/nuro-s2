-- Durable ownership, submission intent and frozen billing terms for native Ark tasks.
-- No cascade: completed billing records must survive account/key soft deletion.
CREATE TABLE IF NOT EXISTS seedance_tasks (
    id TEXT PRIMARY KEY,
    provider_id TEXT,
    user_id BIGINT NOT NULL,
    api_key_id BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    group_id BIGINT,
    state TEXT NOT NULL DEFAULT 'submitting',
    snapshot JSONB NOT NULL,
    response JSONB,
    terminal_response JSONB,
    settled BOOLEAN NOT NULL DEFAULT FALSE,
    effects_pending BOOLEAN NOT NULL DEFAULT FALSE,
    next_poll_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempts INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (account_id, provider_id)
);
CREATE INDEX IF NOT EXISTS seedance_tasks_poll ON seedance_tasks (next_poll_at)
    WHERE (provider_id IS NOT NULL AND NOT settled) OR effects_pending;
CREATE INDEX IF NOT EXISTS seedance_tasks_owner ON seedance_tasks (api_key_id, user_id, provider_id);
CREATE INDEX IF NOT EXISTS seedance_tasks_inflight ON seedance_tasks (account_id, user_id)
    WHERE NOT settled AND state NOT IN ('rejected', 'failed', 'cancelled', 'expired', 'succeeded');
CREATE INDEX IF NOT EXISTS seedance_tasks_user_inflight ON seedance_tasks (user_id)
    WHERE NOT settled AND state NOT IN ('rejected', 'failed', 'cancelled', 'expired', 'succeeded');
