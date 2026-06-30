-- Align user platform quota CHECK constraint with the Grok platform.
-- Validate the expanded constraint before briefly swapping names, so the
-- stricter table lock is not held during the table scan.

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check_v2
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok')) NOT VALID;

ALTER TABLE user_platform_quotas
    VALIDATE CONSTRAINT user_platform_quotas_platform_check_v2;

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    RENAME CONSTRAINT user_platform_quotas_platform_check_v2 TO user_platform_quotas_platform_check;
