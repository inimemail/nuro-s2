package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type atomicRefreshFixture struct {
	RefreshTokenCache
	mu         sync.Mutex
	tokens     map[string]*RefreshTokenData
	consumeErr error
	stores     atomic.Int32
}

func (f *atomicRefreshFixture) GetRefreshToken(_ context.Context, key string) (*RefreshTokenData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.tokens[key]
	if v == nil {
		return nil, ErrRefreshTokenNotFound
	}
	copy := *v
	return &copy, nil
}
func (f *atomicRefreshFixture) ConsumeRefreshToken(ctx context.Context, key string) (*RefreshTokenData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.consumeErr != nil {
		return nil, f.consumeErr
	}
	v := f.tokens[key]
	if v == nil {
		return nil, ErrRefreshTokenNotFound
	}
	delete(f.tokens, key)
	copy := *v
	return &copy, nil
}
func (f *atomicRefreshFixture) StoreRefreshToken(_ context.Context, key string, data *RefreshTokenData, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	copy := *data
	f.tokens[key] = &copy
	f.stores.Add(1)
	return nil
}
func (f *atomicRefreshFixture) AddToUserTokenSet(context.Context, int64, string, time.Duration) error {
	return nil
}
func (f *atomicRefreshFixture) AddToFamilyTokenSet(context.Context, string, string, time.Duration) error {
	return nil
}
func (f *atomicRefreshFixture) DeleteRefreshToken(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tokens, key)
	return nil
}
func (f *atomicRefreshFixture) DeleteTokenFamily(_ context.Context, family string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range f.tokens {
		if v.FamilyID == family {
			delete(f.tokens, k)
		}
	}
	return nil
}

type refreshUserFixture struct {
	UserRepository
	user User
}

func (f *refreshUserFixture) GetByID(context.Context, int64) (*User, error) {
	copy := f.user
	return &copy, nil
}

func TestRefreshTokenPairConcurrentConsumptionAndFailClosed(t *testing.T) {
	token := refreshTokenPrefix + "fixture"
	for _, tc := range []struct {
		name, status string
		version      int64
		expired      bool
		consumeErr   error
		want         error
	}{
		{name: "concurrent", status: StatusActive, version: 3},
		{name: "cache unavailable", status: StatusActive, version: 3, consumeErr: errors.New("redis unavailable"), want: ErrServiceUnavailable},
		{name: "already consumed", status: StatusActive, version: 3, consumeErr: ErrRefreshTokenNotFound, want: ErrRefreshTokenInvalid},
		{name: "disabled", status: StatusDisabled, version: 3, want: ErrUserNotActive},
		{name: "password changed", status: StatusActive, version: 4, want: ErrTokenRevoked},
		{name: "expired", status: StatusActive, version: 3, expired: true, want: ErrRefreshTokenExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expires := time.Now().Add(time.Hour)
			if tc.expired {
				expires = time.Now().Add(-time.Hour)
			}
			cache := &atomicRefreshFixture{tokens: map[string]*RefreshTokenData{hashToken(token): {UserID: 7, TokenVersion: 3, FamilyID: "family", ExpiresAt: expires}}, consumeErr: tc.consumeErr}
			svc := &AuthService{cfg: &config.Config{JWT: config.JWTConfig{Secret: "refresh-rotation-fixture", ExpireHour: 1, AccessTokenExpireMinutes: 60, RefreshTokenExpireDays: 7}}, userRepo: &refreshUserFixture{user: User{ID: 7, Role: RoleUser, Status: tc.status, TokenVersion: tc.version, TokenVersionResolved: true}}, refreshTokenCache: cache}
			if tc.want != nil {
				pair, err := svc.RefreshTokenPair(context.Background(), token)
				require.ErrorIs(t, err, tc.want)
				require.Nil(t, pair)
				require.Zero(t, cache.stores.Load())
				return
			}
			var wg sync.WaitGroup
			var successes atomic.Int32
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					pair, err := svc.RefreshTokenPair(context.Background(), token)
					if err == nil {
						if pair == nil {
							t.Error("nil pair")
						}
						successes.Add(1)
					} else if !errors.Is(err, ErrRefreshTokenInvalid) {
						t.Errorf("unexpected error: %v", err)
					}
				}()
			}
			wg.Wait()
			require.EqualValues(t, 1, successes.Load())
			require.EqualValues(t, 1, cache.stores.Load())
		})
	}
}
