package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestPasskeySessionStoreConsumesTokenExactlyOnce(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewPasskeySessionStore(client)

	token, err := store.Store(context.Background(), &service.PasskeySession{Kind: "login", UserID: 7}, time.Minute)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	got, err := store.Consume(context.Background(), token)
	require.NoError(t, err)
	require.Equal(t, "login", got.Kind)
	require.Equal(t, int64(7), got.UserID)

	_, err = store.Consume(context.Background(), token)
	require.ErrorIs(t, err, service.ErrPasskeySession)
}

func TestPasskeySessionStoreRejectsInvalidTokensWithoutRedisLookup(t *testing.T) {
	store := NewPasskeySessionStore(nil)
	for _, token := range []string{"", "   ", strings.Repeat("x", 129)} {
		_, err := store.Consume(context.Background(), token)
		require.ErrorIs(t, err, service.ErrPasskeySession)
	}
}
