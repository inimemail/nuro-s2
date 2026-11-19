package service

import (
	"math"
	"time"
)

// Attach once before ranking; the comparator only reads stable grades, so
// different latency dimensions cannot create a non-transitive sort order.
func (s *OpenAIGatewayService) decorateExperience(pool []openAIAccountCandidateScore, req OpenAIAccountScheduleRequest, now time.Time, policy adaptiveTTFTSwitchPolicy) {
	for i := range pool {
		pool[i].experience = nil
		pool[i].experienceDegraded = false
		pool[i].experienceRecovery = false
		pool[i].experienceFloor = 0
	}
	if s == nil || req.Experience == nil || req.requiresCompactCapability() || req.RequiredImageCapability != "" || !s.OpenAIStreamExperienceEnabled() {
		return
	}
	x := s.experienceStore()
	if x == nil {
		return
	}
	keys := make([]openAIExperienceKey, len(pool))
	for i := range pool {
		a := pool[i].account
		if a == nil || a.Platform != PlatformOpenAI {
			continue
		}
		transport := req.Experience.Transport
		if transport == OpenAIUpstreamTransportAny {
			if s.openaiWSResolver == nil {
				continue
			}
			transport = s.openaiWSResolver.Resolve(a).Transport
		}
		if transport == OpenAIUpstreamTransportResponsesWebsocketV2Ingress {
			// WS ingress may bridge to HTTP after payload normalization. Its
			// replay/size metadata cannot be predicted safely at account selection.
			// Keep those paths neutral rather than borrowing a WS-only baseline.
			if a.IsOpenAIUpstreamStrongIsolationEnabled() || s.getOpenAIWSProtocolResolver().Resolve(a).Transport != OpenAIUpstreamTransportResponsesWebsocketV2 || s.experienceMayUseHTTPBridge(a, req.Experience.Request) {
				continue
			}
			transport = OpenAIUpstreamTransportResponsesWebsocketV2
		}
		if transport != OpenAIUpstreamTransportHTTPSSE && transport != OpenAIUpstreamTransportResponsesWebsocketV2 {
			continue
		}
		requestedModel := req.Experience.Model
		if requestedModel == "" {
			requestedModel = req.RequestedModel
		}
		model := normalizeOpenAIModelForUpstream(a, resolveOpenAIAccountUpstreamModelForRequest(a, requestedModel, false, ""))
		// Raw relays preserve the body model; ordinary account mappings and
		// OAuth normalization belong only to the transformed forwarding paths.
		if transport == OpenAIUpstreamTransportHTTPSSE && a.IsOpenAIPassthroughEnabled() {
			model = req.Experience.WireModel
			if model == "" {
				model = requestedModel
			}
		} else if transport == OpenAIUpstreamTransportResponsesWebsocketV2 && s.cfg != nil && s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled && a.ResolveOpenAIResponsesWebSocketV2Mode(s.cfg.Gateway.OpenAIWS.IngressModeDefault) == OpenAIWSIngressModePassthrough {
			model = requestedModel
		}
		keys[i] = openAIExperienceKey{a.ID, model, transport, req.Experience.Request}
		pool[i].experience = x.snapshot(keys[i], now, policy.sampleFreshness)
		pool[i].experienceFloor = s.experienceGapFloor()
	}
	// Compare to the best proven baseline in each exact cohort. A single
	// full-stream metric makes this linear in pool size, not pairwise O(n²).
	type cohort struct {
		Model     string
		Transport OpenAIUpstreamTransport
		Request   OpenAIExperienceRequest
		OAuth     bool
	}
	best := make(map[cohort]int)
	baselines := make([]int64, len(pool))
	cohorts := make([]cohort, len(pool))
	for i, c := range pool {
		if c.account == nil || len(c.experience) < 5 {
			continue
		}
		sample := c.experience[0]
		key := cohort{sample.Model, sample.Transport, sample.Request, c.account.IsOAuth()}
		cohorts[i] = key
		baselines[i] = experienceBaseline(c.experience)
		if j, ok := best[key]; !ok || baselines[i] < baselines[j] {
			best[key] = i
		}
	}
	for i := range pool {
		if len(pool[i].experience) < 5 {
			continue
		}
		j := best[cohorts[i]]
		if i != j && experienceWorse(pool[i].experience, pool[j].experience, s.experienceGapFloor()) {
			pool[i].experienceDegraded = true
		}
	}

	type groupEvidence struct {
		count    int
		minimum  int64
		degraded bool
	}
	groups := make(map[cohort]groupEvidence, len(best))
	for i, c := range pool {
		if len(c.experience) < 5 {
			continue
		}
		g := groups[cohorts[i]]
		severity := experienceSeverity(c.experience)
		if g.count == 0 || severity < g.minimum {
			g.minimum = severity
		}
		g.count++
		g.degraded = g.degraded || c.experienceDegraded
		groups[cohorts[i]] = g
	}
	for key, g := range groups {
		if g.count >= 2 && !g.degraded && g.minimum >= s.experienceGapFloor() {
			x.noImprovement(g.count, key.Model)
		}
	}

	// Bounded recovery uses actual future traffic, never duplicate paid probes.
	// An account cannot be promoted more than once per five minutes and the
	// entire process admits at most one recovery promotion per 20 evaluations.
	x.mu.Lock()
	defer x.mu.Unlock()
	x.probeTurns++
	if x.probeTurns%20 != 0 || now.Sub(x.probeAt) < time.Minute {
		return
	}
	for i := range pool {
		if !pool[i].experienceDegraded {
			continue
		}
		e := x.entries[keys[i]]
		if e != nil && now.Sub(e.Probe) >= 5*time.Minute {
			e.Probe = now
			x.probeAt = now
			pool[i].experienceRecovery = true
			break
		}
	}
}

