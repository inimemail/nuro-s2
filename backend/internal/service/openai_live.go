package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	coderws "github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	defaultLiveMaxSessionDuration = time.Hour
	liveLeaseRefreshInterval      = 20 * time.Second
	liveLeaseSetupTimeout         = 45 * time.Second
	liveRedisOperationTimeout     = 3 * time.Second
	liveClosedRecordTTL           = 24 * time.Hour
	liveObserverPollInterval      = 250 * time.Millisecond
	liveUpstreamBodyLimit         = 2 << 20
)

var liveObserverStoreRetryInterval = time.Second

var (
	chatGPTLiveCallsURL        = "https://chatgpt.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas"
	chatGPTLiveSidebandBaseURL = "wss://chatgpt.com/backend-api/codex"
)

type liveFrameConn interface {
	ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error)
	WriteFrame(ctx context.Context, msgType coderws.MessageType, payload []byte) error
	Close() error
}

func liveSidebandReadError(err error) error {
	if coderws.CloseStatus(err) == coderws.StatusNormalClosure {
		return ErrLiveCallNotFound
	}
	return err
}

func hashLiveCallID(callID string) string {
	sum := sha256.Sum256([]byte(callID))
	return hex.EncodeToString(sum[:])
}

func liveGroupID(groupID *int64) int64 {
	if groupID == nil {
		return 0
	}
	return *groupID
}

func liveOptionalID(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	result := value
	return &result
}

func liveRedisContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, liveRedisOperationTimeout)
}

func (s *OpenAIGatewayService) releaseLiveControllerBestEffort(store LiveCallStore, callHash, owner string) {
	if s == nil || store == nil {
		return
	}
	release := func(ctx context.Context) (bool, error) {
		return store.ReleaseLiveController(ctx, callHash, owner)
	}
	ctx, cancel := liveRedisContext(context.Background())
	released, err := release(ctx)
	cancel()
	if err != nil && s.concurrencyService != nil {
		s.concurrencyService.retryConcurrencySlotReleaseInBackground("live controller", 0, callHash, func(ctx context.Context) error {
			retryReleased, retryErr := release(ctx)
			if retryErr == nil && retryReleased {
				s.resumeLiveObserverIfPending(callHash)
			}
			return retryErr
		})
		return
	}
	if released {
		s.resumeLiveObserverIfPending(callHash)
	}
}

func (s *OpenAIGatewayService) liveStore() (LiveCallStore, error) {
	if s == nil || s.cache == nil {
		return nil, ErrLiveUnavailable
	}
	store, ok := s.cache.(LiveCallStore)
	if !ok {
		return nil, ErrLiveUnavailable
	}
	return store, nil
}

func (s *OpenAIGatewayService) liveConcurrencyCache() (LiveConcurrencyCache, error) {
	if s == nil || s.concurrencyService == nil || s.concurrencyService.cache == nil {
		return nil, ErrLiveUnavailable
	}
	cache, ok := s.concurrencyService.cache.(LiveConcurrencyCache)
	if !ok {
		return nil, ErrLiveUnavailable
	}
	return cache, nil
}

func (s *OpenAIGatewayService) liveMaxSessionDuration() time.Duration {
	if s != nil && s.cfg != nil && s.cfg.Gateway.Live.MaxSessionDurationSeconds > 0 {
		return time.Duration(s.cfg.Gateway.Live.MaxSessionDurationSeconds) * time.Second
	}
	return defaultLiveMaxSessionDuration
}

func ValidateLiveCallRequest(request *LiveCallRequest) error {
	if request == nil || strings.TrimSpace(request.SDP) == "" {
		return errors.New("sdp is required")
	}
	if len(request.Session) == 0 || !json.Valid(request.Session) {
		return errors.New("session must be valid JSON")
	}
	var sessionObject map[string]json.RawMessage
	if err := json.Unmarshal(request.Session, &sessionObject); err != nil {
		return errors.New("session must be a JSON object")
	}
	if sessionObject == nil {
		return errors.New("session must be a JSON object")
	}
	return nil
}

