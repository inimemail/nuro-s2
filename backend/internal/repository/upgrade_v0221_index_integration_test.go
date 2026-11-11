//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

// This destructive fixture is deliberately separate from the full integration DB.
func TestV0221UsageCostIndexRecoveryAndPlan(t *testing.T) {
	dsn := os.Getenv("NURO_V0221_INDEX_TEST_DSN")
	if dsn == "" {
		t.Skip("requires disposable nuro_upgrade_verify_v0221_index database")
	}
	require.Contains(t, dsn, "dbname=nuro_upgrade_verify_v0221_index")
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	var database string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&database))
	require.Equal(t, "nuro_upgrade_verify_v0221_index", database)
	_, err = db.Exec(`CREATE TABLE usage_logs(id bigint PRIMARY KEY, api_key_id bigint NOT NULL, created_at timestamptz NOT NULL, actual_cost numeric(20,10) NOT NULL);
CREATE INDEX idx_usage_logs_api_key_id ON usage_logs(api_key_id);
CREATE INDEX idx_usage_logs_api_key_created_at ON usage_logs(api_key_id, created_at);
INSERT INTO usage_logs SELECT i,i%1000,now()-make_interval(secs=>i),0.001 FROM generate_series(1,200000) i`)
	require.NoError(t, err)
	defer db.Exec(`DROP TABLE usage_logs`)
	_, err = db.Exec(`VACUUM ANALYZE usage_logs`)
	require.NoError(t, err)
	query := `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT api_key_id,COALESCE(SUM(actual_cost),0),COALESCE(SUM(actual_cost) FILTER (WHERE created_at>=now()-interval '1 day'),0) FROM usage_logs WHERE api_key_id IN (11,12,13,14,15) GROUP BY api_key_id`
	explain := func(sqlText string) string {
		var body string
		require.NoError(t, db.QueryRow(sqlText).Scan(&body))
		return body
	}
	before := explain(query)
	// A failed concurrent unique build leaves a genuine invalid PostgreSQL index.
	_, err = db.Exec(`CREATE UNIQUE INDEX CONCURRENTLY idx_usage_logs_api_key_created_cost ON usage_logs(api_key_id)`)
	require.Error(t, err)
	invalid, err := indexIsInvalid(context.Background(), db, "idx_usage_logs_api_key_created_cost")
	require.NoError(t, err)
	require.True(t, invalid)
	name := "257_add_usage_logs_api_key_cost_index_notx.sql"
	body, err := migrations.FS.ReadFile(name)
	require.NoError(t, err)
	nonTx, err := validateMigrationExecutionMode(name, string(body))
	require.NoError(t, err)
	require.True(t, nonTx)
	require.NoError(t, prepareNonTransactionalMigration(context.Background(), db, name))
	_, err = db.Exec(string(body))
	require.NoError(t, err)
	_, err = db.Exec(string(body))
	require.NoError(t, err)
	invalid, err = indexIsInvalid(context.Background(), db, "idx_usage_logs_api_key_created_cost")
	require.NoError(t, err)
	require.False(t, invalid)
	_, err = db.Exec(`VACUUM ANALYZE usage_logs`)
	require.NoError(t, err)
	after := explain(query)
	require.Contains(t, after, "Index Only Scan")
	require.Contains(t, after, "idx_usage_logs_api_key_created_cost")
	insert := `EXPLAIN (ANALYZE, BUFFERS, WAL, FORMAT JSON) INSERT INTO usage_logs SELECT i,i%1000,now(),0.001 FROM generate_series(200001,210000) i`
	indexedWrite := explain(insert)
	_, err = db.Exec(`DROP INDEX idx_usage_logs_api_key_created_cost`)
	require.NoError(t, err)
	unindexedWrite := explain(strings.ReplaceAll(insert, "200001,210000", "210001,220000"))
	for label, body := range map[string]string{"read_before": before, "read_after": after, "write_indexed": indexedWrite, "write_without_index": unindexedWrite} {
		var decoded any
		require.NoError(t, json.Unmarshal([]byte(body), &decoded))
		compact, _ := json.Marshal(decoded)
		t.Logf("%s=%s", label, compact)
	}
}
