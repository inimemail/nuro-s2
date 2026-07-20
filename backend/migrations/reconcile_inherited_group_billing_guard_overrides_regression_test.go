package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration192DoesNotGuessAccountOverrideOwnership(t *testing.T) {
	content, err := FS.ReadFile("192_reconcile_inherited_group_billing_guard_overrides.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "'full_rebuild'")
	require.Contains(t, sql, `"refresh_account_metadata":true`)
	require.NotContains(t, sql, "UPDATE account_groups")
	require.NotContains(t, sql, "SET upstream_billing_guard_max_multiplier = NULL")
}