// CreateLiveCall creates a Frameless session. The tenant reservation is
// persistent and independent from request-local escrow state. The scheduler's
// ordinary account slot is atomically replaced inside the account's owner Cell.
func (s *OpenAIGatewayService) CreateLiveCall(
	ctx context.Context,
	request *LiveCallRequest,
	identity LiveCallIdentity,
	userMaxConcurrency int,
) (*LiveCallCreated, error) {
	runtimeCtx, ok := s.beginLiveCreate()
	if !ok {
		return nil, ErrLiveUnavailable
	}
	defer s.liveCreateWorkers.Done()
	if err := ValidateLiveCallRequest(request); err != nil {
		return nil, err
	}
	store, err := s.liveStore()
	if err != nil {
		return nil, err
	}
	liveCache, err := s.liveConcurrencyCache()
	if err != nil {
		return nil, err
	}
	if !s.reserveLiveObserverPermit(runtimeCtx) {
		return nil, ErrLiveConcurrencyFull
	}
	observerPermitOwned := true
	defer func() {
		if observerPermitOwned {
			<-s.liveObserverPermits
		}
	}()
	attestation, attestationCiphertext, err := s.prepareLiveAttestation(ctx)
	if err != nil {
		return nil, err
	}
	leaseID := generateRequestID()
	tenantAcquired, err := liveCache.AcquireLiveTenantLease(
		ctx, identity.UserID, userMaxConcurrency, identity.APIKeyID, leaseID,
	)
	if err != nil {
		return nil, err
	}
	if !tenantAcquired {
		return nil, ErrLiveConcurrencyFull
	}
	// The persistent lease expires after 60 seconds. Bound the complete setup
	// phase (scheduler claim, upstream SDP exchange and Redis mapping write) so
	// a transport with no response-header timeout cannot return a call whose
	// concurrency lease has already expired.
	setupCtx, setupCancel := context.WithTimeout(ctx, liveLeaseSetupTimeout)
	defer setupCancel()
	tenantOwned := true
	defer func() {
		if tenantOwned {
			s.releaseLiveTenantLease(identity.UserID, identity.APIKeyID, leaseID)
		}
	}()

	excluded := make(map[int64]struct{})
	var lastErr error
	for attempt := 0; attempt <= 3; attempt++ {
		selection, _, selectErr := s.SelectAccountWithSchedulerForCapability(
			setupCtx,
			identity.GroupID,
			"",
			// A Live call is pinned by its persistent account lease and call
			// mapping. A random one-shot session hash has no follow-up consumer;
			// it only adds sticky Redis reads/writes and leaves useless keys.
			"",
			"",
			excluded,
			OpenAIUpstreamTransportHTTPSSE,
			OpenAIEndpointCapabilityLive,
			false,
		)
		if selectErr != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, selectErr
		}
		if selection == nil || selection.Account == nil || !selection.Acquired {
			if selection != nil && selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			return nil, ErrLiveConcurrencyFull
		}

		account := selection.Account
		acquired, acquireErr := liveCache.AcquireLiveAccountLease(
			setupCtx,
			PlatformOpenAI,
			account.ID,
			account.Concurrency,
			leaseID,
			true,
		)
		if acquireErr != nil || !acquired {
			selection.ReleaseFunc()
			if acquireErr != nil {
				return nil, acquireErr
			}
			return nil, ErrLiveConcurrencyFull
		}

		created, createErr := s.createUpstreamLiveCall(setupCtx, account, request, attestation)
		selection.ReleaseFunc()
		if createErr != nil {
			s.releaseLiveAccountLease(account.ID, leaseID)
			if !s.shouldFailoverLiveCreateError(createErr) {
				return nil, createErr
			}
			excluded[account.ID] = struct{}{}
			lastErr = createErr
			continue
		}

		now := time.Now()
		model := strings.TrimSpace(gjson.GetBytes(request.Session, "model").String())
		if model == "" {
			model = "gpt-live"
		}
		record := &LiveCallRecord{
			CallID:                created.CallID,
			CallHash:              hashLiveCallID(created.CallID),
			AccountID:             account.ID,
			APIKeyID:              identity.APIKeyID,
			UserID:                identity.UserID,
			GroupID:               liveGroupID(identity.GroupID),
			SubscriptionID:        liveGroupID(identity.SubscriptionID),
			LeaseID:               leaseID,
			Model:                 model,
			CreatedAt:             now,
			ExpiresAt:             now.Add(s.liveMaxSessionDuration()),
			Controller:            LiveControllerPending,
			UserAgent:             identity.UserAgent,
			IPAddress:             identity.IPAddress,
			InboundEndpoint:       identity.InboundEndpoint,
			AttestationCiphertext: attestationCiphertext,
		}
		mappingTTL := s.liveMaxSessionDuration() + 5*time.Minute
		saveCtx, saveCancel := liveRedisContext(setupCtx)
		saveErr := store.SaveLiveCall(saveCtx, record, mappingTTL)
		saveCancel()
		if saveErr != nil {
			s.releaseLiveAccountLease(account.ID, leaseID)
			return nil, fmt.Errorf("save live call mapping: %w", saveErr)
		}
		tenantOwned = false
		created.Account = account
		if _, loaded := s.liveObserverPending.LoadOrStore(record.CallHash, struct{}{}); loaded {
			s.finalizeLiveCall(record)
			return nil, ErrLiveConcurrencyFull
		}
		if !s.dispatchLiveObserverWithReservedPermit(runtimeCtx, record.CallHash) {
			s.liveObserverPending.Delete(record.CallHash)
			s.finalizeLiveCall(record)
			return nil, ErrLiveConcurrencyFull
		}
		observerPermitOwned = false
		return created, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrLiveUnavailable
}

