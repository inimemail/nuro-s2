//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRegistrationBlacklistTransactions(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := newUserRepositoryWithSQL(client, integrationDB)
	suffix := uuid.NewString()
	email := "blocked" + suffix + "@gmail.com"
	actor := mustCreateBlacklistFixtureUser(t, client, &service.User{Email: "actor-" + suffix + "@example.com", Role: service.RoleAdmin})
	otherAdmin := mustCreateBlacklistFixtureUser(t, client, &service.User{Email: email, Role: service.RoleAdmin})
	target := mustCreateBlacklistFixtureUser(t, client, &service.User{Email: "b.locked" + suffix + "+tag@googlemail.com", Balance: 123})
	second := mustCreateBlacklistFixtureUser(t, client, &service.User{Email: "second-" + suffix + "@example.com", Balance: 456})
	setRules := func(entries []string) {
		raw, err := json.Marshal(entries)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "INSERT INTO settings(key,value,updated_at) VALUES($1,$2,NOW()) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=NOW()", service.SettingKeyRegistrationEmailBlacklist, string(raw))
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", service.SettingKeyRegistrationEmailBlacklist)
	})
	setRules([]string{email, second.Email})
	preview, err := repo.PreviewRegistrationBlacklist(ctx, actor.ID, 0)
	require.NoError(t, err)
	require.Len(t, preview.Users, 2)
	_, err = repo.PreviewRegistrationBlacklist(ctx, target.ID, 0)
	require.Error(t, err)
	request := service.RegistrationBlacklistDisableRequest{Revision: preview.Revision, BatchID: uuid.NewString(), Confirm: true, Users: preview.Users}
	// Rules changed after preview: no partial disable and no audit success.
	setRules([]string{email})
	_, err = repo.DisableRegistrationBlacklist(ctx, actor.ID, request)
	require.Error(t, err)
	fresh, err := repo.GetByID(ctx, target.ID)
	require.NoError(t, err)
	require.Equal(t, service.StatusActive, fresh.Status)
	setRules([]string{email, second.Email})
	// Stale member after an earlier matching row: transaction rolls the first back.
	_, err = integrationDB.ExecContext(ctx, "UPDATE users SET updated_at=updated_at+interval '1 second' WHERE id=$1", second.ID)
	require.NoError(t, err)
	_, err = repo.DisableRegistrationBlacklist(ctx, actor.ID, request)
	require.Error(t, err)
	fresh, err = repo.GetByID(ctx, target.ID)
	require.NoError(t, err)
	require.Equal(t, service.StatusActive, fresh.Status)
	preview, err = repo.PreviewRegistrationBlacklist(ctx, actor.ID, 0)
	require.NoError(t, err)
	request.Users = preview.Users
	request.Revision = preview.Revision
	request.BatchID = uuid.NewString()
	ids, err := repo.DisableRegistrationBlacklist(ctx, actor.ID, request)
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{target.ID, second.ID}, ids)
	again, err := repo.DisableRegistrationBlacklist(ctx, actor.ID, request)
	require.NoError(t, err)
	require.Equal(t, ids, again)
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*) FROM audit_logs WHERE action='admin.users.registration_blacklist.disable' AND extra->>'batch_id'=$1", request.BatchID).Scan(&count))
	require.Equal(t, 1, count)
	for _, u := range []*service.User{target, second} {
		fresh, err = repo.GetByID(ctx, u.ID)
		require.NoError(t, err)
		require.Equal(t, service.StatusDisabled, fresh.Status)
		require.Equal(t, u.Balance, fresh.Balance)
	}
	fresh, err = repo.GetByID(ctx, otherAdmin.ID)
	require.NoError(t, err)
	require.Equal(t, service.StatusActive, fresh.Status)
	request.Users = request.Users[:1]
	_, err = repo.DisableRegistrationBlacklist(ctx, actor.ID, request)
	require.Error(t, err)
	// A freshly disabled or demoted actor cannot reuse an idempotency receipt.
	_, err = integrationDB.ExecContext(ctx, "UPDATE users SET status='disabled' WHERE id=$1", actor.ID)
	require.NoError(t, err)
	_, err = repo.DisableRegistrationBlacklist(ctx, actor.ID, request)
	require.Error(t, err)
}
func TestRegistrationBlacklistPreviewPagination(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := newUserRepositoryWithSQL(client, integrationDB)
	suffix := uuid.NewString()
	actor := mustCreateBlacklistFixtureUser(t, client, &service.User{Email: "page-admin-" + suffix + "@example.com", Role: service.RoleAdmin})
	entries := make([]string, 102)
	for i := range entries {
		entries[i] = fmt.Sprintf("page-%s-%03d@example.com", suffix, i)
		mustCreateBlacklistFixtureUser(t, client, &service.User{Email: entries[i]})
	}
	raw, _ := json.Marshal(entries)
	_, err := integrationDB.ExecContext(ctx, "INSERT INTO settings(key,value,updated_at) VALUES($1,$2,NOW()) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value", service.SettingKeyRegistrationEmailBlacklist, string(raw))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM settings WHERE key=$1", service.SettingKeyRegistrationEmailBlacklist)
	})
	page, err := repo.PreviewRegistrationBlacklist(ctx, actor.ID, 0)
	require.NoError(t, err)
	require.Len(t, page.Users, 100)
	require.True(t, page.HasMore)
	next, err := repo.PreviewRegistrationBlacklist(ctx, actor.ID, page.NextID)
	require.NoError(t, err)
	require.Len(t, next.Users, 2)
	require.False(t, next.HasMore)
	require.Equal(t, page.Revision, next.Revision)
	require.True(t, next.Users[0].UpdatedAt.Before(time.Now().Add(time.Second)))
}

