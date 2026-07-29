package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type liveHTTPUpstreamStub struct {
	request *http.Request
	body    []byte
}

func (s *liveHTTPUpstreamStub) Do(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	s.request = request
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	s.body = body
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Location": {"/backend-api/codex/call_test"}},
		Body:       io.NopCloser(strings.NewReader("v=0\r\n")),
	}, nil
}

func (s *liveHTTPUpstreamStub) DoWithTLS(request *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(request, proxyURL, accountID, concurrency)
}

type liveStoreStub struct {
	GatewayCache
	mu              sync.Mutex
	record          *LiveCallRecord
	getFailures     int
	releaseFailures int
	claimStarted    chan struct{}
	claimOnce       sync.Once
	claimCalls      int
	claimCommitErr  error
	markFailures    int
}

type liveConcurrencyCacheStub struct {
	ConcurrencyCache
	mu                     sync.Mutex
	tenantLeaseActive      bool
	accountLeaseActive     bool
	tenantReleaseAttempts  int
	accountReleaseAttempts int
}

func (s *liveConcurrencyCacheStub) AcquireLiveTenantLease(context.Context, int64, int, int64, string) (bool, error) {
	return false, nil
}

func (s *liveConcurrencyCacheStub) AcquireLiveAccountLease(context.Context, string, int64, int, string, bool) (bool, error) {
	return false, nil
}

func (s *liveConcurrencyCacheStub) RefreshLiveTenantLease(context.Context, int64, int64, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tenantLeaseActive, nil
}

func (s *liveConcurrencyCacheStub) RefreshLiveAccountLease(context.Context, int64, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accountLeaseActive, nil
}

func (s *liveConcurrencyCacheStub) ReleaseLiveTenantLease(context.Context, int64, int64, string) error {
	s.mu.Lock()
	s.tenantReleaseAttempts++
	s.tenantLeaseActive = false
	s.mu.Unlock()
	return nil
}

func (s *liveConcurrencyCacheStub) ReleaseLiveAccountLease(context.Context, int64, string) error {
	s.mu.Lock()
	s.accountReleaseAttempts++
	s.accountLeaseActive = false
	s.mu.Unlock()
	return nil
}

type liveUsageLogRepoStub struct {
	UsageLogRepository
	mu   sync.Mutex
	logs []*UsageLog
}

func (s *liveUsageLogRepoStub) Create(_ context.Context, log *UsageLog) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := *log
	s.logs = append(s.logs, &copy)
	return true, nil
}

type liveTestFrame struct {
	typ     coderws.MessageType
	payload []byte
	err     error
}

type liveFrameConnStub struct {
	reads  chan liveTestFrame
	writes chan liveTestFrame
	closed chan struct{}
	once   sync.Once
}

func newLiveFrameConnStub() *liveFrameConnStub {
	return &liveFrameConnStub{
		reads:  make(chan liveTestFrame, 4),
		writes: make(chan liveTestFrame, 4),
		closed: make(chan struct{}),
	}
}

func (c *liveFrameConnStub) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, context.Cause(ctx)
	case <-c.closed:
		return 0, nil, io.EOF
	case frame := <-c.reads:
		return frame.typ, append([]byte(nil), frame.payload...), frame.err
	}
}

func (c *liveFrameConnStub) WriteFrame(ctx context.Context, typ coderws.MessageType, payload []byte) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-c.closed:
		return io.EOF
	case c.writes <- liveTestFrame{typ: typ, payload: append([]byte(nil), payload...)}:
		return nil
	}
}

func (c *liveFrameConnStub) ReadMessage(ctx context.Context) ([]byte, error) {
	_, payload, err := c.ReadFrame(ctx)
	return payload, err
}

func (c *liveFrameConnStub) WriteJSON(ctx context.Context, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.WriteFrame(ctx, coderws.MessageText, payload)
}

func (c *liveFrameConnStub) Ping(context.Context) error { return nil }

func (c *liveFrameConnStub) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

type liveDialerStub struct {
	conn    *liveFrameConnStub
	mu      sync.Mutex
	url     string
	headers http.Header
}