func (s *OpenAIGatewayService) shouldFailoverLiveCreateError(err error) bool {
	var upstreamErr *UpstreamFailoverError
	if !errors.As(err, &upstreamErr) {
		// 凭证读取和网络传输错误都可能只影响当前账号或代理。
		return true
	}
	return s.shouldFailoverOpenAIUpstreamResponse(
		upstreamErr.StatusCode,
		"",
		upstreamErr.ResponseBody,
	)
}

func (s *OpenAIGatewayService) createUpstreamLiveCall(
	ctx context.Context,
	account *Account,
	request *LiveCallRequest,
	attestation string,
) (*LiveCallCreated, error) {
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		logLiveCreateStageFailure(ctx, account.ID, "access_token", err)
		return nil, err
	}
	session := request.Session
	strongIsolation := account.IsOpenAIUpstreamStrongIsolationEnabled()
	if strongIsolation {
		isolated, _, isolationErr := applyOpenAIUpstreamStrongIsolationWSBody(session, true)
		if isolationErr != nil {
			return nil, fmt.Errorf("apply Live upstream strong isolation: %w", isolationErr)
		}
		session = isolated
	}
	body, err := json.Marshal(struct {
		SDP     string          `json:"sdp"`
		Session json.RawMessage `json:"session"`
	}{
		SDP:     request.SDP,
		Session: session,
	})
	if err != nil {
		return nil, err
	}
	reqCtx := WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileOpenAI))
	upstreamReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, chatGPTLiveCallsURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	authHeaders, err := s.buildOpenAIAuthenticationHeaders(ctx, account, token)
	if err != nil {
		logLiveCreateStageFailure(ctx, account.ID, "authentication_headers", err)
		return nil, err
	}
	for key, values := range authHeaders {
		for _, value := range values {
			upstreamReq.Header.Add(key, value)
		}
	}
	upstreamReq.Host = "chatgpt.com"
	if err := resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, upstreamReq.Header, account); err != nil {
		logLiveCreateStageFailure(ctx, account.ID, "account_headers", err)
		return nil, err
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "application/sdp")
	upstreamReq.Header.Set(liveAttestationHeader, attestation)
	applyLiveUpstreamIdentityHeaders(upstreamReq.Header)
	if strongIsolation {
		applyOpenAIUpstreamStrongIsolationHeaders(upstreamReq)
	}

	resp, err := s.httpUpstream.Do(upstreamReq, resolveAccountProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		logLiveCreateStageFailure(ctx, account.ID, "upstream_transport", err)
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, liveUpstreamBodyLimit+1))
	if readErr != nil {
		return nil, readErr
	}
	if len(responseBody) > liveUpstreamBodyLimit {
		return nil, errors.New("live upstream response is too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logLiveUpstreamFailure(ctx, account.ID, resp.StatusCode, resp.Header, responseBody)
		return nil, &UpstreamFailoverError{
			StatusCode:      resp.StatusCode,
			ResponseBody:    responseBody,
			ResponseHeaders: resp.Header.Clone(),
		}
	}
	callID, err := liveCallIDFromLocation(resp.Header.Get("Location"))
	if err != nil {
		return nil, err
	}
	return &LiveCallCreated{
		SDP:      responseBody,
		CallID:   callID,
		Location: resp.Header.Get("Location"),
	}, nil
}

func logLiveCreateStageFailure(ctx context.Context, accountID int64, stage string, err error) {
	logger.FromContext(ctx).Warn(
		"OpenAI Live 创建阶段失败",
		zap.Int64("account_id", accountID),
		zap.String("stage", stage),
		zap.String("error_type", fmt.Sprintf("%T", err)),
	)
}

