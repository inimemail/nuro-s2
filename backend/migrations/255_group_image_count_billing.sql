SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
ALTER TABLE groups ADD COLUMN IF NOT EXISTS image_count_billing BOOLEAN NOT NULL DEFAULT false;
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='groups' AND column_name='image_count_billing' AND data_type='boolean' AND is_nullable='NO' AND column_default='false') THEN
  RAISE EXCEPTION 'groups.image_count_billing has an incompatible definition';
 END IF;
END $$;
COMMENT ON COLUMN groups.image_count_billing IS 'Explicit per-image billing for dedicated image endpoints; disabled by default';
