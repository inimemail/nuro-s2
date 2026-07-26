package service

import (
	"context"
	"strings"
)

var gmailFamilyDomains = map[string]struct{}{
	"gmail.com":      {},
	"googlemail.com": {},
}

// EmailAliasProbe is evaluated against a dot-stripped email expression. Final
// matches are always rechecked with NormalizeEmailForAliasDedup.
type EmailAliasProbe struct {
	Local          string
	Domain         string
	DotInsensitive bool
}

// NormalizeEmailForAliasDedup returns the inbox identity used only for signup
// deduplication. Stored, displayed, login, and delivery addresses are unchanged.
func NormalizeEmailForAliasDedup(email string) string {
	local, domain, ok := splitEmailForAliasDedup(email)
	if !ok {
		return strings.ToLower(strings.TrimSpace(email))
	}
	local = stripEmailPlusSuffix(local)
	if isGmailFamilyDomain(domain) {
		local = stripEmailLocalDots(local)
		domain = "gmail.com"
	}
	return local + "@" + domain
}

func EmailAliasDedupProbes(email string) []EmailAliasProbe {
	local, domain, ok := splitEmailForAliasDedup(email)
	if !ok {
		return nil
	}
	baseLocal := stripEmailPlusSuffix(local)
	if strings.ReplaceAll(baseLocal, ".", "") == "" {
		return nil
	}
	domains := []string{domain}
	if isGmailFamilyDomain(domain) {
		domains = []string{"gmail.com", "googlemail.com"}
	}
	probes := make([]EmailAliasProbe, 0, len(domains))
	for _, candidate := range domains {
		dotInsensitive := isGmailFamilyDomain(candidate)
		probeLocal := baseLocal
		probeDomain := candidate
		if dotInsensitive {
			probeLocal = strings.ReplaceAll(baseLocal, ".", "")
			probeDomain = strings.ReplaceAll(candidate, ".", "")
		}
		probes = append(probes, EmailAliasProbe{
			Local:          probeLocal,
			Domain:         probeDomain,
			DotInsensitive: dotInsensitive,
		})
	}
	return probes
}

func splitEmailForAliasDedup(email string) (local, domain string, ok bool) {
	local, domain, ok = splitEmailForPolicy(email)
	if !ok {
		return "", "", false
	}
	domain = strings.TrimRight(domain, ".")
	if domain == "" {
		return "", "", false
	}
	return local, domain, true
}

func stripEmailPlusSuffix(local string) string {
	if idx := strings.IndexByte(local, '+'); idx > 0 {
		return local[:idx]
	}
	return local
}

func stripEmailLocalDots(local string) string {
	if stripped := strings.ReplaceAll(local, ".", ""); stripped != "" {
		return stripped
	}
	return local
}

func isGmailFamilyDomain(domain string) bool {
	_, ok := gmailFamilyDomains[domain]
	return ok
}

type emailAliasGuardUserRepository interface {
	ExistsByEmailAlias(ctx context.Context, email string) (bool, error)
	CreateWithEmailAliasGuard(ctx context.Context, user *User) error
}

func (s *AuthService) existsByEmailOrAlias(ctx context.Context, email string) (bool, error) {
	exists, err := s.userRepo.ExistsByEmail(ctx, email)
	if err != nil || exists {
		return exists, err
	}
	repo, ok := s.userRepo.(emailAliasGuardUserRepository)
	if !ok {
		return false, nil
	}
	return repo.ExistsByEmailAlias(ctx, email)
}

func (s *AuthService) createUserWithEmailAliasGuard(ctx context.Context, user *User) error {
	if repo, ok := s.userRepo.(emailAliasGuardUserRepository); ok {
		return repo.CreateWithEmailAliasGuard(ctx, user)
	}
	return s.userRepo.Create(ctx, user)
}
