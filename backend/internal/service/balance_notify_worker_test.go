//go:build unit

package service

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type blockedNotifySettings struct {
	SettingRepository
	started    chan struct{}
	active     atomic.Int32
	peak       atomic.Int32
	calls      atomic.Int32
	panicFirst atomic.Bool
}

func (r *blockedNotifySettings) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	r.calls.Add(1)
	if r.panicFirst.CompareAndSwap(true, false) {
		panic("fixture")
	}
	current := r.active.Add(1)
	defer r.active.Add(-1)
	for {
		old := r.peak.Load()
		if current <= old || r.peak.CompareAndSwap(old, current) {
			break
		}
	}
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestBillingNotifyWorkerBoundsQueueAndCancelsStop(t *testing.T) {
	repo := &blockedNotifySettings{started: make(chan struct{}, 10)}
	svc := NewBalanceNotifyService(&EmailService{}, repo, nil)
	defer svc.Stop()
	for range billingNotifyWorkers {
		require.True(t, svc.worker.submit(billingNotifyJob{user: &User{ID: 1}}))
	}
	for range billingNotifyWorkers {
		select {
		case <-repo.started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	for range billingNotifyQueueSize {
		require.True(t, svc.worker.submit(billingNotifyJob{}))
	}
	start := time.Now()
	require.False(t, svc.worker.submit(billingNotifyJob{}))
	require.Less(t, time.Since(start), 100*time.Millisecond)
	done := make(chan struct{})
	go func() { svc.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop failed to cancel active settings reads")
	}
	require.Zero(t, repo.active.Load())
	require.EqualValues(t, billingNotifyWorkers, repo.peak.Load())
	require.GreaterOrEqual(t, svc.NotificationDropped(), uint64(billingNotifyQueueSize+1))
	require.False(t, svc.worker.submit(billingNotifyJob{}))
}
func TestBillingNotifyWorkerPanicAndConcurrentShutdown(t *testing.T) {
	repo := &blockedNotifySettings{started: make(chan struct{}, 32)}
	repo.panicFirst.Store(true)
	svc := NewBalanceNotifyService(&EmailService{}, repo, nil)
	for range 12 {
		svc.worker.submit(billingNotifyJob{})
	}
	select {
	case <-repo.started:
	case <-time.After(time.Second):
		t.Fatal("worker did not recover")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 50 {
				svc.worker.submit(billingNotifyJob{})
			}
		})
	}
	for range 4 {
		wg.Go(svc.Stop)
	}
	wg.Wait()
	require.Zero(t, repo.active.Load())
	require.LessOrEqual(t, repo.peak.Load(), int32(billingNotifyWorkers))
}
func TestNotificationSMTPGreetingCancellationClosesSocket(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	closed := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buffer := make([]byte, 1)
		_, _ = conn.Read(buffer)
		close(closed)
	}()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	n, err := strconv.Atoi(port)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, cleanup, err := connectNotificationSMTP(ctx, &SMTPConfig{Host: host, Port: n})
	if cleanup != nil {
		cleanup()
	}
	require.Error(t, err)
	require.Less(t, time.Since(start), time.Second)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancelled SMTP connection stayed open")
	}
}