func logLiveUpstreamFailure(
	ctx context.Context,
	accountID int64,
	statusCode int,
	headers http.Header,
	body []byte,
) {
	errorType := strings.TrimSpace(gjson.GetBytes(body, "error.type").String())
	errorCode := strings.TrimSpace(gjson.GetBytes(body, "error.code").String())
	errorMessage := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	if errorType == "" {
		errorType = strings.TrimSpace(gjson.GetBytes(body, "type").String())
	}
	if errorCode == "" {
		errorCode = strings.TrimSpace(gjson.GetBytes(body, "code").String())
	}
	if errorMessage == "" {
		errorMessage = strings.TrimSpace(gjson.GetBytes(body, "message").String())
	}
	if errorMessage == "" {
		errorMessage = strings.TrimSpace(gjson.GetBytes(body, "detail").String())
	}

	logger.FromContext(ctx).Warn(
		"OpenAI Live 上游拒绝请求",
		zap.Int64("account_id", accountID),
		zap.Int("upstream_status_code", statusCode),
		zap.String("upstream_error_type", sanitizeLiveUpstreamLogValue(errorType, 120)),
		zap.String("upstream_error_code", sanitizeLiveUpstreamLogValue(errorCode, 120)),
		zap.String("upstream_error_message", sanitizeLiveUpstreamLogValue(errorMessage, 300)),
		zap.String("upstream_content_type", sanitizeLiveUpstreamLogValue(headers.Get("Content-Type"), 120)),
	)
}

func sanitizeLiveUpstreamLogValue(value string, maxLen int) string {
	return truncateOpenAIWSLogValue(sanitizeUpstreamErrorMessage(value), maxLen)
}

func liveCallIDFromLocation(location string) (string, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return "", errors.New("live upstream response has no Location")
	}
	parsed, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("parse live Location: %w", err)
	}
	callID := strings.TrimSpace(path.Base(strings.TrimSuffix(parsed.Path, "/")))
	if callID == "" || callID == "." || callID == "codex" {
		return "", errors.New("live upstream Location has no call id")
	}
	return callID, nil
}

func applyLiveUpstreamIdentityHeaders(headers http.Header) {
	headers.Set("OpenAI-Alpha", "quicksilver=v2")
	ensureCodexIdentityHeaders(headers)
	enforceCodexIdentityHeaders(headers)
	if strings.TrimSpace(headers.Get("session-id")) == "" {
		headers.Set("session-id", uuid.NewString())
	}
	if strings.TrimSpace(headers.Get("thread-id")) == "" {
		headers.Set("thread-id", uuid.NewString())
	}
	// Realtime/Live 不使用 Responses 的实验头。
	headers.Del("OpenAI-Beta")
}

func (s *OpenAIGatewayService) liveSidebandHeaders(
	ctx context.Context,
	account *Account,
	record *LiveCallRecord,
) (http.Header, error) {
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	headers, err := s.buildOpenAIAuthenticationHeaders(ctx, account, token)
	if err != nil {
		return nil, err
	}
	if err := resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, headers, account); err != nil {
		return nil, err
	}
	attestation, err := s.decryptLiveAttestation(record)
	if err != nil {
		return nil, err
	}
	headers.Set(liveAttestationHeader, attestation)
	applyLiveUpstreamIdentityHeaders(headers)
	if account.IsOpenAIUpstreamStrongIsolationEnabled() {
		applyOpenAIUpstreamStrongIsolationHeaderMap(headers)
	}
	return headers, nil
}

func (s *OpenAIGatewayService) dialLiveSideband(ctx context.Context, record *LiveCallRecord) (liveFrameConn, error) {
	if s == nil || record == nil || s.accountRepo == nil {
		return nil, ErrLiveUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, record.AccountID)
	if err != nil {
		return nil, err
	}
	if account == nil || !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityLive) {
		return nil, ErrLiveUnavailable
	}
	headers, err := s.liveSidebandHeaders(ctx, account, record)
	if err != nil {
		return nil, err
	}
	target := strings.TrimRight(chatGPTLiveSidebandBaseURL, "/") + "/" + url.PathEscape(record.CallID)
	conn, status, _, err := s.getOpenAIWSPassthroughDialer().Dial(ctx, target, headers, resolveAccountProxyURL(account))
	if err != nil {
		return nil, fmt.Errorf("dial live sideband (status %d): %w", status, err)
	}
	raw, ok := conn.(liveFrameConn)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("live sideband transport does not support raw frames")
	}
	return raw, nil
}

