SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

-- Keep financial ownership after soft deletion of a user, key or account.
-- Resource names are random public identifiers; upstream names stay private.
CREATE TABLE gemini_cached_contents (
    id VARCHAR(48) PRIMARY KEY,
    user_id BIGINT NOT NULL,
    api_key_id BIGINT NOT NULL,
    group_id BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    requested_model VARCHAR(256) NOT NULL,
    upstream_model VARCHAR(256) NOT NULL,
    billing_model VARCHAR(256) NOT NULL,
    upstream_name TEXT NOT NULL DEFAULT '',
    display_name VARCHAR(128) NOT NULL DEFAULT '',
    state VARCHAR(32) NOT NULL CHECK (state IN ('pending','active','deleting','deleted','reconcile_required')),
    token_count BIGINT NOT NULL DEFAULT 0 CHECK (token_count >= 0),
    expires_at TIMESTAMPTZ NOT NULL,
    paid_until TIMESTAMPTZ NOT NULL,
    pricing_context JSONB NOT NULL,
    revision BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_gemini_cache_owner ON gemini_cached_contents(user_id, api_key_id, created_at, id);
CREATE INDEX idx_gemini_cache_active ON gemini_cached_contents(state, expires_at, id);

CREATE TABLE gemini_cache_operations (
    id VARCHAR(48) PRIMARY KEY,
    resource_id VARCHAR(48) NOT NULL REFERENCES gemini_cached_contents(id),
    user_id BIGINT NOT NULL,
    api_key_id BIGINT NOT NULL,
    group_id BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    subscription_id BIGINT,
    kind VARCHAR(16) NOT NULL CHECK (kind IN ('create','patch','delete')),
    idempotency_key_hash VARCHAR(64),
    fingerprint VARCHAR(64) NOT NULL,
    state VARCHAR(32) NOT NULL CHECK (state IN ('prepared','reserved','send_started','upstream_confirmed','settling','settled','released','reconcile_required')),
    baseline_expiry TIMESTAMPTZ,
    target_expiry TIMESTAMPTZ NOT NULL,
    quote JSONB NOT NULL,
    upstream_name TEXT NOT NULL DEFAULT '',
    confirmed_tokens BIGINT,
    confirmed_expiry TIMESTAMPTZ,
    send_started_at TIMESTAMPTZ,
    generation BIGINT NOT NULL DEFAULT 0,
    lease_owner VARCHAR(48),
    lease_until TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error_code VARCHAR(64) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX idx_gemini_cache_idempotency ON gemini_cache_operations(user_id, api_key_id, kind, idempotency_key_hash) WHERE idempotency_key_hash IS NOT NULL;
CREATE UNIQUE INDEX idx_gemini_cache_pending_mutation ON gemini_cache_operations(resource_id) WHERE state NOT IN ('settled','released');
CREATE INDEX idx_gemini_cache_recovery ON gemini_cache_operations(next_attempt_at, id) WHERE state NOT IN ('settled','released');

CREATE TABLE gemini_cache_charges (
    operation_id VARCHAR(48) PRIMARY KEY REFERENCES gemini_cache_operations(id),
    bill_id VARCHAR(64) NOT NULL UNIQUE,
    reserved_amount NUMERIC(20,10) NOT NULL CHECK (reserved_amount >= 0),
    actual_amount NUMERIC(20,10),
    input_cost NUMERIC(20,10) NOT NULL DEFAULT 0 CHECK (input_cost >= 0),
    storage_cost NUMERIC(20,10) NOT NULL DEFAULT 0 CHECK (storage_cost >= 0),
    state VARCHAR(24) NOT NULL CHECK (state IN ('reserved','settled','released','reconcile_required')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE gemini_cache_charge_scopes (
    operation_id VARCHAR(48) NOT NULL REFERENCES gemini_cache_operations(id),
    scope VARCHAR(24) NOT NULL CHECK (scope IN ('balance','key','subscription','platform')),
    owner_id BIGINT NOT NULL,
    amount NUMERIC(20,10) NOT NULL CHECK (amount >= 0),
    generation BIGINT NOT NULL,
    state VARCHAR(16) NOT NULL CHECK (state IN ('pending','settled','released')),
    PRIMARY KEY(operation_id,scope)
);
CREATE INDEX idx_gemini_cache_pending_scope ON gemini_cache_charge_scopes(scope,owner_id) WHERE state='pending';

-- A transactional clock avoids sequence/commit-order gaps in outbox deltas.
CREATE TABLE gemini_cache_view_clock (id INTEGER PRIMARY KEY CHECK(id=1), revision BIGINT NOT NULL);
INSERT INTO gemini_cache_view_clock VALUES(1,0);
CREATE TABLE gemini_cache_events (
    revision BIGINT PRIMARY KEY,
    resource_id VARCHAR(48) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE gemini_cache_view_nodes (
    node_id VARCHAR(48) PRIMARY KEY,
    revision BIGINT NOT NULL,
    valid_until TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
