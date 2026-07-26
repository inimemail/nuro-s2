package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestGatewayCacheLiveControllerOwnershipAndCloseAreFenced(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	store := NewGatewayCache(client).(service.LiveCallStore)
	record := &service.LiveCallRecord{
		CallID:                "call-secret",
		CallHash:              "hashed-call-id",
		AccountID:             11,
		APIKeyID:              22,
		UserID:                33,
		GroupID:               44,
		LeaseID:               "lease-1",
		Model:                 "gpt-live",
		CreatedAt:             time.Now(),
		ExpiresAt:             time.Now().Add(time.Hour),
		Controller:            service.LiveControllerPending,
		AttestationCiphertext: "encrypted-only",
	}
	require.NoError(t, store.SaveLiveCall(context.Background(), record, time.Hour))

	loaded, err := store.GetLiveCall(context.Background(), record.CallHash)
	require.NoError(t, err)
	require.Equal(t, record.CallID, loaded.CallID)
	require.Equal(t, "encrypted-only", loaded.AttestationCiphertext)

	claimed, err := store.ClaimLiveController(context.Background(), record.CallHash, service.LiveControllerObserver, "observer-1")
	require.NoError(t, err)
	require.True(t, claimed)
	released, err := store.ReleaseLiveController(context.Background(), record.CallHash, "observer-1")
	require.NoError(t, err)
	require.True(t, released)
	claimed, err = store.ClaimLiveController(context.Background(), record.CallHash, service.LiveControllerObserver, "observer-2")
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = store.ClaimLiveController(context.Background(), record.CallHash, service.LiveControllerProxy, "proxy-1")
	require.NoError(t, err)
	require.True(t, claimed)

	released, err = store.ReleaseLiveController(context.Background(), record.CallHash, "wrong-owner")
	require.NoError(t, err)
	require.False(t, released)
	released, err = store.ReleaseLiveController(context.Background(), record.CallHash, "proxy-1")
	require.NoError(t, err)
	require.True(t, released)

	closed, err := store.MarkLiveCallClosed(context.Background(), record.CallHash, time.Hour)
	require.NoError(t, err)
	require.True(t, closed)
	closed, err = store.MarkLiveCallClosed(context.Background(), record.CallHash, time.Hour)
	require.NoError(t, err)
	require.False(t, closed)
	claimed, err = store.ClaimLiveController(context.Background(), record.CallHash, service.LiveControllerProxy, "proxy-2")
	require.NoError(t, err)
	require.False(t, claimed)
}