func (s *OpenAIGatewayService) GetLiveCallForIdentity(
	ctx context.Context,
	callID string,
	identity LiveCallIdentity,
) (*LiveCallRecord, error) {
	store, err := s.liveStore()
	if err != nil {
		return nil, err
	}
	lookupCtx, lookupCancel := liveRedisContext(ctx)
	record, err := store.GetLiveCall(lookupCtx, hashLiveCallID(callID))
	lookupCancel()
	if err != nil {
		return nil, err
	}
	if record.CallID != callID ||
		record.APIKeyID != identity.APIKeyID ||
		record.UserID != identity.UserID ||
		record.GroupID != liveGroupID(identity.GroupID) {
		return nil, ErrLiveIdentityMismatch
	}
	if record.Controller == LiveControllerClosed {
		return nil, ErrLiveCallNotFound
	}
	return record, nil
}

// ProxyLiveSideband 让认证后的客户端接管控制连接；媒体始终不经过这里。
func (s *OpenAIGatewayService) ProxyLiveSideband(
	ctx context.Context,
	record *LiveCallRecord,
	downstream *coderws.Conn,
) error {
	if record == nil || downstream == nil {
		return ErrLiveCallNotFound
	}
	if !s.acquireLiveProxyPermit(ctx) {
		return ErrLiveConcurrencyFull
	}
	defer s.liveProxyWorkers.Done()
	defer s.releaseLiveProxyPermit()
	runtimeCtx, ok := s.liveRuntimeContext()
	if !ok {
		return ErrLiveUnavailable
	}
	// A persisted call record can outlive its 60-second concurrency lease after
	// a restart or Redis release ambiguity. Never let such a record reclaim a
	// controller without proving both tenant and account leases still exist.
	if !s.refreshLiveLease(record) {
		s.finalizeLiveCall(record)
		return ErrLiveUnavailable
	}
	proxyCtx, cancel := context.WithCancel(ctx)
	stopRuntimeCancel := context.AfterFunc(runtimeCtx, cancel)
	defer stopRuntimeCancel()
	defer cancel()
	store, err := s.liveStore()
	if err != nil {
		return err
	}
	owner := uuid.NewString()
	claimCtx, claimCancel := liveRedisContext(proxyCtx)
	claimed, err := store.ClaimLiveController(claimCtx, record.CallHash, LiveControllerProxy, owner)
	claimCancel()
	if err != nil {
		return err
	}
	if !claimed {
		return ErrLiveControllerChanged
	}

	// observer 轮询到接管状态后会关闭旧控制连接；同一个 call 可重新加入。
	handoffTimer := time.NewTimer(liveObserverPollInterval)
	select {
	case <-proxyCtx.Done():
		handoffTimer.Stop()
		s.releaseLiveControllerBestEffort(store, record.CallHash, owner)
		return context.Cause(proxyCtx)
	case <-handoffTimer.C:
	}
	upstream, err := s.dialLiveSideband(proxyCtx, record)
	if err != nil {
		s.releaseLiveControllerBestEffort(store, record.CallHash, owner)
		s.requeueOrFinalizeLive(record)
		return err
	}
	defer func() { _ = upstream.Close() }()
	downstream.SetReadLimit(openAIWSMessageReadLimitBytes)

	errCh := make(chan error, 2)
	var relayWorkers sync.WaitGroup
	relayWorkers.Add(2)
	go func() {
		defer relayWorkers.Done()
		for {
			messageType, payload, readErr := downstream.Read(proxyCtx)
			if readErr != nil {
				select {
				case errCh <- readErr:
				case <-proxyCtx.Done():
				}
				return
			}
			if writeErr := upstream.WriteFrame(proxyCtx, messageType, payload); writeErr != nil {
				select {
				case errCh <- writeErr:
				case <-proxyCtx.Done():
				}
				return
			}
		}
	}()
	go func() {
		defer relayWorkers.Done()
		for {
			messageType, payload, readErr := upstream.ReadFrame(proxyCtx)
			if readErr != nil {
				select {
				case errCh <- liveSidebandReadError(readErr):
				case <-proxyCtx.Done():
				}
				return
			}
			payload = sanitizeLiveSidebandFrameForClient(messageType, payload)
			if writeErr := downstream.Write(proxyCtx, messageType, payload); writeErr != nil {
				select {
				case errCh <- writeErr:
				case <-proxyCtx.Done():
				}
				return
			}
			if messageType == coderws.MessageText {
				eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
				if eventType == "session.closed" || eventType == "session.ended" {
					select {
					case errCh <- ErrLiveCallNotFound:
					case <-proxyCtx.Done():
					}
					return
				}
			}
		}
	}()

	runErr := s.runLiveController(proxyCtx, record, upstream, errCh)
	cancel()
	relayWorkers.Wait()
	s.releaseLiveControllerBestEffort(store, record.CallHash, owner)
	if liveSessionEnded(runErr) || !time.Now().Before(record.ExpiresAt) {
		s.finalizeLiveCall(record)
		return runErr
	}
	s.requeueOrFinalizeLive(record)
	return runErr
}

