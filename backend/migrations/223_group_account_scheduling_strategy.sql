ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS account_scheduling_strategy VARCHAR(30) NOT NULL DEFAULT 'strict_priority';

UPDATE groups
SET account_scheduling_strategy = 'strict_priority'
WHERE account_scheduling_strategy IS NULL
   OR account_scheduling_strategy NOT IN ('strict_priority', 'health_first');

-- Migration 194's broad group trigger predates this field and intentionally
-- skips updates when all fields it knows are unchanged. Invalidate API-key
-- auth snapshots for strategy-only edits so the setting takes effect promptly.
CREATE OR REPLACE FUNCTION enqueue_group_account_scheduling_strategy_auth_cache_invalidation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    PERFORM enqueue_auth_cache_invalidation(k.key)
    FROM api_keys AS k
    WHERE k.group_id = NEW.id
      AND k.deleted_at IS NULL
      AND k.key <> '';
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_groups_account_scheduling_strategy_auth_cache_invalidation ON groups;
CREATE TRIGGER trg_groups_account_scheduling_strategy_auth_cache_invalidation
AFTER UPDATE OF account_scheduling_strategy ON groups
FOR EACH ROW
WHEN (OLD.account_scheduling_strategy IS DISTINCT FROM NEW.account_scheduling_strategy)
EXECUTE FUNCTION enqueue_group_account_scheduling_strategy_auth_cache_invalidation();

COMMENT ON COLUMN groups.account_scheduling_strategy IS
    '账号调度策略：strict_priority 保持原有优先级调度，health_first 启用健康优先调度';
