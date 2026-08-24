DO $$
BEGIN
    ALTER TABLE channel_monitors DROP CONSTRAINT IF EXISTS channel_monitors_provider_check;
    ALTER TABLE channel_monitors ADD CONSTRAINT channel_monitors_provider_check
        CHECK (provider IN ('openai','anthropic','gemini','grok','antigravity','kimi','zhipu','deepseek'));
    ALTER TABLE channel_monitor_request_templates DROP CONSTRAINT IF EXISTS channel_monitor_request_templates_provider_check;
    ALTER TABLE channel_monitor_request_templates ADD CONSTRAINT channel_monitor_request_templates_provider_check
        CHECK (provider IN ('openai','anthropic','gemini','grok','antigravity','kimi','zhipu','deepseek'));
END $$;

ALTER TABLE channel_monitors ADD COLUMN IF NOT EXISTS check_mode VARCHAR(32) NOT NULL DEFAULT 'probe';
ALTER TABLE channel_monitors DROP CONSTRAINT IF EXISTS channel_monitors_check_mode_check;
ALTER TABLE channel_monitors ADD CONSTRAINT channel_monitors_check_mode_check CHECK (check_mode IN ('probe','quota','quota_probe'));
ALTER TABLE channel_monitors ADD COLUMN IF NOT EXISTS account_id BIGINT REFERENCES accounts(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_channel_monitors_account_id ON channel_monitors(account_id);
ALTER TABLE channel_monitor_histories ADD COLUMN IF NOT EXISTS quota JSONB;
INSERT INTO settings (key, value) VALUES ('channel_monitor_show_quota', 'false') ON CONFLICT (key) DO NOTHING;