func (d *liveDialerStub) Dial(
	_ context.Context,
	wsURL string,
	headers http.Header,
	_ string,
) (openAIWSClientConn, int, http.Header, error) {
	d.mu.Lock()
	d.url = wsURL
	d.headers = headers.Clone()
	d.mu.Unlock()
	return d.conn, 0, nil, nil
}

func (s *liveStoreStub) SaveLiveCall(_ context.Context, record *LiveCallRecord, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := *record
	s.record = &copy
	return nil
}

func (s *liveStoreStub) GetLiveCall(_ context.Context, callHash string) (*LiveCallRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getFailures > 0 {
		s.getFailures--
		return nil, errors.New("temporary redis failure")
	}
	if s.record == nil || s.record.CallHash != callHash {
		return nil, ErrLiveCallNotFound
	}
	copy := *s.record
	return &copy, nil
}

func (s *liveStoreStub) ClaimLiveController(ctx context.Context, callHash, controller, owner string) (bool, error) {
	if s.claimStarted != nil {
		s.claimOnce.Do(func() { close(s.claimStarted) })
	}
	s.mu.Lock()
	s.claimCalls++
	if s.record == nil || s.record.CallHash != callHash || s.record.Controller == LiveControllerClosed {
		s.mu.Unlock()
		return false, nil
	}
	if controller == LiveControllerObserver && s.record.Controller != LiveControllerPending {
		s.mu.Unlock()
		return false, nil
	}
	s.record.Controller = controller
	s.record.ControllerOwner = owner
	commitErr := s.claimCommitErr
	s.claimCommitErr = nil
	s.mu.Unlock()
	if commitErr != nil {
		return false, commitErr
	}
	if s.claimStarted != nil && controller == LiveControllerObserver {
		<-ctx.Done()
		return false, context.Cause(ctx)
	}
	return true, nil
}

func (s *liveStoreStub) ReleaseLiveController(_ context.Context, callHash, owner string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.releaseFailures > 0 {
		s.releaseFailures--
		return false, errors.New("temporary redis failure")
	}
	if s.record == nil || s.record.CallHash != callHash || s.record.ControllerOwner != owner {
		return false, nil
	}
	s.record.Controller = LiveControllerPending
	s.record.ControllerOwner = ""
	return true, nil
}

func (s *liveStoreStub) GetLiveController(_ context.Context, callHash string) (string, error) {
	record, err := s.GetLiveCall(context.Background(), callHash)
	if err != nil {
		return "", err
	}
	return record.Controller, nil
}

func (s *liveStoreStub) MarkLiveCallClosed(_ context.Context, callHash string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markFailures > 0 {
		s.markFailures--
		return false, errors.New("temporary redis failure")
	}
	if s.record == nil || s.record.CallHash != callHash || s.record.Controller == LiveControllerClosed {
		return false, nil
	}
	s.record.Controller = LiveControllerClosed
	s.record.ControllerOwner = ""
	return true, nil
}

func TestFinalizeLiveCallSettlesWhenCloseMarkerTemporarilyFails(t *testing.T) {
	record := &LiveCallRecord{
		CallID: "call-finalize", CallHash: "hash-finalize", Controller: LiveControllerPending,
		AccountID: 7, UserID: 11, APIKeyID: 12, LeaseID: "lease-finalize",
		Model: "gpt-live", CreatedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour),
	}
	store := &liveStoreStub{markFailures: 1}
	require.NoError(t, store.SaveLiveCall(context.Background(), record, time.Hour))
	leases := &liveConcurrencyCacheStub{tenantLeaseActive: true, accountLeaseActive: true}
	usage := &liveUsageLogRepoStub{}
	svc := &OpenAIGatewayService{
		cache: store, usageLogRepo: usage,
		concurrencyService: &ConcurrencyService{cache: leases},
	}

	svc.finalizeLiveCall(record)

	leasingDeadline := time.Now().Add(time.Second)
	for {
		store.mu.Lock()
		closed := store.record != nil && store.record.Controller == LiveControllerClosed
		store.mu.Unlock()
		if closed || time.Now().After(leasingDeadline) {
			require.True(t, closed, "close marker retry did not recover")
			break
		}
		time.Sleep(time.Millisecond)
	}
	leases.mu.Lock()
	require.Equal(t, 1, leases.tenantReleaseAttempts)
	require.Equal(t, 1, leases.accountReleaseAttempts)
	leases.mu.Unlock()
	usage.mu.Lock()
	require.Len(t, usage.logs, 1)
	require.Equal(t, record.CallHash, usage.logs[0].RequestID)
	require.Equal(t, RequestTypeLive, usage.logs[0].RequestType)
	usage.mu.Unlock()
}

