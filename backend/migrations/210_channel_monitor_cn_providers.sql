-- Allow the OpenAI-compatible domestic providers in channel monitor checks.
-- Existing rows are unchanged; the constraint is replaced transactionally.
DO $$
BEGIN
    IF to_regclass('channel_monitors') IS NOT NULL THEN
        ALTER TABLE channel_monitors DROP CONSTRAINT IF EXISTS channel_monitors_provider_check;
        ALTER TABLE channel_monitors
            ADD CONSTRAINT channel_monitors_provider_check
            CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'kimi', 'zhipu', 'deepseek'));
    END IF;

    IF to_regclass('channel_monitor_request_templates') IS NOT NULL THEN
        ALTER TABLE channel_monitor_request_templates DROP CONSTRAINT IF EXISTS channel_monitor_request_templates_provider_check;
        ALTER TABLE channel_monitor_request_templates
            ADD CONSTRAINT channel_monitor_request_templates_provider_check
            CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'kimi', 'zhipu', 'deepseek'));
    END IF;
END $$;