func TestRegistrationBlacklistAuditFailureAndConcurrentIdempotency(t *testing.T) {
	ctx := t.Context()
	client := testEntClient(t)
	repo := newUserRepositoryWithSQL(client, integrationDB)
	suffix := uuid.NewString()
	actor := mustCreateBlacklistFixtureUser(t, client, &service.User{Email: "audit-admin-" + suffix + "@example.com", Role: service.RoleAdmin})
	target := mustCreateBlacklistFixtureUser(t, client, &service.User{Email: "audit-" + suffix + "@gmail.com", Balance: 321})
	// Historical imported email spelling must match the Go registration key.
	_, err := integrationDB.ExecContext(ctx, "UPDATE users SET email=$1 WHERE id=$2", "\t\u00a0audit-"+suffix+"+tag@googlemail.com.\r\n", target.ID)
	require.NoError(t, err)
	raw, err := json.Marshal([]string{target.Email})
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, "INSERT INTO settings(key,value,updated_at) VALUES($1,$2,NOW()) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value", service.SettingKeyRegistrationEmailBlacklist, string(raw))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM settings WHERE key=$1", service.SettingKeyRegistrationEmailBlacklist)
	})
	preview, err := repo.PreviewRegistrationBlacklist(ctx, actor.ID, 0)
	require.NoError(t, err)
	require.Len(t, preview.Users, 1)
	request := service.RegistrationBlacklistDisableRequest{Revision: preview.Revision, BatchID: uuid.NewString(), Confirm: true, Users: preview.Users}
	// Only this fixture's unique batch is rejected. An audit insert failure must
	// roll back the already-updated user in the same database transaction.
	constraint := "blacklist_fixture_" + fmt.Sprint(target.ID)
	_, err = integrationDB.ExecContext(ctx, "ALTER TABLE audit_logs ADD CONSTRAINT "+constraint+" CHECK (extra->>'batch_id' IS DISTINCT FROM '"+request.BatchID+"') NOT VALID")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "ALTER TABLE audit_logs DROP CONSTRAINT IF EXISTS "+constraint)
	})
	_, err = repo.DisableRegistrationBlacklist(ctx, actor.ID, request)
	require.Error(t, err)
	fresh, err := repo.GetByID(ctx, target.ID)
	require.NoError(t, err)
	require.Equal(t, service.StatusActive, fresh.Status)
	require.Equal(t, 321.0, fresh.Balance)
	_, err = integrationDB.ExecContext(ctx, "ALTER TABLE audit_logs DROP CONSTRAINT "+constraint)
	require.NoError(t, err)
	var wg sync.WaitGroup
	results := make([][]int64, 2)
	errors := make([]error, 2)
	for i := range 2 {
		wg.Go(func() { results[i], errors[i] = repo.DisableRegistrationBlacklist(ctx, actor.ID, request) })
	}
	wg.Wait()
	for i := range 2 {
		require.NoError(t, errors[i])
		require.Equal(t, []int64{target.ID}, results[i])
	}
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*) FROM audit_logs WHERE action='admin.users.registration_blacklist.disable' AND extra->>'batch_id'=$1", request.BatchID).Scan(&count))
	require.Equal(t, 1, count)
}

// These tests commit real transactions, so they own and remove their fixtures.
func mustCreateBlacklistFixtureUser(t *testing.T, client *dbent.Client, user *service.User) *service.User {
	t.Helper()
	user = mustCreateUser(t, client, user)
	t.Cleanup(func() {
		ctx := context.Background()
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM audit_logs WHERE actor_user_id=$1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id=$1", user.ID)
		require.NoError(t, err)
	})
	return user
}