func TestRefreshLiveLeaseFailsClosedWhenPersistentLeaseExpired(t *testing.T) {
	leases := &liveConcurrencyCacheStub{tenantLeaseActive: false, accountLeaseActive: true}
	svc := &OpenAIGatewayService{concurrencyService: &ConcurrencyService{cache: leases}}
	require.False(t, svc.refreshLiveLease(&LiveCallRecord{
		AccountID: 7, UserID: 11, APIKeyID: 12, LeaseID: "expired",
	}))
}

func TestLiveObserverCapacityFailsClosedBeforeLeaseCanExpireInQueue(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	svc.cfg.Gateway.Live.ObserverWorkers = 1
	svc.cfg.Gateway.Live.ObserverQueueSize = 8
	t.Cleanup(svc.StopLiveSessions)

	_, ok := svc.liveRuntimeContext()
	require.True(t, ok)
	// Occupy the one long-lived observer permit without depending on a Redis
	// store. A second distinct call must not be admitted merely because the
	// buffered dispatch queue still has room.
	svc.liveObserverPending.Store("active-call", struct{}{})
	svc.liveObserverPermits <- struct{}{}
	runtimeCtx, ok := svc.liveRuntimeContext()
	require.True(t, ok)
	require.False(t, svc.reserveLiveObserverPermit(runtimeCtx), "admission must fail before tenant/account lease acquisition")
	require.False(t, svc.enqueueLiveObserver("queued-call"))
	_, pending := svc.liveObserverPending.Load("queued-call")
	require.False(t, pending)
	<-svc.liveObserverPermits
	svc.liveObserverPending.Delete("active-call")
}

func TestStopLiveSessionsWaitsForInFlightCreate(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	_, ok := svc.beginLiveCreate()
	require.True(t, ok)

	stopped := make(chan struct{})
	go func() {
		svc.StopLiveSessions()
		close(stopped)
	}()
	require.Eventually(t, svc.liveObserverStopped.Load, time.Second, time.Millisecond)
	select {
	case <-stopped:
		t.Fatal("shutdown returned while a Live create was still active")
	default:
	}
	svc.liveCreateWorkers.Done()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after the Live create exited")
	}
}

func TestLiveCapabilityOnlyAllowsOpenAIOAuth(t *testing.T) {
	require.True(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}).SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityLive))
	require.False(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}).SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityLive))
	require.False(t, (&Account{Platform: PlatformGrok, Type: AccountTypeOAuth}).SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityLive))
	require.False(t, (&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"auth_mode": OpenAIAuthModeAgentIdentity,
		},
	}).SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityLive))
}

func TestValidateLiveCallRequestPreservesSessionObject(t *testing.T) {
	request := &LiveCallRequest{
		SDP:     "v=0\r\n",
		Session: json.RawMessage(`{"model":"gpt-live","custom":{"keep":true}}`),
	}
	require.NoError(t, ValidateLiveCallRequest(request))
	require.Contains(t, string(request.Session), `"keep":true`)
	for _, invalid := range []*LiveCallRequest{
		nil,
		{Session: json.RawMessage(`{"model":"gpt-live"}`)},
		{SDP: "v=0", Session: json.RawMessage(`[]`)},
		{SDP: "v=0", Session: json.RawMessage(`null`)},
	} {
		require.Error(t, ValidateLiveCallRequest(invalid))
	}
}

