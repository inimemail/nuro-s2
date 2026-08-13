package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type registrationQuotaUserRepo struct {
	UserRepository
	domainCount  int
	countDomain  string
	createDomain string
	aliasCreates int
}

func (r *registrationQuotaUserRepo) CountUsersByEmailDomain(_ context.Context, domain string) (int, error) {
	r.countDomain = domain
	return r.domainCount, nil
}

func (r *registrationQuotaUserRepo) CreateWithEmailAliasGuardAndDomainLimit(_ context.Context, _ *User, domain string) error {
	r.createDomain = domain
	return nil
}

func (r *registrationQuotaUserRepo) CreateWithEmailAliasGuard(_ context.Context, _ *User) error {
	r.aliasCreates++
	return nil
}

func (r *registrationQuotaUserRepo) ExistsByEmailAlias(context.Context, string) (bool, error) {
	return false, nil
}

func newRegistrationQuotaAuthService(values map[string]string, repo UserRepository) *AuthService {
	settings := NewSettingService(&allowClaudeCodeSettingRepoStub{values: values}, &config.Config{})
	return &AuthService{settingService: settings, userRepo: repo}
}

func TestRegistrationEmailDomainQuota_EnabledWithEmptyWhitelist(t *testing.T) {
	repo := &registrationQuotaUserRepo{domainCount: 1}
	svc := newRegistrationQuotaAuthService(map[string]string{
		SettingKeyRegistrationEmailDomainQuotaEnabled: "true",
	}, repo)

	require.NoError(t, svc.ValidateRegistrationEmailHandlerPolicy(context.Background(), "new@sub.example.com"))
	require.ErrorIs(t, svc.validateRegistrationEmailQuota(context.Background(), "new@sub.example.com"), ErrEmailDomainRegistrationLimit)
	require.Equal(t, "example.com", repo.countDomain)

	repo.domainCount = 0
	require.NoError(t, svc.createUserWithRegistrationEmailGuard(context.Background(), &User{Email: "new@sub.example.com"}))
	require.Equal(t, "example.com", repo.createDomain)
	require.Zero(t, repo.aliasCreates)
}

func TestRegistrationEmailDomainQuota_WhitelistRemainsExempt(t *testing.T) {
	repo := &registrationQuotaUserRepo{domainCount: 1}
	svc := newRegistrationQuotaAuthService(map[string]string{
		SettingKeyRegistrationEmailDomainQuotaEnabled: "true",
		SettingKeyRegistrationEmailSuffixWhitelist:    `["@trusted.example"]`,
	}, repo)

	require.NoError(t, svc.validateRegistrationEmailQuota(context.Background(), "new@trusted.example"))
	require.NoError(t, svc.createUserWithRegistrationEmailGuard(context.Background(), &User{Email: "new@trusted.example"}))
	require.Empty(t, repo.countDomain)
	require.Empty(t, repo.createDomain)
	require.Equal(t, 1, repo.aliasCreates)
}

func TestRegistrationEmailDomainQuota_DisabledPreservesEmptyWhitelistAllowAll(t *testing.T) {
	repo := &registrationQuotaUserRepo{domainCount: 1}
	svc := newRegistrationQuotaAuthService(nil, repo)

	require.NoError(t, svc.ValidateRegistrationEmailHandlerPolicy(context.Background(), "new@example.com"))
	require.NoError(t, svc.validateRegistrationEmailQuota(context.Background(), "new@example.com"))
	require.NoError(t, svc.createUserWithRegistrationEmailGuard(context.Background(), &User{Email: "new@example.com"}))
	require.Empty(t, repo.countDomain)
	require.Empty(t, repo.createDomain)
	require.Equal(t, 1, repo.aliasCreates)
}
