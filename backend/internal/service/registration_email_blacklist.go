package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const SettingKeyRegistrationEmailBlacklist = "registration_email_blacklist"
const MaxRegistrationEmailBlacklist = 20000

type registrationBlacklistSnapshot struct{ entries map[string]struct{} }

func NormalizeRegistrationEmailBlacklist(entries []string) ([]string, error) {
	if len(entries) > MaxRegistrationEmailBlacklist {
		return nil, fmt.Errorf("at most %d email addresses are allowed", MaxRegistrationEmailBlacklist)
	}
	out := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for i, entry := range entries {
		e := strings.ToLower(strings.TrimSpace(entry))
		parsed, err := mail.ParseAddress(e)
		if err != nil || parsed.Address != e || len(e) > 254 || strings.ContainsAny(e, "\r\n") || !strings.Contains(e, "@") {
			return nil, fmt.Errorf("invalid email address at line %d", i+1)
		}
		key := NormalizeEmailForAliasDedup(e)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}

func decodeRegistrationBlacklist(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{}, nil
	}
	var entries []string
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, err
	}
	return NormalizeRegistrationEmailBlacklist(entries)
}
func parseRegistrationBlacklist(raw string) []string {
	entries, _ := decodeRegistrationBlacklist(raw)
	return entries
}
func RegistrationBlacklistRevision(entries []string) string {
	raw, _ := json.Marshal(entries)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (s *SettingService) publishRegistrationBlacklist(entries []string) {
	snapshot := &registrationBlacklistSnapshot{entries: make(map[string]struct{}, len(entries))}
	for _, e := range entries {
		snapshot.entries[e] = struct{}{}
	}
	s.registrationBlacklist.Store(snapshot)
}
func (s *SettingService) RefreshRegistrationBlacklist(ctx context.Context) error {
	if s == nil || s.settingRepo == nil {
		return nil
	}
	_, err, _ := s.registrationBlacklistFlight.Do("refresh", func() (any, error) {
		prior := s.registrationBlacklist.Load()
		raw, err := s.settingRepo.GetValue(ctx, SettingKeyRegistrationEmailBlacklist)
		if err != nil && !errors.Is(err, ErrSettingNotFound) {
			return nil, err
		}
		entries, err := decodeRegistrationBlacklist(raw)
		if err != nil {
			return nil, err
		}
		snapshot := &registrationBlacklistSnapshot{entries: make(map[string]struct{}, len(entries))}
		for _, e := range entries {
			snapshot.entries[e] = struct{}{}
		}
		// A concurrent administrator save wins over an older refresh.
		s.registrationBlacklist.CompareAndSwap(prior, snapshot)
		return nil, nil
	})
	return err
}
func (s *SettingService) CheckRegistrationEmailBlacklist(ctx context.Context, email string) error {
	if s == nil {
		return nil
	}
	snapshot := s.registrationBlacklist.Load()
	if snapshot == nil {
		if err := s.RefreshRegistrationBlacklist(ctx); err != nil {
			return ErrServiceUnavailable
		}
		snapshot = s.registrationBlacklist.Load()
	}
	if snapshot == nil || len(snapshot.entries) == 0 {
		return nil
	}
	if _, blocked := snapshot.entries[NormalizeEmailForAliasDedup(email)]; blocked {
		return infraerrors.Forbidden("REGISTRATION_EMAIL_BLOCKED", "Registration is not available for this email address")
	}
	return nil
}
