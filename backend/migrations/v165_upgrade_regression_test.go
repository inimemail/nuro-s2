package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestV165AdaptedMigrationSequence(t *testing.T) {
	tests := []struct {
		name     string
		contains []string
	}{
		{name: "198_add_usage_log_session_id.sql", contains: []string{"usage_logs", "batch_image_jobs", "session_id"}},
		{name: "199_allow_live_usage_request_type.sql", contains: []string{"usage_logs_request_type_check", "request_type <= 5", "NOT VALID"}},
		{name: "200_add_group_allow_live.sql", contains: []string{"allow_live", "DEFAULT false"}},
		{name: "201_add_users_email_alias_dedup_index_notx.sql", contains: []string{"CREATE INDEX CONCURRENTLY IF NOT EXISTS", "REPLACE(LOWER(TRIM(email)), '.', '')", "deleted_at IS NULL"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := FS.ReadFile(tt.name)
			require.NoError(t, err)
			sql := string(content)
			for _, want := range tt.contains {
				require.Contains(t, sql, want)
			}
			if strings.HasSuffix(tt.name, "_notx.sql") {
				require.NotContains(t, strings.ToUpper(sql), "BEGIN;")
			}
		})
	}
}
