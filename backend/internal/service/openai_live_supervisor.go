package service

import (
	"context"
	"time"
)

func (s *OpenAIGatewayService) initLiveObserverSupervisor() {
	workers := 128
	queueSize := 1024
	proxyConnections := 512
	if s != nil && s.cfg != nil {
		if configured := s.cfg.Gateway.Live.ObserverWorkers; configured > 0 {
			workers = configured
		}
		if configured := s.cfg.Gateway.Live.ObserverQueueSize; configured > 0 {
			queueSize = configured
		}
		if configured := s.cfg.Gateway.Live.ProxyConnections; configured > 0 {
			proxyConnections = configured
		}
	}
	workers = min(workers, 4096)
	queueSize = min(queueSize, 65536)
	proxyConnections = min(proxyConnections, 16384)
	s.liveObserverContext, s.liveObserverCancel = context.WithCancel(context.Background())
	s.liveObserverQueue = make(chan string, queueSize)
	// One observer owns the sideband connection for the lifetime of a call.
	// Bound accepted calls by worker capacity so queued calls cannot silently
	// outlive their 60-second persistent leases before a worker reaches them.
	s.liveObserverPermits = make(chan struct{}, workers)
	s.liveProxyPermits = make(chan struct{}, proxyConnections)
	s.liveLifecycleMu.Lock()
	defer s.liveLifecycleMu.Unlock()
	if s.liveObserverStopped.Load() {
		s.liveObserverCancel()
		return
	}
	for range workers {
		s.liveObserverWorkers.Add(1)
		go func() {
			defer s.liveObserverWorkers.Done()
			for {
				select {
				case <-s.liveObserverContext.Done():
					return
				case callHash := <-s.liveObserverQueue:
					s.observeLiveCall(s.liveObserverContext, callHash)
					stopping := s.liveObserverContext.Err() != nil || s.liveObserverStopped.Load()
					if stopping {
						// Stop waits for observer workers while Redis and the
						// concurrency Cells are still available. Finalize an
						// in-flight call before removing its pending marker so it
						// cannot disappear from StopLiveSessions' fallback scan.
						s.finalizeLiveCallByHash(callHash)
					}
					s.liveObserverPending.Delete(callHash)
					<-s.liveObserverPermits
					if !stopping {
						s.resumeLiveObserverIfPending(callHash)
					}
				}
			}
		}()
	}
}

func (s *OpenAIGatewayService) liveRuntimeContext() (context.Context, bool) {
	if s == nil || s.liveObserverStopped.Load() {
		return nil, false
	}
	s.liveObserverSupervisorOnce.Do(s.initLiveObserverSupervisor)
	if s.liveObserverContext == nil || s.liveObserverContext.Err() != nil {
		return nil, false
	}
	return s.liveObserverContext, true
}

func (s *OpenAIGatewayService) beginLiveCreate() (context.Context, bool) {
	runtimeCtx, ok := s.liveRuntimeContext()
	if !ok {
		return nil, false
	}
	s.liveLifecycleMu.Lock()
	defer s.liveLifecycleMu.Unlock()
	if s.liveObserverStopped.Load() || runtimeCtx.Err() != nil {
		return nil, false
	}
	s.liveCreateWorkers.Add(1)
	return runtimeCtx, true
}

func (s *OpenAIGatewayService) enqueueLiveObserver(callHash string) bool {
	if s == nil || callHash == "" {
		return false
	}
	runtimeCtx, ok := s.liveRuntimeContext()
	if !ok || s.liveObserverQueue == nil || s.liveObserverPermits == nil {
		return false
	}
	if _, loaded := s.liveObserverPending.LoadOrStore(callHash, struct{}{}); loaded {
		return true
	}
	if !s.reserveLiveObserverPermit(runtimeCtx) {
		s.liveObserverPending.Delete(callHash)
		return false
	}
	if s.dispatchLiveObserverWithReservedPermit(runtimeCtx, callHash) {
		return true
	}
	s.liveObserverPending.Delete(callHash)
	<-s.liveObserverPermits
	return false
}

