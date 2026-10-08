//go:build integration

package setup

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestAdminBootstrapConcurrentReplicas(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("Docker required in CI: %v", err)
		}
		t.Skip("Docker unavailable; concurrent PostgreSQL bootstrap requires an isolated container")
	}
	container, err := tcpostgres.Run(ctx, "postgres:18.1-alpine3.23", tcpostgres.WithDatabase("bootstrap_test"), tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("postgres"), tcpostgres.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, container.Terminate(context.Background())) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	first, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer first.Close()
	second, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer second.Close()
	_, err = first.ExecContext(ctx, `CREATE TABLE users (email text UNIQUE, password_hash text, role text, balance numeric, concurrency integer, status text, created_at timestamptz, updated_at timestamptz)`)
	require.NoError(t, err)
	start := make(chan struct{})
	type result struct {
		created bool
		err     error
	}
	results := make(chan result, 2)
	for _, db := range []*sql.DB{first, second} {
		go func() {
			<-start
			created, _, err := bootstrapAdminUser(ctx, db, &SetupConfig{})
			results <- result{created, err}
		}()
	}
	close(start)
	created := 0
	for range 2 {
		r := <-results
		require.NoError(t, r.err)
		if r.created {
			created++
		}
	}
	require.Equal(t, 1, created)
	var count int
	require.NoError(t, first.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&count))
	require.Equal(t, 1, count)
	// A competing installer must respect cancellation while the lock is held.
	tx, err := first.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x4e55524f41444d))
	require.NoError(t, err)
	waitCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	createdAfterTimeout, _, err := bootstrapAdminUser(waitCtx, second, &SetupConfig{})
	require.Error(t, err)
	require.False(t, createdAfterTimeout)
}
