package service

import (
	"context"
	"io"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type v025ReaderConn struct {
	openAIWSFakeConn
	incoming chan []byte
	done     chan struct{}
	once     sync.Once
}

func (*v025ReaderConn) RequiresReaderLoop() bool { return true }
func (c *v025ReaderConn) ReadMessage(ctx context.Context) ([]byte, error) {
	select {
	case b, ok := <-c.incoming:
		if !ok {
			return nil, io.EOF
		}
		return b, nil
	case <-c.done:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (c *v025ReaderConn) CloseNow() error { c.once.Do(func() { close(c.done) }); return nil }

func TestOpenAIWSReaderKeepsTerminalOnImmediateClose(t *testing.T) {
	ws := &v025ReaderConn{incoming: make(chan []byte, 1), done: make(chan struct{})}
	c := newOpenAIWSConn("reader", 1, ws, nil)
	defer c.close()
	require.True(t, c.tryAcquire())
	ws.incoming <- []byte(`{"type":"response.completed"}`)
	close(ws.incoming)
	require.Eventually(t, func() bool {
		select {
		case <-c.closedCh:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	b, err := c.readMessageWithTimeout(time.Second)
	require.NoError(t, err)
	require.Contains(t, string(b), "response.completed")
	_, err = c.readMessageWithTimeout(time.Second)
	require.ErrorIs(t, err, io.EOF)
}

func TestOpenAIWSReaderRejectsDirtyIdleConnection(t *testing.T) {
	ws := &v025ReaderConn{incoming: make(chan []byte, 1), done: make(chan struct{})}
	c := newOpenAIWSConn("dirty", 1, ws, nil)
	defer c.close()
	ws.incoming <- []byte(`{"type":"response.output_text.delta","delta":"old"}`)
	require.Eventually(t, c.leaseHasPendingData, time.Second, time.Millisecond)
	require.False(t, c.tryAcquire())
}

func TestOpenAIWSExecutionScopeSeparatesSideRequests(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set("session_id", "shared")
	scope := func(thread, kind string, key int64) string {
		c.Request.Header.Set(openAIWSTurnMetadataHeader, `{"thread_id":"`+thread+`","request_kind":"`+kind+`"}`)
		return resolveOpenAIWSExecutionScope(c, nil, key)
	}
	turn := scope("thread", "turn", 1)
	require.NotEmpty(t, turn)
	require.Equal(t, turn, scope("thread", "prewarm", 1))
	require.Equal(t, turn, scope("thread", "compaction", 1))
	require.NotEqual(t, turn, scope("thread", "memory", 1))
	require.NotEqual(t, turn, scope("child", "turn", 1))
	require.NotEqual(t, turn, scope("thread", "turn", 2))
	require.Empty(t, scope("", "turn", 1))
	c.Request.Header.Set(openAIWSTurnMetadataHeader, `{"thread_id":"bad","request_kind":42}`)
	require.Empty(t, resolveOpenAIWSExecutionScope(c, nil, 1))
}

func TestOpenAIWSQueueWakeSelectsAvailableConnection(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 2
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 4
	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()
	account := &Account{ID: 88, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 2}
	first := newOpenAIWSConn("first", account.ID, &openAIWSFakeConn{}, nil)
	second := newOpenAIWSConn("second", account.ID, &openAIWSFakeConn{}, nil)
	require.True(t, first.tryAcquire())
	require.True(t, second.tryAcquire())
	defer first.release()
	ap := pool.getOrCreateAccountPool(account.ID)
	ap.mu.Lock()
	ap.conns[first.id], ap.conns[second.id] = first, second
	ap.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan *openAIWSConnLease, 1)
	errors := make(chan error, 1)
	go func() {
		lease, err := pool.Acquire(ctx, openAIWSAcquireRequest{Account: account, WSURL: "wss://example.com/v1/responses", PreferredConnID: first.id})
		if err != nil {
			errors <- err
			return
		}
		result <- lease
	}()
	require.Eventually(t, func() bool { return first.waiters.Load() == 1 }, time.Second, time.Millisecond)
	// The next acquisition goes through the idle fast path after a pool wake;
	// its reported queue time must still include the wait on the first conn.
	time.Sleep(20 * time.Millisecond)
	(&openAIWSConnLease{pool: pool, accountID: account.ID, conn: second}).Release()
	select {
	case lease := <-result:
		defer lease.Release()
		require.Equal(t, second.id, lease.ConnID())
		require.Zero(t, first.waiters.Load())
		require.GreaterOrEqual(t, lease.QueueWaitDuration(), 20*time.Millisecond)
		require.Equal(t, int64(1), pool.metrics.acquireQueueWaitTotal.Load())
		require.GreaterOrEqual(t, pool.metrics.acquireQueueWaitMs.Load(), int64(20))
	case err := <-errors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("waiter stayed on the busy connection")
	}
}
