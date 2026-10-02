-- Compatibility bridge for upstream model_allowlist naming. Existing
-- deployments keep models_list_config during the transition; values are
-- copied into the new column for forward-compatible tooling.
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS model_allowlist JSONB NOT NULL DEFAULT '{}'::jsonb;
UPDATE groups
SET model_allowlist = models_list_config
WHERE (model_allowlist = '{}'::jsonb OR model_allowlist IS NULL)
  AND models_list_config IS NOT NULL;

CREATE OR REPLACE FUNCTION sync_group_model_allowlist_compat()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.models_list_config IS NOT NULL
           AND NEW.models_list_config IS DISTINCT FROM '{}'::jsonb THEN
            NEW.model_allowlist := NEW.models_list_config;
        ELSIF NEW.model_allowlist IS NOT NULL
              AND NEW.model_allowlist IS DISTINCT FROM '{}'::jsonb THEN
            NEW.models_list_config := NEW.model_allowlist;
        END IF;
    ELSIF NEW.models_list_config IS DISTINCT FROM OLD.models_list_config THEN
        NEW.model_allowlist := NEW.models_list_config;
    ELSIF NEW.model_allowlist IS DISTINCT FROM OLD.model_allowlist THEN
        NEW.models_list_config := NEW.model_allowlist;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_groups_model_allowlist_compat ON groups;
CREATE TRIGGER trg_groups_model_allowlist_compat
BEFORE INSERT OR UPDATE ON groups
FOR EACH ROW EXECUTE FUNCTION sync_group_model_allowlist_compat();
