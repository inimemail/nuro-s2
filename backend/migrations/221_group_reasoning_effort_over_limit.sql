ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS max_reasoning_effort_over_limit VARCHAR(20) NOT NULL DEFAULT 'downgrade';

COMMENT ON COLUMN groups.max_reasoning_effort_over_limit IS
    'Behavior for explicit reasoning effort above the group ceiling: downgrade or deny';
