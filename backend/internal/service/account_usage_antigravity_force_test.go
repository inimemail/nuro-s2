package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type quotaReaderFixture struct {
	calls atomic.Int32
	fetch func(context.Context, *Account) (*QuotaResult, error)
}

func (f *quotaReaderFixture) CanFetch(*Account) bool                       { return true }
func (f *quotaReaderFixture) GetProxyURL(context.Context, *Account) string { return "" }
func (f *quotaReaderFixture) FetchQuota(ctx context.Context, a *Account, _ string) (*QuotaResult, error) {
	f.calls.Add(1)
	return f.fetch(ctx, a)
}

func TestAntigravityForceBypassesFreshCacheAndKeepsCopies(t *testing.T) {
	account := &Account{ID: 11, Platform: PlatformAntigravity}
	reader := &quotaReaderFixture{fetch: func(ctx context.Context, _ *Account) (*QuotaResult, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), 30*time.Second)
		return &QuotaResult{UsageInfo: &UsageInfo{SubscriptionTierRaw: "new"}}, nil
	}}
	s := &AccountUsageService{cache: NewUsageCache(), antigravityQuotaFetcher: reader}
	s.cache.antigravityCache.Store(account.ID, &antigravityUsageCache{usageInfo: &UsageInfo{SubscriptionTierRaw: "old"}, timestamp: time.Now()})
	old, err := s.getAntigravityUsage(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "old", old.SubscriptionTierRaw)
	require.Zero(t, reader.calls.Load())
	fresh, err := s.getAntigravityUsage(context.Background(), account, true)
	require.NoError(t, err)
	require.Equal(t, "new", fresh.SubscriptionTierRaw)
	fresh.SubscriptionTierRaw = "caller mutation"
	got, err := s.getAntigravityUsage(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "new", got.SubscriptionTierRaw)
	require.EqualValues(t, 1, reader.calls.Load())
}

func TestAntigravityForceDoesNotAcceptCacheOnlyFlight(t *testing.T) {
	account := &Account{ID: 12, Platform: PlatformAntigravity}
	reader := &quotaReaderFixture{fetch: func(context.Context, *Account) (*QuotaResult, error) {
		return &QuotaResult{UsageInfo: &UsageInfo{SubscriptionTierRaw: "upstream"}}, nil
	}}
	s := &AccountUsageService{cache: NewUsageCache(), antigravityQuotaFetcher: reader}
	blocked := make(chan struct{})
	started := make(chan struct{})
	cachedFlight := s.cache.antigravityFlight.DoChan("ag-usage:12", func() (any, error) {
		close(started)
		<-blocked
		return antigravityUsageFlightResult{usage: &UsageInfo{SubscriptionTierRaw: "cached"}}, nil
	})
	<-started
	result := make(chan *UsageInfo, 1)
	errors := make(chan error, 1)
	go func() {
		usage, err := s.getAntigravityUsage(context.Background(), account, true)
		result <- usage
		errors <- err
	}()
	require.Eventually(t, func() bool {
		s.cache.antigravityForceMu.Lock()
		defer s.cache.antigravityForceMu.Unlock()
		return s.cache.antigravityForce[account.ID] == 1
	}, time.Second, time.Millisecond)
	close(blocked)
	<-cachedFlight
	require.NoError(t, <-errors)
	require.Equal(t, "upstream", (<-result).SubscriptionTierRaw)
	require.EqualValues(t, 1, reader.calls.Load())
}