// Live sideband text frames are protocol JSON. Keep ordinary control/data
// events byte-identical, but never forward an upstream diagnostic envelope.
// Binary media/control frames remain untouched unless they are JSON errors.
func sanitizeLiveSidebandFrameForClient(messageType coderws.MessageType, payload []byte) []byte {
	if messageType != coderws.MessageText && messageType != coderws.MessageBinary {
		return payload
	}
	if !gjson.ValidBytes(payload) {
		if messageType == coderws.MessageText {
			return safeOpenAIStreamErrorPayload("error")
		}
		return payload
	}
	eventType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "type").String()))
	errorValue := gjson.GetBytes(payload, "error")
	responseErrorValue := gjson.GetBytes(payload, "response.error")
	hasError := eventType == "error" || strings.HasSuffix(eventType, ".error") ||
		strings.HasSuffix(eventType, ".failed") ||
		(errorValue.Exists() && errorValue.Type != gjson.Null) ||
		(responseErrorValue.Exists() && responseErrorValue.Type != gjson.Null)
	if !hasError {
		return payload
	}
	return safeOpenAIStreamErrorPayload("error")
}

// liveSessionEnded 判断控制连接的退出原因是否意味着会话已终结（应 finalize：写
// usage log 并释放租约），而不是可以交给 observer 重连的临时错误。
//
// ErrLiveUnavailable 在控制循环里只会来自租约续租失败。RefreshLiveLease 的 Lua 在
// leaseID 被 GC 后不会重新写入，重连也拿不回并发槽 —— 若按临时错误重试，会话会以
// 约 1 秒一轮的节奏空转到 ExpiresAt，期间持着上游连接却不计入任何并发限制。
func liveSessionEnded(err error) bool {
	return errors.Is(err, ErrLiveCallNotFound) ||
		errors.Is(err, ErrLiveUnavailable) ||
		errors.Is(err, context.DeadlineExceeded)
}

func (s *OpenAIGatewayService) runLiveController(
	ctx context.Context,
	record *LiveCallRecord,
	upstream liveFrameConn,
	errCh <-chan error,
) error {
	refreshTicker := time.NewTicker(liveLeaseRefreshInterval)
	defer refreshTicker.Stop()
	maxTimer := time.NewTimer(time.Until(record.ExpiresAt))
	defer maxTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case err := <-errCh:
			return err
		case <-maxTimer.C:
			closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = upstream.WriteFrame(closeCtx, coderws.MessageText, []byte(`{"type":"session.close"}`))
			cancel()
			return context.DeadlineExceeded
		case <-refreshTicker.C:
			if !s.refreshLiveLease(record) {
				return ErrLiveUnavailable
			}
		}
	}
}

