package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeEmailForAliasDedup(t *testing.T) {
	tests := []struct {
		name  string
		email string
		want  string
	}{
		{name: "trim and lowercase", email: " User@Example.COM ", want: "user@example.com"},
		{name: "plus alias", email: "user+campaign@example.com", want: "user@example.com"},
		{name: "gmail dots", email: "u.s.e.r+tag@gmail.com", want: "user@gmail.com"},
		{name: "googlemail folds", email: "u.s.e.r@googlemail.com", want: "user@gmail.com"},
		{name: "fqdn root dot", email: "user@example.com.", want: "user@example.com"},
		{name: "non gmail dots stay significant", email: "first.last@qq.com", want: "first.last@qq.com"},
		{name: "leading plus is not collapsed", email: "+alice@gmail.com", want: "+alice@gmail.com"},
		{name: "malformed is stable", email: " NOT-AN-EMAIL ", want: "not-an-email"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, NormalizeEmailForAliasDedup(tt.email))
		})
	}
}

func TestEmailAliasDedupProbes(t *testing.T) {
	require.Equal(t, []EmailAliasProbe{
		{Local: "username", Domain: "gmailcom", DotInsensitive: true},
		{Local: "username", Domain: "googlemailcom", DotInsensitive: true},
	}, EmailAliasDedupProbes("user.name+tag@gmail.com."))
	require.Equal(t, []EmailAliasProbe{{Local: "first.last", Domain: "qq.com"}},
		EmailAliasDedupProbes("first.last+tag@qq.com"))
	require.Nil(t, EmailAliasDedupProbes("not-an-email"))
	require.Nil(t, EmailAliasDedupProbes("...@gmail.com"))
}