func TestAntigravityCallerCancelDoesNotCancelSharedFetch(t *testing.T) {
	started, proceed := make(chan struct{}), make(chan struct{})
	reader := &quotaReaderFixture{fetch: func(ctx context.Context, _ *Account) (*QuotaResult, error) {
		close(started)
		select {
		case <-proceed:
			return &QuotaResult{UsageInfo: &UsageInfo{SubscriptionTierRaw: "shared"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	s := &AccountUsageService{cache: NewUsageCache(), antigravityQuotaFetcher: reader}
	account := &Account{ID: 13, Platform: PlatformAntigravity}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := s.getAntigravityUsage(ctx, account, true); result <- err }()
	<-started
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
	close(proceed)
	got, err := s.getAntigravityUsage(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "shared", got.SubscriptionTierRaw)
	require.EqualValues(t, 1, reader.calls.Load())
	_, err = s.getAntigravityUsage(ctx, account, true)
	require.True(t, errors.Is(err, context.Canceled))
	require.EqualValues(t, 1, reader.calls.Load())
}

func TestAntigravityConcurrentForceSharesFetchAndIsolatesAccounts(t *testing.T) {
	started, proceed := make(chan struct{}), make(chan struct{})
	reader := &quotaReaderFixture{fetch: func(ctx context.Context, account *Account) (*QuotaResult, error) {
		if account.ID == 14 {
			close(started)
			select {
			case <-proceed:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &QuotaResult{UsageInfo: &UsageInfo{SubscriptionTierRaw: account.Name}}, nil
	}}
	s := &AccountUsageService{cache: NewUsageCache(), antigravityQuotaFetcher: reader}
	account := &Account{ID: 14, Name: "shared", Platform: PlatformAntigravity}
	const callers = 8
	results := make(chan *UsageInfo, callers)
	errors := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			got, err := s.getAntigravityUsage(context.Background(), account, true)
			results <- got
			errors <- err
		}()
	}
	<-started
	require.Eventually(t, func() bool {
		s.cache.antigravityForceMu.Lock()
		defer s.cache.antigravityForceMu.Unlock()
		return s.cache.antigravityForce[account.ID] == callers
	}, time.Second, time.Millisecond)
	other, err := s.getAntigravityUsage(context.Background(), &Account{ID: 15, Name: "isolated", Platform: PlatformAntigravity}, true)
	require.NoError(t, err)
	require.Equal(t, "isolated", other.SubscriptionTierRaw)
	close(proceed)
	for i := 0; i < callers; i++ {
		require.NoError(t, <-errors)
		got := <-results
		require.NotNil(t, got)
		require.Equal(t, "shared", got.SubscriptionTierRaw)
	}
	require.EqualValues(t, 2, reader.calls.Load(), "one actual fetch for each account")
}

func TestAntigravityForceReusesFreshPublicationAfterCacheOnlyFlight(t *testing.T) {
	account := &Account{ID: 16, Platform: PlatformAntigravity}
	reader := &quotaReaderFixture{fetch: func(context.Context, *Account) (*QuotaResult, error) {
		return nil, errors.New("a newer fetch has already completed")
	}}
	s := &AccountUsageService{cache: NewUsageCache(), antigravityQuotaFetcher: reader}
	started, proceed := make(chan struct{}), make(chan struct{})
	stale := &UsageInfo{SubscriptionTierRaw: "stale"}
	flight := s.cache.antigravityFlight.DoChan("ag-usage:16", func() (any, error) {
		close(started)
		<-proceed
		// Simulate a stale result delivery racing a newer completed publication.
		s.cache.antigravityCache.Store(account.ID, &antigravityUsageCache{usageInfo: &UsageInfo{SubscriptionTierRaw: "fresh"}, timestamp: time.Now()})
		return antigravityUsageFlightResult{usage: stale}, nil
	})
	<-started
	result := make(chan *UsageInfo, 1)
	errs := make(chan error, 1)
	go func() {
		got, err := s.getAntigravityUsage(context.Background(), account, true)
		errs <- err
		result <- got
	}()
	require.Eventually(t, func() bool {
		s.cache.antigravityForceMu.Lock()
		defer s.cache.antigravityForceMu.Unlock()
		return s.cache.antigravityForce[account.ID] == 1
	}, time.Second, time.Millisecond)
	close(proceed)
	<-flight
	require.NoError(t, <-errs)
	got := <-result
	require.NotNil(t, got)
	require.Equal(t, "fresh", got.SubscriptionTierRaw)
	require.Zero(t, reader.calls.Load(), "do not duplicate a fetch completed during this request")
}