func (s *OpenAIGatewayService) observeLiveCall(ctx context.Context, callHash string) {
	store, err := s.liveStore()
	if err != nil {
		return
	}
	owner := uuid.NewString()
	for {
		claimCtx, claimCancel := liveRedisContext(ctx)
		claimed, claimErr := store.ClaimLiveController(claimCtx, callHash, LiveControllerObserver, owner)
		claimCancel()
		if claimErr == nil && claimed {
			break
		}
		// A timed-out claim may have committed before its response was lost.
		// Accept only our own fenced ownership; another controller ends this worker.
		verificationParent := ctx
		if ctx.Err() != nil {
			verificationParent = context.Background()
		}
		getCtx, getCancel := liveRedisContext(verificationParent)
		record, getErr := store.GetLiveCall(getCtx, callHash)
		getCancel()
		if getErr == nil && record.Controller == LiveControllerObserver && record.ControllerOwner == owner {
			if ctx.Err() != nil {
				// The supervisor finalizes this record after the worker returns.
				// Avoid resuming/finalizing from the release callback as well.
				return
			}
			break
		}
		if getErr == nil || errors.Is(getErr, ErrLiveCallNotFound) {
			return
		}
		if !waitForLiveStoreRetry(ctx) {
			return
		}
	}
	// Return observer ownership to pending on transient Redis/dial/read exits so
	// the bounded supervisor can enqueue a replacement. Owner fencing makes this
	// a no-op after a proxy has already taken over or the call was finalized.
	defer s.releaseLiveControllerBestEffort(store, callHash, owner)
	for {
		getCtx, getCancel := liveRedisContext(ctx)
		record, getErr := store.GetLiveCall(getCtx, callHash)
		getCancel()
		if errors.Is(getErr, ErrLiveCallNotFound) {
			return
		}
		if getErr != nil {
			if !waitForLiveStoreRetry(ctx) {
				return
			}
			continue
		}
		if record.Controller != LiveControllerObserver || record.ControllerOwner != owner {
			return
		}
		if !time.Now().Before(record.ExpiresAt) {
			s.finalizeLiveCall(record)
			return
		}
		upstream, dialErr := s.dialLiveSideband(ctx, record)
		if dialErr != nil {
			if !s.waitForLiveObserverRetry(ctx, record) {
				return
			}
			continue
		}
		runErr := s.runLiveObserverConnection(ctx, record, upstream)
		_ = upstream.Close()
		if errors.Is(runErr, ErrLiveControllerChanged) {
			return
		}
		if liveSessionEnded(runErr) {
			s.finalizeLiveCall(record)
			return
		}
		if !s.waitForLiveObserverRetry(ctx, record) {
			return
		}
	}
}

func (s *OpenAIGatewayService) runLiveObserverConnection(parent context.Context, record *LiveCallRecord, upstream liveFrameConn) error {
	ctx, cancel := context.WithCancel(parent)
	frameCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			messageType, payload, err := upstream.ReadFrame(ctx)
			if err != nil {
				select {
				case errCh <- liveSidebandReadError(err):
				case <-ctx.Done():
				}
				return
			}
			if messageType == coderws.MessageText {
				select {
				case frameCh <- payload:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-readerDone
	}()
	refreshTicker := time.NewTicker(liveLeaseRefreshInterval)
	defer refreshTicker.Stop()
	controllerTicker := time.NewTicker(liveObserverPollInterval)
	defer controllerTicker.Stop()
	maxTimer := time.NewTimer(time.Until(record.ExpiresAt))
	defer maxTimer.Stop()
	store, _ := s.liveStore()
	for {
		select {
		case payload := <-frameCh:
			eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
			if eventType == "session.closed" || eventType == "session.ended" {
				return ErrLiveCallNotFound
			}
		case err := <-errCh:
			return err
		case <-controllerTicker.C:
			pollCtx, pollCancel := liveRedisContext(ctx)
			controller, err := store.GetLiveController(pollCtx, record.CallHash)
			pollCancel()
			if err != nil {
				return err
			}
			if controller != LiveControllerObserver {
				return ErrLiveControllerChanged
			}
		case <-refreshTicker.C:
			if !s.refreshLiveLease(record) {
				return ErrLiveUnavailable
			}
		case <-maxTimer.C:
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = upstream.WriteFrame(closeCtx, coderws.MessageText, []byte(`{"type":"session.close"}`))
			closeCancel()
			return context.DeadlineExceeded
		}
	}
}

func (s *OpenAIGatewayService) waitForLiveObserverRetry(ctx context.Context, record *LiveCallRecord) bool {
	if !waitForLiveStoreRetry(ctx) {
		return false
	}
	store, err := s.liveStore()
	if err != nil {
		return false
	}
	pollCtx, pollCancel := liveRedisContext(ctx)
	controller, err := store.GetLiveController(pollCtx, record.CallHash)
	pollCancel()
	if err != nil && !errors.Is(err, ErrLiveCallNotFound) {
		return true
	}
	// 过期不在此处判定：返回 true 让调用方回到循环顶部的过期分支，由它 finalize
	// （写 usage log + 释放租约）。在这里直接返回 false 会让会话静默结束、不留记录。
	return err == nil && controller == LiveControllerObserver
}

func waitForLiveStoreRetry(ctx context.Context) bool {
	timer := time.NewTimer(liveObserverStoreRetryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *OpenAIGatewayService) refreshLiveLease(record *LiveCallRecord) bool {
	cache, err := s.liveConcurrencyCache()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveRedisOperationTimeout)
	defer cancel()
	tenantRefreshed, err := cache.RefreshLiveTenantLease(ctx, record.UserID, record.APIKeyID, record.LeaseID)
	if err != nil || !tenantRefreshed {
		return false
	}
	accountRefreshed, err := cache.RefreshLiveAccountLease(ctx, record.AccountID, record.LeaseID)
	return err == nil && accountRefreshed
}

func (s *OpenAIGatewayService) releaseLiveTenantLease(userID, apiKeyID int64, leaseID string) {
	cache, err := s.liveConcurrencyCache()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveRedisOperationTimeout)
	err = cache.ReleaseLiveTenantLease(ctx, userID, apiKeyID, leaseID)
	cancel()
	if err != nil && s.concurrencyService != nil {
		s.concurrencyService.retryConcurrencySlotReleaseInBackground("live tenant", userID, leaseID, func(ctx context.Context) error {
			return cache.ReleaseLiveTenantLease(ctx, userID, apiKeyID, leaseID)
		})
	}
}

