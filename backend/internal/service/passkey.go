package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	passkeySessionRegistration = "registration"
	passkeySessionLogin        = "login"
	passkeySessionTTL          = 5 * time.Minute
	defaultPasskeyName         = "Passkey"
	maxPasskeyNameLength       = 100
)

var (
	ErrPasskeysDisabled = infraerrors.Forbidden("PASSKEY_DISABLED", "passkey authentication is not enabled")
	ErrPasskeyNotFound  = infraerrors.NotFound("PASSKEY_NOT_FOUND", "passkey not found")
	ErrPasskeyExists    = infraerrors.Conflict("PASSKEY_ALREADY_EXISTS", "this passkey is already registered")
	ErrPasskeySession   = infraerrors.BadRequest("PASSKEY_SESSION_INVALID", "passkey session is invalid or expired")
	ErrPasskeyVerify    = infraerrors.Unauthorized("PASSKEY_VERIFICATION_FAILED", "passkey verification failed")
)

type PasskeyCredentialRecord struct {
	ID         int64
	UserID     int64
	UserHandle []byte
	Name       string
	Credential webauthn.Credential
	LastUsedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type PasskeyRepository interface {
	EnsureUserHandle(context.Context, int64, []byte) ([]byte, error)
	GetUserHandle(context.Context, int64) ([]byte, error)
	GetByCredentialID(context.Context, []byte) (*PasskeyCredentialRecord, error)
	ListByUserID(context.Context, int64) ([]PasskeyCredentialRecord, error)
	Create(context.Context, *PasskeyCredentialRecord) (*PasskeyCredentialRecord, error)
	UpdateCredential(context.Context, int64, *webauthn.Credential, time.Time) error
	Rename(context.Context, int64, int64, string) error
	Delete(context.Context, int64, int64) error
}

type PasskeySession struct {
	Kind     string               `json:"kind"`
	UserID   int64                `json:"user_id,omitempty"`
	WebAuthn webauthn.SessionData `json:"webauthn"`
}

type PasskeySessionStore interface {
	Store(context.Context, *PasskeySession, time.Duration) (string, error)
	Consume(context.Context, string) (*PasskeySession, error)
}

type PasskeyCredentialSummary struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Backup     bool       `json:"backup"`
}

type passkeyUser struct {
	account     *User
	handle      []byte
	credentials []webauthn.Credential
}

func (u *passkeyUser) WebAuthnID() []byte                         { return u.handle }
func (u *passkeyUser) WebAuthnName() string                       { return u.account.Email }
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }
func (u *passkeyUser) WebAuthnDisplayName() string {
	if name := strings.TrimSpace(u.account.Username); name != "" {
		return name
	}
	return u.account.Email
}

type PasskeyService struct {
	enabled  bool
	webAuthn *webauthn.WebAuthn
	repo     PasskeyRepository
	sessions PasskeySessionStore
	userRepo UserRepository
}

func NewPasskeyService(cfg *config.Config, repo PasskeyRepository, sessions PasskeySessionStore, userRepo UserRepository) (*PasskeyService, error) {
	s := &PasskeyService{repo: repo, sessions: sessions, userRepo: userRepo}
	if cfg == nil || !cfg.WebAuthn.Enabled {
		return s, nil
	}
	instance, err := webauthn.New(&webauthn.Config{
		RPDisplayName: cfg.WebAuthn.RPDisplayName,
		RPID:          cfg.WebAuthn.RPID,
		RPOrigins:     append([]string(nil), cfg.WebAuthn.RPOrigins...),
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize WebAuthn: %w", err)
	}
	s.enabled, s.webAuthn = true, instance
	return s, nil
}

func (s *PasskeyService) Enabled() bool { return s != nil && s.enabled && s.webAuthn != nil }
func (s *PasskeyService) requireEnabled() error {
	if !s.Enabled() {
		return ErrPasskeysDisabled
	}
	return nil
}

func verifyPasskeyPassword(user *User, password string) error {
	if password == "" {
		return ErrPasswordRequired
	}
	if user == nil || !user.CheckPassword(password) {
		return ErrPasswordIncorrect
	}
	return nil
}

func (s *PasskeyService) BeginRegistration(ctx context.Context, userID int64, password string) (*protocol.CredentialCreation, string, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, "", err
	}
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	if !user.IsActive() {
		return nil, "", ErrUserNotActive
	}
	if err = verifyPasskeyPassword(user, password); err != nil {
		return nil, "", err
	}
	candidate := make([]byte, 32)
	if _, err = rand.Read(candidate); err != nil {
		return nil, "", fmt.Errorf("generate passkey user handle: %w", err)
	}
	handle, err := s.repo.EnsureUserHandle(ctx, userID, candidate)
	if err != nil {
		return nil, "", err
	}
	waUser, err := s.loadWebAuthnUser(ctx, user, handle)
	if err != nil {
		return nil, "", err
	}
	creation, session, err := s.webAuthn.BeginRegistration(
		waUser,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(webauthn.Credentials(waUser.credentials).CredentialDescriptors()),
		webauthn.WithExtensions(protocol.AuthenticationExtensions{"credProps": true}),
	)
	if err != nil {
		return nil, "", fmt.Errorf("begin passkey registration: %w", err)
	}
	token, err := s.sessions.Store(ctx, &PasskeySession{Kind: passkeySessionRegistration, UserID: userID, WebAuthn: *session}, passkeySessionTTL)
	return creation, token, err
}

