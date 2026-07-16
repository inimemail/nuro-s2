package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSessionBindingHashIsStableAndSensitiveToBothFields(t *testing.T) {
	base := (&SessionBinding{IP: "203.0.113.10", UserAgent: "client/1"}).Hash()
	require.NotEmpty(t, base)
	require.Equal(t, base, (&SessionBinding{IP: " 203.0.113.10 ", UserAgent: "client/1"}).Hash())
	require.NotEqual(t, base, (&SessionBinding{IP: "203.0.113.11", UserAgent: "client/1"}).Hash())
	require.NotEqual(t, base, (&SessionBinding{IP: "203.0.113.10", UserAgent: "client/2"}).Hash())
}

func TestGenerateTokenWithContextCarriesSessionBinding(t *testing.T) {
	svc := &AuthService{cfg: &config.Config{JWT: config.JWTConfig{Secret: "test-secret", ExpireHour: 1}}}
	user := &User{ID: 7, Email: "user@example.com", Role: RoleUser}
	ctx := WithSessionBinding(context.Background(), &SessionBinding{IP: "203.0.113.10", UserAgent: "client/1"})

	token, err := svc.GenerateTokenWithContext(ctx, user)
	require.NoError(t, err)
	claims, err := svc.ValidateToken(token)
	require.NoError(t, err)
	require.NotEmpty(t, claims.SessionID)
	require.Equal(t, SessionBindingFromContext(ctx).Hash(), claims.BindingHash)

	legacy, err := svc.GenerateToken(user)
	require.NoError(t, err)
	legacyClaims, err := svc.ValidateToken(legacy)
	require.NoError(t, err)
	require.Empty(t, legacyClaims.BindingHash)
}