func TestCreateUpstreamLiveCallPreservesProtocolAndSession(t *testing.T) {
	upstream := &liveHTTPUpstreamStub{}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:          7,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 2,
		Credentials: map[string]any{
			"access_token":       "test-access-token",
			"chatgpt_account_id": "acct_test",
		},
	}
	session := json.RawMessage(`{"model":"gpt-live","custom":{"keep":true}}`)
	created, err := svc.createUpstreamLiveCall(context.Background(), account, &LiveCallRequest{
		SDP: "v=offer\r\n", Session: session,
	}, `{"opaque":"attestation"}`)
	require.NoError(t, err)
	require.Equal(t, "call_test", created.CallID)
	require.Equal(t, []byte("v=0\r\n"), created.SDP)

	var forwarded LiveCallRequest
	require.NoError(t, json.Unmarshal(upstream.body, &forwarded))
	require.Equal(t, "v=offer\r\n", forwarded.SDP)
	require.JSONEq(t, string(session), string(forwarded.Session))
	require.Equal(t, "Bearer test-access-token", upstream.request.Header.Get("Authorization"))
	require.Equal(t, "acct_test", upstream.request.Header.Get("Chatgpt-Account-Id"))
	require.Equal(t, "quicksilver=v2", upstream.request.Header.Get("OpenAI-Alpha"))
	require.Equal(t, `{"opaque":"attestation"}`, upstream.request.Header.Get(liveAttestationHeader))
	require.Empty(t, upstream.request.Header.Get("OpenAI-Beta"))
}

func TestCreateUpstreamLiveCallHonorsAccountStrongIsolation(t *testing.T) {
	upstream := &liveHTTPUpstreamStub{}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 2,
		Credentials: map[string]any{
			"access_token":                      "test-access-token",
			"upstream_strong_isolation_enabled": true,
		},
	}
	original := json.RawMessage(`{"model":"gpt-live","conversation_id":"conv","session_id":"sess","previous_response_id":"resp","client_metadata":{"x-codex-turn-state":"state","safe":"value"},"store":true,"custom":{"keep":true}}`)
	created, err := svc.createUpstreamLiveCall(context.Background(), account, &LiveCallRequest{
		SDP: "v=offer\r\n", Session: original,
	}, `{"opaque":"attestation"}`)
	require.NoError(t, err)
	require.NotNil(t, created)

	var forwarded LiveCallRequest
	require.NoError(t, json.Unmarshal(upstream.body, &forwarded))
	for _, field := range []string{"conversation_id", "session_id", "previous_response_id", "client_metadata"} {
		require.False(t, gjson.GetBytes(forwarded.Session, field).Exists(), field)
	}
	require.False(t, gjson.GetBytes(forwarded.Session, "store").Bool())
	require.True(t, gjson.GetBytes(forwarded.Session, "custom.keep").Bool())
	require.Contains(t, string(original), `"conversation_id":"conv"`, "caller-owned session must stay unchanged")
	require.Empty(t, upstream.request.Header.Get("originator"))
	require.Empty(t, upstream.request.Header.Get("session_id"))
}

func TestLiveSidebandHeadersHonorAccountStrongIsolationAndDisabledNoOp(t *testing.T) {
	cipher := newLiveAttestationCipher(&config.Config{JWT: config.JWTConfig{Secret: "live-test-secret"}})
	ciphertext, err := cipher.Encrypt(`{"device":"test"}`)
	require.NoError(t, err)
	svc := &OpenAIGatewayService{
		accountRepo:           stubOpenAIAccountRepo{},
		liveAttestationCipher: cipher,
	}
	record := &LiveCallRecord{AttestationCiphertext: ciphertext}

	for _, tc := range []struct {
		name            string
		strongIsolation bool
		wantOriginator  bool
	}{
		{name: "disabled_noop", wantOriginator: true},
		{name: "enabled", strongIsolation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token":                      "test-access-token",
					"upstream_strong_isolation_enabled": tc.strongIsolation,
				},
			}
			headers, headerErr := svc.liveSidebandHeaders(context.Background(), account, record)
			require.NoError(t, headerErr)
			require.Equal(t, "Bearer test-access-token", headers.Get("Authorization"))
			require.Equal(t, `{"device":"test"}`, headers.Get(liveAttestationHeader))
			require.NotEmpty(t, headers.Get("session-id"))
			require.NotEmpty(t, headers.Get("thread-id"))
			if tc.wantOriginator {
				require.NotEmpty(t, headers.Get("originator"))
			} else {
				require.Empty(t, headers.Get("originator"))
			}
		})
	}
}

