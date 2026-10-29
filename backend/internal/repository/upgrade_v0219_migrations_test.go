package repository

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUpgradeV0219MigrationContracts(t *testing.T) {
	dsn := os.Getenv("NURO_CACHE_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated nuro_upgrade_verify database")
	}
	require.Contains(t, dsn, "dbname=nuro_upgrade_verify")
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	var database string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&database))
	require.Equal(t, "nuro_upgrade_verify", database)

	cases := []struct {
		name, file, before, drift string
	}{
		{"image fresh and repeat", "255", "", ""},
		{"image wrong default", "255", "ALTER TABLE groups ADD image_count_billing boolean NOT NULL DEFAULT true", ""},
		{"image missing default", "255", "ALTER TABLE groups ADD image_count_billing boolean NOT NULL", ""},
		{"image nullable", "255", "ALTER TABLE groups ADD image_count_billing boolean DEFAULT false", ""},
		{"image wrong type", "255", "ALTER TABLE groups ADD image_count_billing text NOT NULL DEFAULT 'false'", ""},
		{"scoped fresh and repeat", "256", "", ""},
		{"scoped missing column", "256", "", "ALTER TABLE scoped_admin_api_keys DROP COLUMN policy"},
		{"scoped wrong default", "256", "", "ALTER TABLE scoped_admin_api_keys ALTER COLUMN enabled SET DEFAULT false"},
		{"scoped nullable expiry flag", "256", "", "ALTER TABLE scoped_admin_api_keys ALTER COLUMN enabled DROP NOT NULL"},
		{"scoped missing key uniqueness", "256", "", "ALTER TABLE scoped_admin_api_keys DROP CONSTRAINT scoped_admin_api_keys_key_hash_key"},
		{"scoped missing receipt uniqueness", "256", "", "ALTER TABLE scoped_admin_operations DROP CONSTRAINT scoped_admin_operations_pkey"},
		{"scoped missing receipt foreign key", "256", "", "ALTER TABLE scoped_admin_operations DROP CONSTRAINT scoped_admin_operations_key_id_fkey"},
		{"scoped missing creator foreign key", "256", "", "ALTER TABLE scoped_admin_api_keys DROP CONSTRAINT scoped_admin_api_keys_creator_id_fkey"},
		{"scoped missing revision guard", "256", "", "ALTER TABLE scoped_admin_api_keys DROP CONSTRAINT scoped_admin_api_keys_policy_revision_check"},
		{"scoped missing rpm guard", "256", "", "ALTER TABLE scoped_admin_api_keys DROP CONSTRAINT scoped_admin_api_keys_rpm_check"},
	}
	files, err := migrations.FS.ReadDir(".")
	require.NoError(t, err)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var migration []byte
			for _, file := range files {
				if strings.HasPrefix(file.Name(), tc.file+"_") {
					migration, err = migrations.FS.ReadFile(file.Name())
					require.NoError(t, err)
				}
			}
			require.NotEmpty(t, migration)
			tx, err := db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			// Transaction-local schemas avoid touching the full fixture schema.
			schema := "v0219_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			_, err = tx.Exec(`CREATE SCHEMA ` + schema + `; SET LOCAL search_path TO ` + schema + `; CREATE TABLE users(id bigint PRIMARY KEY); CREATE TABLE groups(id bigint PRIMARY KEY)`)
			require.NoError(t, err)
			if tc.before != "" {
				_, err = tx.Exec(tc.before)
				require.NoError(t, err)
			}
			if tc.drift != "" {
				_, err = tx.Exec(string(migration))
				require.NoError(t, err)
				_, err = tx.Exec(tc.drift)
				require.NoError(t, err)
			}
			_, err = tx.Exec(string(migration))
			if tc.before != "" || tc.drift != "" {
				require.ErrorContains(t, err, "incompatible")
				return
			}
			require.NoError(t, err)
			_, err = tx.Exec(string(migration))
			require.NoError(t, err, "new migrations must be repeatable")
		})
	}
}