func (s *OpenAIGatewayService) releaseLiveAccountLease(accountID int64, leaseID string) {
	cache, err := s.liveConcurrencyCache()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveRedisOperationTimeout)
	err = cache.ReleaseLiveAccountLease(ctx, accountID, leaseID)
	cancel()
	if err != nil && s.concurrencyService != nil {
		s.concurrencyService.retryConcurrencySlotReleaseInBackground("live account", accountID, leaseID, func(ctx context.Context) error {
			return cache.ReleaseLiveAccountLease(ctx, accountID, leaseID)
		})
	}
}

func (s *OpenAIGatewayService) releaseLiveLease(accountID, userID, apiKeyID int64, leaseID string) {
	s.releaseLiveAccountLease(accountID, leaseID)
	s.releaseLiveTenantLease(userID, apiKeyID, leaseID)
}

func (s *OpenAIGatewayService) finalizeLiveCall(record *LiveCallRecord) {
	if record == nil {
		return
	}
	store, err := s.liveStore()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveRedisOperationTimeout)
	_, markErr := store.MarkLiveCallClosed(ctx, record.CallHash, liveClosedRecordTTL)
	cancel()
	if markErr != nil && s.concurrencyService != nil {
		s.concurrencyService.retryConcurrencySlotReleaseInBackground("live finalize marker", record.AccountID, record.CallHash, func(ctx context.Context) error {
			_, retryErr := store.MarkLiveCallClosed(ctx, record.CallHash, liveClosedRecordTTL)
			return retryErr
		})
	}
	// Lease release and audit persistence are independently idempotent. They
	// must not be skipped when the close marker response is lost after Redis
	// executed the script, or when another finalizer won the marker race.
	s.releaseLiveLease(record.AccountID, record.UserID, record.APIKeyID, record.LeaseID)
	if s.usageLogRepo == nil {
		return
	}
	duration := int(time.Since(record.CreatedAt).Milliseconds())
	if duration < 0 {
		duration = 0
	}
	inboundEndpoint := record.InboundEndpoint
	upstreamEndpoint := "/backend-api/codex/realtime/calls"
	userAgent := record.UserAgent
	ipAddress := record.IPAddress
	billingType := int8(BillingTypeBalance)
	if record.SubscriptionID > 0 {
		billingType = BillingTypeSubscription
	}
	writeUsageLogBestEffort(context.Background(), s.usageLogRepo, &UsageLog{
		UserID:           record.UserID,
		APIKeyID:         record.APIKeyID,
		AccountID:        record.AccountID,
		RequestID:        record.CallHash,
		Model:            record.Model,
		RequestedModel:   record.Model,
		GroupID:          liveOptionalID(record.GroupID),
		SubscriptionID:   liveOptionalID(record.SubscriptionID),
		RateMultiplier:   1,
		BillingType:      billingType,
		RequestType:      RequestTypeLive,
		DurationMs:       &duration,
		UserAgent:        &userAgent,
		IPAddress:        &ipAddress,
		InboundEndpoint:  &inboundEndpoint,
		UpstreamEndpoint: &upstreamEndpoint,
		CreatedAt:        record.CreatedAt,
	}, "service.openai_live")
}

func (s *OpenAIGatewayService) finalizeLiveCallByHash(callHash string) {
	store, err := s.liveStore()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveRedisOperationTimeout)
	record, err := store.GetLiveCall(ctx, callHash)
	cancel()
	if err == nil {
		s.finalizeLiveCall(record)
	}
}