func TestLiveSessionEndedTreatsLeaseLossAsTerminal(t *testing.T) {
	require.True(t, liveSessionEnded(ErrLiveUnavailable))
	require.True(t, liveSessionEnded(ErrLiveCallNotFound))
	require.True(t, liveSessionEnded(context.DeadlineExceeded))
	require.False(t, liveSessionEnded(ErrLiveControllerChanged))
	require.False(t, liveSessionEnded(errors.New("temporary read failure")))
}

func TestSanitizeLiveUpstreamLogValueRemovesIdentityAndCredentials(t *testing.T) {
	for _, unsafe := range []string{
		`request to https://chatgpt.com/backend-api/codex failed`,
		`<!DOCTYPE html><title>cloudflare error</title>`,
		`Authorization: Bearer live-secret-token`,
		`Cookie=session=live-secret-cookie`,
		`api_key=live-secret-key`,
	} {
		sanitized := sanitizeLiveUpstreamLogValue(unsafe, 300)
		require.NotContains(t, strings.ToLower(sanitized), "chatgpt")
		require.NotContains(t, strings.ToLower(sanitized), "cloudflare")
		require.NotContains(t, sanitized, "live-secret")
		require.NotContains(t, sanitized, "<!DOCTYPE")
	}
}

func TestDialLiveSidebandFailsClosedWithoutRequiredDependencies(t *testing.T) {
	var nilService *OpenAIGatewayService
	_, err := nilService.dialLiveSideband(context.Background(), &LiveCallRecord{})
	require.ErrorIs(t, err, ErrLiveUnavailable)

	svc := &OpenAIGatewayService{}
	_, err = svc.dialLiveSideband(context.Background(), nil)
	require.ErrorIs(t, err, ErrLiveUnavailable)
	_, err = svc.dialLiveSideband(context.Background(), &LiveCallRecord{})
	require.ErrorIs(t, err, ErrLiveUnavailable)
}

func TestLiveObserverSupervisorIsBoundedAndStops(t *testing.T) {
	record := &LiveCallRecord{
		CallID: "call-1", CallHash: "hash-1", Controller: LiveControllerPending,
		AccountID: 7, UserID: 11, APIKeyID: 12, LeaseID: "lease-1",
		Model: "gpt-live", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	store := &liveStoreStub{claimStarted: make(chan struct{})}
	require.NoError(t, store.SaveLiveCall(context.Background(), record, time.Hour))
	leases := &liveConcurrencyCacheStub{tenantLeaseActive: true, accountLeaseActive: true}
	usage := &liveUsageLogRepoStub{}
	svc := &OpenAIGatewayService{
		cache: store, usageLogRepo: usage,
		concurrencyService: &ConcurrencyService{cache: leases},
		cfg: &config.Config{Gateway: config.GatewayConfig{Live: config.GatewayLiveConfig{
			ObserverWorkers: 1, ObserverQueueSize: 1, ProxyConnections: 1,
		}}},
	}
	require.True(t, svc.enqueueLiveObserver("hash-1"))
	select {
	case <-store.claimStarted:
	case <-time.After(time.Second):
		t.Fatal("observer worker did not start")
	}
	require.False(t, svc.enqueueLiveObserver("hash-2"))
	require.False(t, svc.enqueueLiveObserver("hash-3"))

	done := make(chan struct{})
	go func() {
		svc.StopLiveSessions()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Live supervisor did not stop after cancellation")
	}
	store.mu.Lock()
	require.Equal(t, LiveControllerClosed, store.record.Controller)
	store.mu.Unlock()
	leases.mu.Lock()
	require.Equal(t, 1, leases.tenantReleaseAttempts)
	require.Equal(t, 1, leases.accountReleaseAttempts)
	leases.mu.Unlock()
	usage.mu.Lock()
	require.Len(t, usage.logs, 1)
	require.Equal(t, record.CallHash, usage.logs[0].RequestID)
	usage.mu.Unlock()
	require.False(t, svc.enqueueLiveObserver("hash-after-stop"))
}

func TestLiveProxyPermitFailsFastWhenCapacityIsFull(t *testing.T) {
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{Live: config.GatewayLiveConfig{
			ObserverWorkers: 1, ObserverQueueSize: 1, ProxyConnections: 1,
		}}},
	}
	_, ok := svc.liveRuntimeContext()
	require.True(t, ok)
	t.Cleanup(svc.StopLiveSessions)

	require.True(t, svc.acquireLiveProxyPermit(context.Background()))
	started := time.Now()
	require.False(t, svc.acquireLiveProxyPermit(context.Background()))
	require.Less(t, time.Since(started), 100*time.Millisecond)

	svc.liveProxyWorkers.Done()
	svc.releaseLiveProxyPermit()
}

