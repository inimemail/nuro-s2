SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
CREATE TABLE IF NOT EXISTS scoped_admin_api_keys (
 id BIGSERIAL PRIMARY KEY,
 name VARCHAR(100) NOT NULL,
 description VARCHAR(1000) NOT NULL DEFAULT '',
 key_prefix VARCHAR(32) NOT NULL,
 key_hash VARCHAR(64) NOT NULL UNIQUE,
 enabled BOOLEAN NOT NULL DEFAULT true,
 expires_at TIMESTAMPTZ,
 ip_allowlist JSONB NOT NULL DEFAULT '[]',
 policy JSONB NOT NULL,
 policy_revision BIGINT NOT NULL DEFAULT 1 CHECK (policy_revision>0),
 rpm INTEGER NOT NULL DEFAULT 60 CHECK (rpm BETWEEN 1 AND 600),
 creator_id BIGINT NOT NULL REFERENCES users(id),
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 last_used_at TIMESTAMPTZ,
 revoked_at TIMESTAMPTZ,
 deleted_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_scoped_admin_keys_creator ON scoped_admin_api_keys(creator_id) WHERE deleted_at IS NULL;
-- A committed business operation and its minimal receipt share one transaction.
-- Policy/secret revisions deliberately do not participate in deduplication.
CREATE TABLE IF NOT EXISTS scoped_admin_operations (
 key_id BIGINT NOT NULL REFERENCES scoped_admin_api_keys(id),
 namespace VARCHAR(256) NOT NULL,
 idempotency_hash VARCHAR(64) NOT NULL,
 fingerprint VARCHAR(64) NOT NULL,
 resource_id BIGINT NOT NULL,
 result JSONB NOT NULL DEFAULT '{}',
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(key_id,namespace,idempotency_hash)
);
-- IF NOT EXISTS must not accept a partial pre-existing table: missing unique
-- receipts, defaults or foreign keys would silently weaken authorization and
-- replay guarantees. Reject drift; never repair or overwrite existing data.
DO $$
DECLARE invalid_column text;
BEGIN
 SELECT expected.table_name || '.' || expected.column_name INTO invalid_column
 FROM (VALUES
  ('scoped_admin_api_keys','id','bigint','NO',NULL::integer,'serial'),
  ('scoped_admin_api_keys','name','character varying','NO',100,NULL),
  ('scoped_admin_api_keys','description','character varying','NO',1000,'''''::character varying'),
  ('scoped_admin_api_keys','key_prefix','character varying','NO',32,NULL),
  ('scoped_admin_api_keys','key_hash','character varying','NO',64,NULL),
  ('scoped_admin_api_keys','enabled','boolean','NO',NULL,'true'),
  ('scoped_admin_api_keys','expires_at','timestamp with time zone','YES',NULL,NULL),
  ('scoped_admin_api_keys','ip_allowlist','jsonb','NO',NULL,'''[]''::jsonb'),
  ('scoped_admin_api_keys','policy','jsonb','NO',NULL,NULL),
  ('scoped_admin_api_keys','policy_revision','bigint','NO',NULL,'1'),
  ('scoped_admin_api_keys','rpm','integer','NO',NULL,'60'),
  ('scoped_admin_api_keys','creator_id','bigint','NO',NULL,NULL),
  ('scoped_admin_api_keys','created_at','timestamp with time zone','NO',NULL,'now()'),
  ('scoped_admin_api_keys','updated_at','timestamp with time zone','NO',NULL,'now()'),
  ('scoped_admin_api_keys','last_used_at','timestamp with time zone','YES',NULL,NULL),
  ('scoped_admin_api_keys','revoked_at','timestamp with time zone','YES',NULL,NULL),
  ('scoped_admin_api_keys','deleted_at','timestamp with time zone','YES',NULL,NULL),
  ('scoped_admin_operations','key_id','bigint','NO',NULL,NULL),
  ('scoped_admin_operations','namespace','character varying','NO',256,NULL),
  ('scoped_admin_operations','idempotency_hash','character varying','NO',64,NULL),
  ('scoped_admin_operations','fingerprint','character varying','NO',64,NULL),
  ('scoped_admin_operations','resource_id','bigint','NO',NULL,NULL),
  ('scoped_admin_operations','result','jsonb','NO',NULL,'''{}''::jsonb'),
  ('scoped_admin_operations','created_at','timestamp with time zone','NO',NULL,'now()')
 ) AS expected(table_name,column_name,data_type,is_nullable,max_length,column_default)
 LEFT JOIN information_schema.columns actual
 ON actual.table_schema=current_schema() AND actual.table_name=expected.table_name AND actual.column_name=expected.column_name
 WHERE actual.column_name IS NULL OR actual.data_type<>expected.data_type OR actual.is_nullable<>expected.is_nullable
 OR actual.character_maximum_length IS DISTINCT FROM expected.max_length
 OR CASE WHEN expected.column_default='serial' THEN
     actual.column_default IS NULL OR actual.column_default NOT LIKE 'nextval(%'
    ELSE actual.column_default IS DISTINCT FROM expected.column_default END
 LIMIT 1;
 IF invalid_column IS NOT NULL THEN
  RAISE EXCEPTION 'incompatible scoped admin column: %', invalid_column;
 END IF;

 IF EXISTS (
  SELECT 1 FROM (VALUES
   ('scoped_admin_api_keys','p','{id}'::text[],NULL::text,NULL::text[]),
   ('scoped_admin_api_keys','u','{key_hash}',NULL,NULL),
   ('scoped_admin_api_keys','f','{creator_id}','users','{id}'),
   ('scoped_admin_operations','p','{key_id,namespace,idempotency_hash}',NULL,NULL),
   ('scoped_admin_operations','f','{key_id}','scoped_admin_api_keys','{id}')
  ) AS expected(table_name,kind,columns,foreign_table,foreign_columns)
  WHERE NOT EXISTS (
   SELECT 1 FROM pg_constraint c
   WHERE c.conrelid=to_regclass(format('%I.%I',current_schema(),expected.table_name)) AND c.contype::text=expected.kind
   AND c.convalidated AND NOT c.condeferrable
   AND (SELECT array_agg(a.attname::text ORDER BY k.ordinality) FROM unnest(c.conkey) WITH ORDINALITY k(num,ordinality) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.num)=expected.columns
   AND (expected.foreign_table IS NULL OR (
    c.confrelid=to_regclass(format('%I.%I',current_schema(),expected.foreign_table)) AND c.confdeltype='a'
    AND (SELECT array_agg(a.attname::text ORDER BY k.ordinality) FROM unnest(c.confkey) WITH ORDINALITY k(num,ordinality) JOIN pg_attribute a ON a.attrelid=c.confrelid AND a.attnum=k.num)=expected.foreign_columns
   ))
  )
 ) OR NOT EXISTS (
  SELECT 1 FROM pg_constraint WHERE conrelid='scoped_admin_api_keys'::regclass AND contype='c' AND convalidated AND pg_get_expr(conbin,conrelid)='(policy_revision > 0)'
 ) OR NOT EXISTS (
  SELECT 1 FROM pg_constraint WHERE conrelid='scoped_admin_api_keys'::regclass AND contype='c' AND convalidated AND pg_get_expr(conbin,conrelid)='((rpm >= 1) AND (rpm <= 600))'
 ) THEN
  RAISE EXCEPTION 'incompatible scoped admin constraints';
 END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_scoped_admin_operations_retention ON scoped_admin_operations(key_id,created_at);