// reserveLiveObserverPermit is deliberately non-blocking. Live calls keep an
// observer connection for their full lifetime, so waiting here would consume
// tenant/account leases before the process has capacity to own the call.
func (s *OpenAIGatewayService) reserveLiveObserverPermit(runtimeCtx context.Context) bool {
	if s == nil || runtimeCtx == nil || s.liveObserverPermits == nil {
		return false
	}
	select {
	case s.liveObserverPermits <- struct{}{}:
	default:
		return false
	}
	if runtimeCtx.Err() != nil || s.liveObserverStopped.Load() {
		<-s.liveObserverPermits
		return false
	}
	return true
}

// dispatchLiveObserverWithReservedPermit transfers an already-held permit to
// an observer worker. The caller retains and must release it on failure.
func (s *OpenAIGatewayService) dispatchLiveObserverWithReservedPermit(runtimeCtx context.Context, callHash string) bool {
	if s == nil || runtimeCtx == nil || callHash == "" || s.liveObserverQueue == nil {
		return false
	}
	select {
	case <-runtimeCtx.Done():
		return false
	case s.liveObserverQueue <- callHash:
		return true
	default:
		return false
	}
}

// resumeLiveObserverIfPending closes the handoff race where a proxy releases
// ownership before the old observer worker clears its local pending marker.
func (s *OpenAIGatewayService) resumeLiveObserverIfPending(callHash string) {
	store, err := s.liveStore()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveRedisOperationTimeout)
	defer cancel()
	record, err := store.GetLiveCall(ctx, callHash)
	if err != nil || record == nil || record.Controller == LiveControllerClosed {
		return
	}
	if !time.Now().Before(record.ExpiresAt) {
		s.finalizeLiveCall(record)
		return
	}
	if record.Controller == LiveControllerPending && !s.enqueueLiveObserver(callHash) {
		s.finalizeLiveCall(record)
	}
}

func (s *OpenAIGatewayService) acquireLiveProxyPermit(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	runtimeCtx, ok := s.liveRuntimeContext()
	if !ok || s.liveProxyPermits == nil {
		return false
	}
	select {
	case s.liveProxyPermits <- struct{}{}:
	case <-ctx.Done():
		return false
	case <-runtimeCtx.Done():
		return false
	default:
		return false
	}
	s.liveLifecycleMu.Lock()
	defer s.liveLifecycleMu.Unlock()
	if s.liveObserverStopped.Load() {
		<-s.liveProxyPermits
		return false
	}
	s.liveProxyWorkers.Add(1)
	return true
}

func (s *OpenAIGatewayService) releaseLiveProxyPermit() {
	if s != nil && s.liveProxyPermits != nil {
		<-s.liveProxyPermits
	}
}

// StopLiveSessions stops accepting local Live work, cancels all bounded
// observer/proxy workers, then finalizes remaining records while Redis is live.
func (s *OpenAIGatewayService) StopLiveSessions() {
	if s == nil || s.liveObserverStopped.Swap(true) {
		return
	}
	s.liveObserverSupervisorOnce.Do(s.initLiveObserverSupervisor)
	if s.liveObserverCancel != nil {
		s.liveObserverCancel()
	}
	// Synchronize with the last worker registration before waiting.
	s.liveLifecycleMu.Lock()
	s.liveLifecycleMu.Unlock()
	s.liveCreateWorkers.Wait()
	s.liveObserverWorkers.Wait()
	s.liveProxyWorkers.Wait()
	s.liveObserverPending.Range(func(key, _ any) bool {
		callHash, _ := key.(string)
		if callHash != "" {
			s.finalizeLiveCallByHash(callHash)
		}
		return true
	})
}

func (s *OpenAIGatewayService) requeueOrFinalizeLive(record *LiveCallRecord) {
	if record == nil {
		return
	}
	if !s.enqueueLiveObserver(record.CallHash) {
		s.finalizeLiveCall(record)
	}
}