func TestResumeLiveObserverAfterFastProxyHandoff(t *testing.T) {
	record := &LiveCallRecord{
		CallID: "call-handoff", CallHash: "hash-handoff", Controller: LiveControllerPending,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	store := &liveStoreStub{}
	require.NoError(t, store.SaveLiveCall(context.Background(), record, time.Hour))
	svc := &OpenAIGatewayService{
		cache: store,
		cfg: &config.Config{Gateway: config.GatewayConfig{Live: config.GatewayLiveConfig{
			ObserverWorkers: 1, ObserverQueueSize: 1, ProxyConnections: 1,
		}}},
	}
	_, ok := svc.liveRuntimeContext()
	require.True(t, ok)
	svc.liveObserverPending.Store(record.CallHash, struct{}{})
	// This mirrors the old observer worker finishing after the proxy already
	// released ownership back to pending.
	svc.liveObserverPending.Delete(record.CallHash)
	svc.resumeLiveObserverIfPending(record.CallHash)
	_, pending := svc.liveObserverPending.Load(record.CallHash)
	require.True(t, pending)
	svc.StopLiveSessions()
}

func TestObserveLiveCallReleasesOwnershipAfterTransientStoreFailure(t *testing.T) {
	restoreRetryInterval := liveObserverStoreRetryInterval
	liveObserverStoreRetryInterval = time.Millisecond
	t.Cleanup(func() { liveObserverStoreRetryInterval = restoreRetryInterval })
	record := &LiveCallRecord{
		CallID: "call-retry", CallHash: "hash-retry", Controller: LiveControllerPending,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	store := &liveStoreStub{}
	require.NoError(t, store.SaveLiveCall(context.Background(), record, time.Hour))
	store.getFailures = 1
	svc := &OpenAIGatewayService{cache: store}
	svc.liveObserverPending.Store(record.CallHash, struct{}{})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	svc.observeLiveCall(ctx, record.CallHash)

	store.mu.Lock()
	require.Equal(t, LiveControllerPending, store.record.Controller)
	require.Empty(t, store.record.ControllerOwner)
	store.mu.Unlock()
	svc.liveObserverPending.Delete(record.CallHash)
}

func TestObserveLiveCallRecognizesTimedOutClaimOwnedBySameWorker(t *testing.T) {
	restoreRetryInterval := liveObserverStoreRetryInterval
	liveObserverStoreRetryInterval = time.Millisecond
	t.Cleanup(func() { liveObserverStoreRetryInterval = restoreRetryInterval })
	record := &LiveCallRecord{
		CallID: "call-claim-timeout", CallHash: "hash-claim-timeout", Controller: LiveControllerPending,
		CreatedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Second),
	}
	store := &liveStoreStub{claimCommitErr: context.DeadlineExceeded}
	require.NoError(t, store.SaveLiveCall(context.Background(), record, time.Hour))
	svc := &OpenAIGatewayService{cache: store}
	svc.liveObserverPending.Store(record.CallHash, struct{}{})

	svc.observeLiveCall(context.Background(), record.CallHash)

	store.mu.Lock()
	require.Equal(t, LiveControllerClosed, store.record.Controller, "same-owner verification must continue with the committed claim")
	require.Empty(t, store.record.ControllerOwner)
	require.Equal(t, 1, store.claimCalls, "same-owner verification must avoid a second claim")
	store.mu.Unlock()
	svc.liveObserverPending.Delete(record.CallHash)
}

func TestReleaseLiveControllerRetriesTransientStoreFailure(t *testing.T) {
	record := &LiveCallRecord{
		CallID: "call-release-retry", CallHash: "hash-release-retry",
		Controller: LiveControllerProxy, ControllerOwner: "proxy-owner",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	store := &liveStoreStub{}
	require.NoError(t, store.SaveLiveCall(context.Background(), record, time.Hour))
	store.releaseFailures = 1
	concurrency := NewConcurrencyService(nil)
	defer concurrency.StopBackgroundWorkers()
	svc := &OpenAIGatewayService{cache: store, concurrencyService: concurrency}
	svc.liveObserverPending.Store(record.CallHash, struct{}{})

	svc.releaseLiveControllerBestEffort(store, record.CallHash, record.ControllerOwner)

	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.record.Controller == LiveControllerPending && store.record.ControllerOwner == ""
	}, time.Second, time.Millisecond)
	svc.liveObserverPending.Delete(record.CallHash)
}

