package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/stretchr/testify/require"
)

func TestNormalizePasskeyName(t *testing.T) {
	require.Equal(t, defaultPasskeyName, normalizePasskeyName("   "))
	require.Equal(t, "Laptop", normalizePasskeyName("  Laptop  "))
	require.Len(t, []rune(normalizePasskeyName(strings.Repeat("x", maxPasskeyNameLength+20))), maxPasskeyNameLength)
}

func TestPasskeySummaryUsesCurrentBackupState(t *testing.T) {
	record := &PasskeyCredentialRecord{Credential: webauthn.Credential{Flags: webauthn.CredentialFlags{BackupEligible: true}}}
	require.False(t, passkeySummary(record).Backup)
	record.Credential.Flags.BackupState = true
	require.True(t, passkeySummary(record).Backup)
}

type passkeyTestUserRepo struct {
	UserRepository
	user  *User
	calls int
}

func (r *passkeyTestUserRepo) GetByID(context.Context, int64) (*User, error) {
	r.calls++
	return r.user, nil
}

type passkeyTestRepo struct {
	PasskeyRepository
	handleCalled bool
	deleteCalled bool
}

func (r *passkeyTestRepo) EnsureUserHandle(_ context.Context, _ int64, candidate []byte) ([]byte, error) {
	r.handleCalled = true
	return candidate, nil
}

func (r *passkeyTestRepo) ListByUserID(context.Context, int64) ([]PasskeyCredentialRecord, error) {
	return nil, nil
}

func (r *passkeyTestRepo) Delete(context.Context, int64, int64) error {
	r.deleteCalled = true
	return nil
}

type passkeyTestSessions struct{ PasskeySessionStore }

func (passkeyTestSessions) Store(context.Context, *PasskeySession, time.Duration) (string, error) {
	return "single-use-session", nil
}

func newEnabledPasskeyTestService(t *testing.T, user *User) (*PasskeyService, *passkeyTestRepo) {
	t.Helper()
	repo := &passkeyTestRepo{}
	svc, err := NewPasskeyService(&config.Config{WebAuthn: config.WebAuthnConfig{
		Enabled: true, RPDisplayName: "Sub2API", RPID: "sub2api.example.com",
		RPOrigins: []string{"https://sub2api.example.com"},
	}}, repo, passkeyTestSessions{}, &passkeyTestUserRepo{user: user})
	require.NoError(t, err)
	return svc, repo
}

func TestPasskeyEnrollmentAndDeletionRequirePassword(t *testing.T) {
	user := &User{ID: 7, Email: "user@example.com", Status: StatusActive}
	require.NoError(t, user.SetPassword("correct-password"))
	svc, repo := newEnabledPasskeyTestService(t, user)

	_, _, err := svc.BeginRegistration(context.Background(), user.ID, "")
	require.ErrorIs(t, err, ErrPasswordRequired)
	_, _, err = svc.BeginRegistration(context.Background(), user.ID, "wrong-password")
	require.ErrorIs(t, err, ErrPasswordIncorrect)
	require.False(t, repo.handleCalled)

	creation, token, err := svc.BeginRegistration(context.Background(), user.ID, "correct-password")
	require.NoError(t, err)
	require.NotNil(t, creation)
	require.Equal(t, "single-use-session", token)
	require.True(t, repo.handleCalled)

	require.ErrorIs(t, svc.Delete(context.Background(), user.ID, 1, ""), ErrPasswordRequired)
	require.ErrorIs(t, svc.Delete(context.Background(), user.ID, 1, "wrong-password"), ErrPasswordIncorrect)
	require.False(t, repo.deleteCalled)
	require.NoError(t, svc.Delete(context.Background(), user.ID, 1, "correct-password"))
	require.True(t, repo.deleteCalled)
}

func TestDisabledPasskeyServiceFailsBeforeRepositoryAccess(t *testing.T) {
	users := &passkeyTestUserRepo{}
	repo := &passkeyTestRepo{}
	svc, err := NewPasskeyService(&config.Config{}, repo, passkeyTestSessions{}, users)
	require.NoError(t, err)
	require.False(t, svc.Enabled())

	_, _, err = svc.BeginRegistration(context.Background(), 1, "password")
	require.ErrorIs(t, err, ErrPasskeysDisabled)
	_, _, err = svc.BeginLogin(context.Background())
	require.ErrorIs(t, err, ErrPasskeysDisabled)
	_, err = svc.List(context.Background(), 1)
	require.ErrorIs(t, err, ErrPasskeysDisabled)
	require.ErrorIs(t, svc.Delete(context.Background(), 1, 1, "password"), ErrPasskeysDisabled)
	require.Zero(t, users.calls)
	require.False(t, repo.handleCalled)
	require.False(t, repo.deleteCalled)
}
