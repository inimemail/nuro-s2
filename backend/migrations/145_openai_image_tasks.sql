CREATE TABLE IF NOT EXISTS openai_image_tasks (
    id BIGSERIAL PRIMARY KEY,
    owner_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    api_key_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL,
    user_concurrency INTEGER NOT NULL DEFAULT 0,
    endpoint TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    request_body BYTEA NOT NULL,
    request_headers JSONB NOT NULL DEFAULT '{}'::jsonb,
    response_body BYTEA NULL,
    status_code INTEGER NOT NULL DEFAULT 0,
    error_message TEXT NOT NULL DEFAULT '',
    locked_by TEXT NOT NULL DEFAULT '',
    locked_until TIMESTAMPTZ NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    started_at TIMESTAMPTZ NULL,
    finished_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (owner_id, task_id)
);

CREATE INDEX IF NOT EXISTS openai_image_tasks_owner_updated_idx
    ON openai_image_tasks (owner_id, updated_at DESC);

CREATE INDEX IF NOT EXISTS openai_image_tasks_queue_idx
    ON openai_image_tasks (status, created_at ASC);

CREATE INDEX IF NOT EXISTS openai_image_tasks_locked_until_idx
    ON openai_image_tasks (locked_until)
    WHERE status = 'running';

CREATE INDEX IF NOT EXISTS openai_image_tasks_finished_cleanup_idx
    ON openai_image_tasks (finished_at)
    WHERE status IN ('success', 'error');