func TestGetLiveCallForIdentityRejectsCrossTenantAccess(t *testing.T) {
	groupID := int64(9)
	record := &LiveCallRecord{
		CallID: "call-isolated", CallHash: hashLiveCallID("call-isolated"),
		UserID: 11, APIKeyID: 12, GroupID: groupID,
		Controller: LiveControllerPending,
	}
	store := &liveStoreStub{}
	require.NoError(t, store.SaveLiveCall(context.Background(), record, time.Hour))
	svc := &OpenAIGatewayService{cache: store}

	got, err := svc.GetLiveCallForIdentity(context.Background(), record.CallID, LiveCallIdentity{
		UserID: 11, APIKeyID: 12, GroupID: &groupID,
	})
	require.NoError(t, err)
	require.Equal(t, record.CallID, got.CallID)

	for _, identity := range []LiveCallIdentity{
		{UserID: 99, APIKeyID: 12, GroupID: &groupID},
		{UserID: 11, APIKeyID: 99, GroupID: &groupID},
		{UserID: 11, APIKeyID: 12},
	} {
		_, err = svc.GetLiveCallForIdentity(context.Background(), record.CallID, identity)
		require.ErrorIs(t, err, ErrLiveIdentityMismatch)
	}
}

func TestProxyLiveSidebandPreservesTextAndBinaryFrames(t *testing.T) {
	groupID := int64(9)
	cipher := newLiveAttestationCipher(&config.Config{JWT: config.JWTConfig{Secret: "live-test-secret"}})
	ciphertext, err := cipher.Encrypt(`{"device":"test"}`)
	require.NoError(t, err)
	record := &LiveCallRecord{
		CallID: "call-frames", CallHash: hashLiveCallID("call-frames"),
		AccountID: 7, UserID: 11, APIKeyID: 12, GroupID: groupID, LeaseID: "lease-frames",
		Controller: LiveControllerPending, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		AttestationCiphertext: ciphertext,
	}
	store := &liveStoreStub{}
	require.NoError(t, store.SaveLiveCall(context.Background(), record, time.Hour))
	upstream := newLiveFrameConnStub()
	dialer := &liveDialerStub{conn: upstream}
	svc := &OpenAIGatewayService{
		cache: store,
		cfg: &config.Config{Gateway: config.GatewayConfig{Live: config.GatewayLiveConfig{
			ObserverWorkers: 1, ObserverQueueSize: 1, ProxyConnections: 1,
		}}},
		accountRepo: stubOpenAIAccountRepo{accounts: []Account{{
			ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
			Schedulable: true, Concurrency: 1,
			Credentials: map[string]any{"access_token": "test-access-token", "chatgpt_account_id": "acct_live"},
		}}},
		concurrencyService:        &ConcurrencyService{cache: &liveConcurrencyCacheStub{tenantLeaseActive: true, accountLeaseActive: true}},
		liveAttestationCipher:     cipher,
		openaiWSPassthroughDialer: dialer,
	}
	t.Cleanup(svc.StopLiveSessions)

	proxyResult := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downstream, acceptErr := coderws.Accept(w, r, nil)
		if acceptErr != nil {
			proxyResult <- acceptErr
			return
		}
		defer func() { _ = downstream.CloseNow() }()
		proxyResult <- svc.ProxyLiveSideband(r.Context(), record, downstream)
	}))
	defer server.Close()

	clientCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(clientCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()

	clientText := []byte(`{"type":"session.update","instructions":"keep bytes"}`)
	require.NoError(t, client.Write(clientCtx, coderws.MessageText, clientText))
	select {
	case frame := <-upstream.writes:
		require.Equal(t, coderws.MessageText, frame.typ)
		require.Equal(t, clientText, frame.payload)
	case <-clientCtx.Done():
		t.Fatal("client text frame was not forwarded upstream")
	}

	upstreamBinary := []byte{0x00, 0x01, 0x7f, 0xff}
	upstream.reads <- liveTestFrame{typ: coderws.MessageBinary, payload: upstreamBinary}
	typ, payload, err := client.Read(clientCtx)
	require.NoError(t, err)
	require.Equal(t, coderws.MessageBinary, typ)
	require.Equal(t, upstreamBinary, payload)

	upstream.reads <- liveTestFrame{typ: coderws.MessageText, payload: []byte(`{"type":"session.closed"}`)}
	_, _, err = client.Read(clientCtx)
	require.NoError(t, err)
	select {
	case err = <-proxyResult:
		require.ErrorIs(t, err, ErrLiveCallNotFound)
	case <-clientCtx.Done():
		t.Fatal("Live sideband proxy did not stop after session.closed")
	}

	dialer.mu.Lock()
	require.Equal(t, chatGPTLiveSidebandBaseURL+"/call-frames", dialer.url)
	require.Equal(t, "Bearer test-access-token", dialer.headers.Get("Authorization"))
	require.Equal(t, "acct_live", dialer.headers.Get("Chatgpt-Account-Id"))
	require.Equal(t, `{"device":"test"}`, dialer.headers.Get(liveAttestationHeader))
	dialer.mu.Unlock()
}

