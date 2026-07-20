package service

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type openAIFirstTokenPlaceholderGuardReadHook struct {
	getCalls atomic.Int64
	delay    time.Duration
}

func (h *openAIFirstTokenPlaceholderGuardReadHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (h *openAIFirstTokenPlaceholderGuardReadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" {
			h.getCalls.Add(1)
			time.Sleep(h.delay)
		}
		return next(ctx, cmd)
	}
}

func (h *openAIFirstTokenPlaceholderGuardReadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		return next(ctx, cmds)
	}
}

func newOpenAIFirstTokenPlaceholderGuardTestService(t *testing.T, address string) *OpenAIGatewayService {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: address})
	t.Cleanup(func() { _ = client.Close() })
	svc := &OpenAIGatewayService{}
	svc.SetOpenAIFirstTokenTimeoutPlaceholderGuardRedisClient(client)
	return svc
}

func openAIFirstTokenPlaceholderGuardTestAccount() *Account {
	return &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra: map[string]any{
			openAIAPIKeyFirstTokenTimeoutPlaceholderEnabledExtraKey:      true,
			openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey:           100,
			openAIAPIKeyFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey:   3000,
			openAIAPIKeyFirstTokenTimeoutPlaceholderGuardEnabledExtraKey: true,
		},
	}
}

func TestOpenAIStreamFirstTokenTimeoutPlaceholderGuardRecoversAfterOneFastSampleAcrossInstances(t *testing.T) {
	redisServer := miniredis.RunT(t)
	first := newOpenAIFirstTokenPlaceholderGuardTestService(t, redisServer.Addr())
	second := newOpenAIFirstTokenPlaceholderGuardTestService(t, redisServer.Addr())
	account := openAIFirstTokenPlaceholderGuardTestAccount()

	require.Equal(t, 100, first.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4"))
	first.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, "gpt-5.4", 20000)
	require.Zero(t, second.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4"))

	second.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, "gpt-5.4", 800)
	require.Equal(t, 100, first.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4"))
}

func TestOpenAIStreamFirstTokenTimeoutPlaceholderGuardRetriesLatestFailedSample(t *testing.T) {
	redisServer := miniredis.RunT(t)
	svc := newOpenAIFirstTokenPlaceholderGuardTestService(t, redisServer.Addr())
	account := openAIFirstTokenPlaceholderGuardTestAccount()

	svc.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, "gpt-5.4", 20000)
	require.Zero(t, svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4"))

	redisServer.SetError("LOADING Redis is loading the dataset in memory")
	svc.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, "gpt-5.4", 20000)
	svc.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, "gpt-5.4", 800)
	redisServer.SetError("")

	require.Eventually(t, func() bool {
		return svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4") == 100
	}, 3*time.Second, 20*time.Millisecond)
}

