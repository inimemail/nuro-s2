package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAdminCredentialValidation(t *testing.T) {
	for _, tc := range []struct {
		password string
		valid    bool
	}{
		{"1234567", false}, {"12345678", true}, {strings.Repeat("x", 72), true},
		{strings.Repeat("x", 73), false}, {strings.Repeat("中", 24), true}, {strings.Repeat("中", 25), false},
	} {
		require.Equal(t, tc.valid, validatePassword(tc.password) == nil)
	}
	for _, email := range []string{"Name <admin@example.com>", "admin@", "admin", "a b@example.com"} {
		require.False(t, validateEmail(email), email)
	}
	credentials, err := prepareAdminCredentials(AdminConfig{})
	require.NoError(t, err)
	require.True(t, validateEmail(credentials.Email))
	require.NoError(t, validatePassword(credentials.Password))
	next, err := prepareAdminCredentials(AdminConfig{})
	require.NoError(t, err)
	require.NotEqual(t, credentials, next)
	credentials, err = prepareAdminCredentials(AdminConfig{Email: " admin@example.com ", Password: " pass word "})
	require.NoError(t, err)
	require.Equal(t, "admin@example.com", credentials.Email)
	require.Equal(t, " pass word ", credentials.Password)
}

func TestGeneratedAdminCredentialsPrintedOnlyAfterCommit(t *testing.T) {
	for _, successful := range []bool{false, true} {
		t.Run(fmt.Sprint(successful), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(`SELECT COUNT\(1\) FROM users$`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
			mock.ExpectQuery(`SELECT COUNT\(1\) FROM users WHERE role`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
			mock.ExpectExec(`INSERT INTO users`).WillReturnResult(sqlmock.NewResult(1, 1))
			if successful {
				mock.ExpectCommit()
			} else {
				mock.ExpectCommit().WillReturnError(errors.New("commit rejected"))
			}
			read, write, err := os.Pipe()
			require.NoError(t, err)
			original := os.Stdout
			os.Stdout = write
			t.Cleanup(func() { os.Stdout = original; read.Close(); write.Close() })
			cfg := &SetupConfig{}
			created, _, bootstrapErr := bootstrapAdminUser(context.Background(), db, cfg)
			os.Stdout = original
			require.NoError(t, write.Close())
			output, err := io.ReadAll(read)
			require.NoError(t, err)
			require.Equal(t, successful, created)
			if successful {
				require.NoError(t, bootstrapErr)
				require.Contains(t, string(output), cfg.Admin.Email)
				require.Contains(t, string(output), cfg.Admin.Password)
			} else {
				require.Error(t, bootstrapErr)
				require.Empty(t, output)
				require.Empty(t, cfg.Admin.Email)
				require.Empty(t, cfg.Admin.Password)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestBootstrapAdminTransaction(t *testing.T) {
	for _, mode := range []string{"create", "skip", "users without admin", "invalid", "insert failure", "commit failure", "lock failure"} {
		t.Run(mode, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			cfg := &SetupConfig{Admin: AdminConfig{Email: "admin@example.com", Password: "password123"}}
			mock.ExpectBegin()
			lock := mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WithArgs(int64(0x4e55524f41444d))
			if mode == "lock failure" {
				lock.WillReturnError(context.DeadlineExceeded)
				mock.ExpectRollback()
			} else {
				lock.WillReturnResult(sqlmock.NewResult(0, 0))
				users, admins := 0, 0
				if mode == "skip" || mode == "users without admin" {
					users, admins = 1, 1
					if mode == "users without admin" {
						admins = 0
					}
					cfg.Admin = AdminConfig{Email: "invalid", Password: "short"}
				}
				mock.ExpectQuery(`SELECT COUNT\(1\) FROM users$`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(users))
				mock.ExpectQuery(`SELECT COUNT\(1\) FROM users WHERE role`).WithArgs(service.RoleAdmin).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(admins))
				if mode == "skip" || mode == "users without admin" || mode == "invalid" {
					if mode == "invalid" {
						cfg.Admin.Password = "short"
					}
					mock.ExpectRollback()
				} else {
					insert := mock.ExpectExec(`INSERT INTO users`).WithArgs(cfg.Admin.Email, sqlmock.AnyArg(), service.RoleAdmin, 0.0, setupDefaultAdminConcurrency(), service.StatusActive, sqlmock.AnyArg(), sqlmock.AnyArg())
					if mode == "insert failure" {
						insert.WillReturnError(errors.New("insert failed"))
						mock.ExpectRollback()
					} else {
						insert.WillReturnResult(sqlmock.NewResult(1, 1))
						if mode == "commit failure" {
							mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
						} else {
							mock.ExpectCommit()
						}
					}
				}
			}
			original := cfg.Admin
			created, _, err := bootstrapAdminUser(context.Background(), db, cfg)
			require.Equal(t, mode == "create", created)
			require.Equal(t, mode == "create" || mode == "skip" || mode == "users without admin", err == nil)
			require.Equal(t, original, cfg.Admin)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