func TestSanitizeLiveSidebandFrameForClient(t *testing.T) {
	safeText := []byte(`{"type":"session.updated","session":{"model":"gpt-5.6"}}`)
	require.Equal(t, safeText, sanitizeLiveSidebandFrameForClient(coderws.MessageText, safeText))

	binary := []byte{0x00, 0x01, 0x7f, 0xff}
	require.Equal(t, binary, sanitizeLiveSidebandFrameForClient(coderws.MessageBinary, binary))

	for _, test := range []struct {
		name    string
		typ     coderws.MessageType
		payload []byte
	}{
		{name: "text error", typ: coderws.MessageText, payload: []byte(`{"type":"error","error":{"message":"https://private.example Authorization: Bearer secret"}}`)},
		{name: "namespaced error", typ: coderws.MessageText, payload: []byte(`{"type":"session.error","message":"<html>Cloudflare</html>"}`)},
		{name: "binary json error", typ: coderws.MessageBinary, payload: []byte(`{"type":"session.failed","error":"api.openai.com"}`)},
		{name: "malformed text", typ: coderws.MessageText, payload: []byte(`upstream private.example failed`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := sanitizeLiveSidebandFrameForClient(test.typ, test.payload)
			require.JSONEq(t, `{"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}`, string(got))
			require.NotContains(t, string(got), "private.example")
			require.NotContains(t, string(got), "openai.com")
			require.NotContains(t, string(got), "Bearer")
		})
	}
}

func TestRequestTypeLive(t *testing.T) {
	require.True(t, RequestTypeLive.IsValid())
	require.Equal(t, "live", RequestTypeLive.String())
	parsed, err := ParseUsageRequestType("live")
	require.NoError(t, err)
	require.Equal(t, RequestTypeLive, parsed)
}