func (s *OpenAIGatewayService) experienceMayUseHTTPBridge(a *Account, request OpenAIExperienceRequest) bool {
	if !s.openAIWSHTTPBridgeEnabled() || request.Continuation {
		return false
	}
	// Passthrough returns before the dynamic-size bridge in the forwarding path.
	if s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled && a.ResolveOpenAIResponsesWebSocketV2Mode(s.cfg.Gateway.OpenAIWS.IngressModeDefault) == OpenAIWSIngressModePassthrough {
		return false
	}
	// Only a size bucket that could reach the bridge threshold is uncertain.
	// Normal WS requests continue using the signal with the default bridge on.
	upper := [...]int64{16 << 10, 64 << 10, 256 << 10, 1 << 20}
	return request.InputBucket < 0 || request.InputBucket >= len(upper) || upper[request.InputBucket] >= s.openAIWSHTTPBridgeThresholdBytes()
}

func experienceProfile(c openAIAccountCandidateScore) adaptiveAccountHealthProfile {
	return adaptiveAccountHealthProfile{account: c.account, errorRate: c.errorRate, errorSamples: c.sampleCount, p50: c.ttft, p90: c.ttftP90, hasTTFT: c.hasTTFT, ttftSamples: c.ttftSampleCount, errorUpdated: c.errorUpdated, ttftUpdated: c.ttftUpdated, experience: c.experience, experienceDegraded: c.experienceDegraded, experienceRecovery: c.experienceRecovery, experienceFloor: c.experienceFloor}
}

func adaptiveExperienceCostTier(profiles []adaptiveAccountHealthProfile, selected, eligible []int, now time.Time) []int {
	if len(selected) == 0 {
		return selected
	}
	for _, i := range selected {
		if !profiles[i].experienceDegraded || profiles[i].experienceRecovery || len(profiles[i].experience) < 5 {
			return selected
		}
	}
	baseRate := adaptiveHighestMultiplier(profiles, selected, now)
	reference := profiles[selected[0]]
	weakest := int64(math.MaxInt64)
	floor := int64(0)
	for _, i := range selected {
		if !sameExperienceCohort(reference.experience, profiles[i].experience) || reference.account.IsOAuth() != profiles[i].account.IsOAuth() {
			return selected
		}
		severity := experienceSeverity(profiles[i].experience)
		if severity < weakest {
			weakest = severity
		}
		floor = experienceMax(floor, profiles[i].experienceFloor)
	}
	bestRate := math.MaxFloat64
	var improved []int
	for _, j := range eligible {
		p := profiles[j]
		rate, known := accountEffectiveUpstreamMultiplier(p.account, now)
		if !known || rate <= baseRate+accountHealthCostTierEpsilon || rate > bestRate || p.experienceDegraded || reference.account.IsOAuth() != p.account.IsOAuth() || !sameExperienceCohort(reference.experience, p.experience) {
			continue
		}
		if weakest < experienceThreshold(p.experience, floor) {
			continue
		}
		if rate < bestRate {
			bestRate = rate
			improved = nil
		}
		improved = append(improved, j)
	}

	if len(improved) == 0 {
		return selected
	}
	// Keep all accounts in the chosen exact layer as capacity fallbacks; ranking
	// already puts verified smooth accounts ahead of degraded ones.
	var tier []int
	for _, i := range eligible {
		rate, known := accountEffectiveUpstreamMultiplier(profiles[i].account, now)
		if known && math.Abs(rate-bestRate) <= accountHealthCostTierEpsilon {
			tier = append(tier, i)
		}
	}
	return tier
}

func experienceMax(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func experienceGrade(c openAIAccountCandidateScore) int {
	if c.experienceRecovery {
		return -1
	}
	if c.experienceDegraded {
		return 1
	}
	return 0
}
