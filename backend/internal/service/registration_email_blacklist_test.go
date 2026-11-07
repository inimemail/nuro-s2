package service

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestRegistrationBlacklistCanonicalExactOnly(t *testing.T) {
	entries, err := NormalizeRegistrationEmailBlacklist([]string{" A.B+tag@googlemail.com ", "ab@gmail.com", "u.ser+tag@example.com"})
	require.NoError(t, err)
	require.Equal(t, []string{"ab@gmail.com", "u.ser@example.com"}, entries)
	s := &SettingService{}
	s.publishRegistrationBlacklist(entries)
	for _, email := range []string{"a.b+other@gmail.com", "ab@googlemail.com", "u.ser+other@example.com"} {
		require.Error(t, s.CheckRegistrationEmailBlacklist(t.Context(), email), email)
	}
	for _, email := range []string{"other@gmail.com", "ab@example.com", "user@example.com", "other@evilgmail.com"} {
		require.NoError(t, s.CheckRegistrationEmailBlacklist(t.Context(), email), email)
	}
	for _, bad := range []string{"*example.com", "bad", "Name <x@example.com>", "a@example.com\nb@example.com"} {
		_, err := NormalizeRegistrationEmailBlacklist([]string{bad})
		require.Error(t, err)
	}
	_, err = NormalizeRegistrationEmailBlacklist(strings.Split(strings.Repeat("x,", 20000), ","))
	require.Error(t, err)
	s.publishRegistrationBlacklist(nil)
	require.NoError(t, s.CheckRegistrationEmailBlacklist(t.Context(), "ab@gmail.com"))
}