func (s *PasskeyService) FinishRegistration(ctx context.Context, userID int64, sessionToken, name string, request *http.Request) (*PasskeyCredentialSummary, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, err
	}
	session, err := s.sessions.Consume(ctx, sessionToken)
	if err != nil {
		return nil, err
	}
	if session == nil || session.Kind != passkeySessionRegistration || session.UserID != userID {
		return nil, ErrPasskeySession
	}
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !user.IsActive() {
		return nil, ErrUserNotActive
	}
	handle, err := s.repo.GetUserHandle(ctx, userID)
	if err != nil {
		return nil, err
	}
	waUser, err := s.loadWebAuthnUser(ctx, user, handle)
	if err != nil {
		return nil, err
	}
	credential, err := s.webAuthn.FinishRegistration(waUser, session.WebAuthn, request)
	if err != nil {
		return nil, ErrPasskeyVerify
	}
	record, err := s.repo.Create(ctx, &PasskeyCredentialRecord{UserID: userID, UserHandle: handle, Name: normalizePasskeyName(name), Credential: *credential})
	if err != nil {
		return nil, err
	}
	return passkeySummary(record), nil
}

func (s *PasskeyService) BeginLogin(ctx context.Context) (*protocol.CredentialAssertion, string, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, "", err
	}
	assertion, session, err := s.webAuthn.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, "", fmt.Errorf("begin passkey login: %w", err)
	}
	token, err := s.sessions.Store(ctx, &PasskeySession{Kind: passkeySessionLogin, WebAuthn: *session}, passkeySessionTTL)
	return assertion, token, err
}

func (s *PasskeyService) FinishLogin(ctx context.Context, sessionToken string, request *http.Request) (*User, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, err
	}
	session, err := s.sessions.Consume(ctx, sessionToken)
	if err != nil {
		return nil, err
	}
	if session == nil || session.Kind != passkeySessionLogin {
		return nil, ErrPasskeySession
	}
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		record, lookupErr := s.repo.GetByCredentialID(ctx, rawID)
		if lookupErr != nil || record == nil || !bytes.Equal(record.UserHandle, userHandle) {
			return nil, ErrPasskeyVerify
		}
		account, lookupErr := s.userRepo.GetByID(ctx, record.UserID)
		if lookupErr != nil || account == nil || !account.IsActive() {
			return nil, ErrPasskeyVerify
		}
		return s.loadWebAuthnUser(ctx, account, record.UserHandle)
	}
	validated, credential, err := s.webAuthn.FinishPasskeyLogin(handler, session.WebAuthn, request)
	if err != nil {
		return nil, ErrPasskeyVerify
	}
	waUser, ok := validated.(*passkeyUser)
	if !ok || waUser.account == nil {
		return nil, ErrPasskeyVerify
	}
	if err = s.repo.UpdateCredential(ctx, waUser.account.ID, credential, time.Now().UTC()); err != nil {
		return nil, err
	}
	return waUser.account, nil
}

func (s *PasskeyService) List(ctx context.Context, userID int64) ([]PasskeyCredentialSummary, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, err
	}
	records, err := s.repo.ListByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	result := make([]PasskeyCredentialSummary, 0, len(records))
	for i := range records {
		result = append(result, *passkeySummary(&records[i]))
	}
	return result, nil
}

func (s *PasskeyService) Rename(ctx context.Context, userID, credentialID int64, name string) error {
	if err := s.requireEnabled(); err != nil {
		return err
	}
	return s.repo.Rename(ctx, userID, credentialID, normalizePasskeyName(name))
}

func (s *PasskeyService) Delete(ctx context.Context, userID, credentialID int64, password string) error {
	if err := s.requireEnabled(); err != nil {
		return err
	}
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if err = verifyPasskeyPassword(user, password); err != nil {
		return err
	}
	return s.repo.Delete(ctx, userID, credentialID)
}

func (s *PasskeyService) loadWebAuthnUser(ctx context.Context, user *User, handle []byte) (*passkeyUser, error) {
	records, err := s.repo.ListByUserID(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	credentials := make([]webauthn.Credential, 0, len(records))
	for i := range records {
		credentials = append(credentials, records[i].Credential)
	}
	return &passkeyUser{account: user, handle: handle, credentials: credentials}, nil
}

func normalizePasskeyName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return defaultPasskeyName
	}
	runes := []rune(name)
	if len(runes) > maxPasskeyNameLength {
		return string(runes[:maxPasskeyNameLength])
	}
	return name
}

func passkeySummary(record *PasskeyCredentialRecord) *PasskeyCredentialSummary {
	return &PasskeyCredentialSummary{ID: record.ID, Name: record.Name, CreatedAt: record.CreatedAt, LastUsedAt: record.LastUsedAt, Backup: record.Credential.Flags.BackupState}
}

var _ webauthn.User = (*passkeyUser)(nil)