func TestOpenAIStreamFirstTokenTimeoutPlaceholderGuardRecoversPersistentRetryAcrossInstances(t *testing.T) {
	redisServer := miniredis.RunT(t)
	first := newOpenAIFirstTokenPlaceholderGuardTestService(t, redisServer.Addr())
	second := newOpenAIFirstTokenPlaceholderGuardTestService(t, redisServer.Addr())
	account := openAIFirstTokenPlaceholderGuardTestAccount()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	first.openaiFirstTokenTimeoutPlaceholderGuard.retryOutbox = &openAIFirstTokenTimeoutPlaceholderGuardOutbox{db: db}
	second.openaiFirstTokenTimeoutPlaceholderGuard.retryOutbox = &openAIFirstTokenTimeoutPlaceholderGuardOutbox{db: db}

	first.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, "gpt-5.4", 20000, 100)
	require.Zero(t, second.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4"))

	mock.ExpectExec("INSERT INTO openai_first_token_guard_outbox").
		WithArgs(sqlmock.AnyArg(), openAIFirstTokenTimeoutPlaceholderGuardKey(1, "gpt-5.4"), 800, int64(200)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	redisServer.SetError("LOADING Redis is loading the dataset in memory")
	first.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, "gpt-5.4", 800, 200)
	redisServer.SetError("")

	mock.ExpectQuery("SELECT guard_key, real_token_ms, recorded_at_ns").
		WithArgs(
			openAIFirstTokenTimeoutPlaceholderGuardRetryBatchSize,
			openAIFirstTokenTimeoutPlaceholderGuardStateTTL.Milliseconds(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"guard_key", "real_token_ms", "recorded_at_ns"}).
			AddRow(openAIFirstTokenTimeoutPlaceholderGuardKey(1, "gpt-5.4"), 800, int64(200)))
	mock.ExpectExec("DELETE FROM openai_first_token_guard_outbox").
		WithArgs(sqlmock.AnyArg(), int64(200)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	second.openaiFirstTokenTimeoutPlaceholderGuard.drainOutbox()

	require.Equal(t, 100, second.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAIStreamFirstTokenTimeoutPlaceholderGuardRejectsOlderRetry(t *testing.T) {
	redisServer := miniredis.RunT(t)
	svc := newOpenAIFirstTokenPlaceholderGuardTestService(t, redisServer.Addr())
	guard := &svc.openaiFirstTokenTimeoutPlaceholderGuard
	key := openAIFirstTokenTimeoutPlaceholderGuardKey(1, "gpt-5.4")

	require.NoError(t, guard.writeSample(openAIFirstTokenTimeoutPlaceholderGuardSample{
		key:         key,
		realTokenMS: 800,
		recordedAt:  200,
	}))
	require.NoError(t, guard.writeSample(openAIFirstTokenTimeoutPlaceholderGuardSample{
		key:         key,
		realTokenMS: 20000,
		recordedAt:  100,
	}))
	require.True(t, guard.allow(1, "gpt-5.4", 3000))
}

func TestOpenAIStreamFirstTokenTimeoutPlaceholderGuardUsesProvidedCompletionTime(t *testing.T) {
	redisServer := miniredis.RunT(t)
	svc := newOpenAIFirstTokenPlaceholderGuardTestService(t, redisServer.Addr())
	account := openAIFirstTokenPlaceholderGuardTestAccount()

	svc.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, "gpt-5.4", 800, 200)
	svc.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, "gpt-5.4", 20000, 100)
	require.Equal(t, 100, svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4"))
}

func TestOpenAIStreamFirstTokenTimeoutPlaceholderGuardCoalescesConcurrentReads(t *testing.T) {
	redisServer := miniredis.RunT(t)
	svc := newOpenAIFirstTokenPlaceholderGuardTestService(t, redisServer.Addr())
	account := openAIFirstTokenPlaceholderGuardTestAccount()
	hook := &openAIFirstTokenPlaceholderGuardReadHook{delay: 10 * time.Millisecond}
	svc.openaiFirstTokenTimeoutPlaceholderGuard.redisClient.AddHook(hook)

	const readers = 64
	start := make(chan struct{})
	results := make(chan int, readers)
	var wg sync.WaitGroup
	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			<-start
			results <- svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4")
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for result := range results {
		require.Equal(t, 100, result)
	}

	require.LessOrEqual(t, hook.getCalls.Load(), int64(2))
}

func TestOpenAIStreamFirstTokenTimeoutPlaceholderGuardIsScopedByAccountAndModel(t *testing.T) {
	redisServer := miniredis.RunT(t)
	svc := newOpenAIFirstTokenPlaceholderGuardTestService(t, redisServer.Addr())
	account := openAIFirstTokenPlaceholderGuardTestAccount()

	svc.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, "gpt-5.4", 20000)
	require.Zero(t, svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4"))
	require.Equal(t, 100, svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.5"))

	otherAccount := openAIFirstTokenPlaceholderGuardTestAccount()
	otherAccount.ID = 2
	require.Equal(t, 100, svc.openAIStreamFirstTokenTimeoutPlaceholderMs(otherAccount, "gpt-5.4"))
}

func TestOpenAIStreamFirstTokenTimeoutPlaceholderGuardUsesCurrentAccountThresholds(t *testing.T) {
	redisServer := miniredis.RunT(t)
	svc := newOpenAIFirstTokenPlaceholderGuardTestService(t, redisServer.Addr())
	account := openAIFirstTokenPlaceholderGuardTestAccount()

	svc.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, "gpt-5.4", 6000)
	require.Zero(t, svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4"))

	account.Extra[openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey] = 1500
	account.Extra[openAIAPIKeyFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey] = 7000
	require.Equal(t, 1500, svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4"))
}

func TestOpenAIStreamFirstTokenTimeoutPlaceholderGuardExpiresAndFailsOpen(t *testing.T) {
	redisServer := miniredis.RunT(t)
	svc := newOpenAIFirstTokenPlaceholderGuardTestService(t, redisServer.Addr())
	account := openAIFirstTokenPlaceholderGuardTestAccount()

	svc.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, "gpt-5.4", 20000)
	require.Zero(t, svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4"))
	redisServer.FastForward(openAIFirstTokenTimeoutPlaceholderGuardStateTTL + time.Second)
	require.Equal(t, 100, svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4"))

	redisServer.Close()
	require.Equal(t, 100, svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4"))
}

func TestOpenAIStreamFirstTokenTimeoutPlaceholderGuardCanBeDisabled(t *testing.T) {
	redisServer := miniredis.RunT(t)
	svc := newOpenAIFirstTokenPlaceholderGuardTestService(t, redisServer.Addr())
	account := openAIFirstTokenPlaceholderGuardTestAccount()
	account.Extra[openAIAPIKeyFirstTokenTimeoutPlaceholderGuardEnabledExtraKey] = false

	svc.recordOpenAIFirstTokenTimeoutPlaceholderGuardSample(account, "gpt-5.4", 20000)
	require.Equal(t, 100, svc.openAIStreamFirstTokenTimeoutPlaceholderMs(account, "gpt-5.4"))
}
