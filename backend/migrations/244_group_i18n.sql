SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
ALTER TABLE groups ADD COLUMN IF NOT EXISTS i18n JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE groups ADD CONSTRAINT groups_i18n_object CHECK (jsonb_typeof(i18n) = 'object');
