package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

var experienceTestBody = []byte(`{"model":"gpt-6.1-sol","reasoning":{"effort":"high"},"service_tier":"default","input":"hello"}`)

func experienceEvent(t *openAIExperienceTracker, kind, delta string) {
	payload, _ := json.Marshal(map[string]string{"type": kind, "delta": delta})
	t.observe(payload)
}
func TestOpenAIExperienceFullStreamAndReasoning(t *testing.T) {
	x := newOpenAIExperienceTracker(experienceTestBody)
	for i := 0; i < 20; i++ {
		x.advance(5 * time.Second)
		experienceEvent(x, "response.reasoning_text.delta", "thinking")
	}
	require.Nil(t, x.snapshot().FirstReadyMS)
	require.Equal(t, int64(5000), x.snapshot().MaxGapMS)
	experienceEvent(x, "response.output_text.delta", "first")
	x.advance(15 * time.Second)
	experienceEvent(x, "response.transport_progress.delta", " ")
	x.advance(15 * time.Second)
	experienceEvent(x, "response.output_text.delta", "second")
	experienceEvent(x, "response.completed", "")
	sample := x.snapshot()
	require.True(t, sample.Complete)
	require.Equal(t, int64(100000), *sample.FirstReadyMS)
	require.Equal(t, int64(30000), sample.MaxGapMS)
	require.Equal(t, 1, sample.LongGaps)
	x.advance(time.Hour)
	require.Equal(t, sample.WaitMS, x.snapshot().WaitMS, "idle after completion must not count")
}

func TestOpenAIExperienceInterleavedToolsAndNeutralTerminals(t *testing.T) {
	x := newOpenAIExperienceTracker(experienceTestBody)
	for _, id := range []string{"a", "b"} {
		x.observe([]byte(fmt.Sprintf(`{"type":"response.output_item.added","item":{"id":%q,"type":"function_call"}}`, id)))
	}
	x.observe([]byte(`{"type":"response.output_item.done","item":{"id":"a"}}`))
	x.advance(30 * time.Second)
	experienceEvent(x, "response.function_call_arguments.delta", "{}")
	require.Equal(t, int64(30000), x.snapshot().ToolGapMS)
	for _, terminal := range []string{"response.failed", "response.incomplete", "response.cancelled", "response.canceled"} {
		x := newOpenAIExperienceTracker(experienceTestBody)
		experienceEvent(x, terminal, "")
		require.False(t, x.snapshot().Complete)
	}
	require.False(t, newOpenAIExperienceTracker(experienceTestBody).snapshot().Complete)
}

type experienceSource struct {
	chunks []string
	clock  *time.Time
	wait   time.Duration
}

func (r *experienceSource) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	*r.clock = r.clock.Add(r.wait)
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}
func (r *experienceSource) Close() error { return nil }
func TestOpenAIExperienceReaderPreservesBytesAndExcludesDownstream(t *testing.T) {
	clock := time.Unix(1, 0)
	chunks := []string{"data: {\r\n", `data: "type":"response.output_text.delta","delta":"a"}` + "\r\n\r\n", "data: {\"type\":\"response.completed\"}\n\n"}
	want := strings.Join(chunks, "")
	source := &experienceSource{append([]string(nil), chunks...), &clock, time.Second}
	tracker := newOpenAIExperienceTracker(experienceTestBody)
	r := &openAIExperienceReader{ReadCloser: source, tracker: tracker, now: func() time.Time { return clock }}
	var output strings.Builder
	p := make([]byte, 4096)
	for {
		n, err := r.Read(p)
		_, writeErr := output.Write(p[:n])
		require.NoError(t, writeErr)
		clock = clock.Add(100 * time.Second)
		if err != nil {
			require.ErrorIs(t, err, io.EOF)
			break
		}
	}
	require.Equal(t, want, output.String())
	sample := tracker.snapshot()
	require.True(t, sample.Complete)
	require.Equal(t, int64(3000), sample.WaitMS)
	require.Equal(t, int64(1000), sample.MaxGapMS)
	oversized := "data: " + strings.Repeat("x", 1<<20) + "\n\ndata: {\"type\":\"response.completed\"}\n\n"
	r = &openAIExperienceReader{ReadCloser: io.NopCloser(strings.NewReader(oversized)), tracker: newOpenAIExperienceTracker(experienceTestBody)}
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, oversized, string(out))
	require.True(t, r.invalid.Load())
	require.False(t, r.tracker.snapshot().Complete)
}

func experienceTestSample(gap int64) OpenAIStreamExperienceSample {
	return OpenAIStreamExperienceSample{Request: experienceRequest(experienceTestBody), Model: "gpt-6.1-sol", Transport: OpenAIUpstreamTransportHTTPSSE, MaxGapMS: gap, WaitMS: gap + 5000, Complete: true, AtMS: time.Now().UnixMilli()}
}
func experienceTestSamples(gap int64, n int) []OpenAIStreamExperienceSample {
	var out []OpenAIStreamExperienceSample
	for i := 0; i < n; i++ {
		out = append(out, experienceTestSample(gap))
	}
	return out
}
func experienceTestPool(t *testing.T, rates ...float64) (*OpenAIGatewayService, *defaultOpenAIAccountScheduler, []openAIAccountCandidateScore, OpenAIAccountScheduleRequest) {
	t.Helper()
	s := &OpenAIGatewayService{}
	t.Cleanup(s.closeExperience)
	scheduler := &defaultOpenAIAccountScheduler{service: s, stats: newOpenAIAccountRuntimeStats()}
	var pool []openAIAccountCandidateScore
	for i, rate := range rates {
		a := withOpenAIUpstreamProbeMultiplier(&Account{ID: int64(i + 1), Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, rate, time.Now().Add(time.Hour))
		pool = append(pool, openAIAccountCandidateScore{account: a, loadInfo: &AccountLoadInfo{}, sampleCount: 5, ttftSampleCount: 5, ttft: 1000, ttftP90: 2000, hasTTFT: true})
	}
	req := OpenAIAccountScheduleRequest{Experience: &openAIExperienceRoute{Request: experienceRequest(experienceTestBody), Transport: OpenAIUpstreamTransportHTTPSSE}, RequestedModel: "gpt-6.1-sol", AccountSchedulingStrategy: AccountSchedulingStrategyHealthFirst}
	return s, scheduler, pool, req
}
func experienceSeed(s *OpenAIGatewayService, account *Account, gap int64, n int) {
	for i := 0; i < n; i++ {
		sample := experienceTestSample(gap)
		s.RecordOpenAIStreamExperience(account, OpenAIUpstreamTransportHTTPSSE, &sample, fmt.Sprintf("%d-%d-%d", account.ID, gap, i))
	}
}

func TestOpenAIExperienceRepeatedEvidenceAndCohorts(t *testing.T) {
	fast := experienceTestSamples(5000, 5)
	slow := experienceTestSamples(40000, 5)
	require.True(t, experienceWorse(slow, fast, 20000))
	require.False(t, experienceWorse(fast, slow, 20000))
	require.False(t, experienceWorse(slow[:4], fast, 20000))
	require.False(t, experienceWorse(slow, slow, 20000))
	for _, change := range []func(*OpenAIStreamExperienceSample){func(s *OpenAIStreamExperienceSample) { s.Model = "other" }, func(s *OpenAIStreamExperienceSample) { s.Request.Effort = "low" }, func(s *OpenAIStreamExperienceSample) { s.Request.InputBucket++ }, func(s *OpenAIStreamExperienceSample) { s.Request.ServiceTier = "flex" }, func(s *OpenAIStreamExperienceSample) { s.Transport = OpenAIUpstreamTransportResponsesWebsocketV2 }, func(s *OpenAIStreamExperienceSample) { s.Request.Continuation = true }, func(s *OpenAIStreamExperienceSample) { s.Request.HasTools = true }} {
		b := experienceTestSamples(5000, 5)
		for i := range b {
			change(&b[i])
		}
		require.False(t, experienceWorse(slow, b, 20000))
	}
	oneOff := experienceTestSamples(5000, 5)
	oneOff[4].MaxGapMS = 120000
	require.False(t, experienceWorse(oneOff, fast, 20000))
	recovered := append(experienceTestSamples(40000, 5), experienceTestSamples(5000, 3)...)
	require.False(t, experienceWorse(recovered, fast, 20000), "three recent good requests remove the penalty")
}

func TestOpenAIExperienceRecentSlowBaselineCannotProveImprovement(t *testing.T) {
	for _, strategy := range []string{AccountSchedulingStrategyHealthFirst, AccountSchedulingStrategyHealthCostBalanced} {
		t.Run(strategy, func(t *testing.T) {
			s, scheduler, pool, req := experienceTestPool(t, 0.05, 0.06)
			req.AccountSchedulingStrategy = strategy
			req.StickyAccountID = 1
			experienceSeed(s, pool[0].account, 40000, 5)
			// This account used to be smooth but four of its latest five
			// observations now stall as much as the cheaper account.
			for i := 0; i < 10; i++ {
				gap := int64(5000)
				if i >= 6 {
					gap = 40000
				}
				sample := experienceTestSample(gap)
				sample.AtMS = time.Now().Add(time.Duration(i-10) * time.Second).UnixMilli()
				s.RecordOpenAIStreamExperience(pool[1].account, OpenAIUpstreamTransportHTTPSSE, &sample, fmt.Sprintf("baseline-deteriorated-%d", i))
			}
			ordered := scheduler.buildHealthFirstSelectionOrder(pool, req)
			filtered := filterOpenAIAdaptiveCandidateScoresToPolicyTier(ordered, strategy, time.Now())
			require.Equal(t, int64(1), filtered[0].account.ID, "old fast samples cannot justify moving to an account that is now equally slow")
			for _, candidate := range ordered {
				require.False(t, candidate.experienceDegraded, "there is no measured recent improvement")
			}
		})
	}
}

func TestOpenAIExperienceFindsRecentlySmoothReference(t *testing.T) {
	s, scheduler, pool, req := experienceTestPool(t, 0.05, 0.05, 0.05)
	for i := 0; i < 10; i++ {
		gap := int64(5000)
		if i >= 6 {
			gap = 40000
		}
		sample := experienceTestSample(gap)
		sample.AtMS = time.Now().Add(time.Duration(i-10) * time.Second).UnixMilli()
		s.RecordOpenAIStreamExperience(pool[0].account, OpenAIUpstreamTransportHTTPSSE, &sample, fmt.Sprintf("older-smooth-reference-%d", i))
	}
	experienceSeed(s, pool[1].account, 5000, 5)
	experienceSeed(s, pool[2].account, 40000, 5)
	ordered := scheduler.buildHealthFirstSelectionOrder(pool, req)
	for _, candidate := range ordered {
		if candidate.account.ID == 3 {
			require.True(t, candidate.experienceDegraded, "use the genuinely smooth reference even when another account has old fast observations")
		}
	}
}

func TestOpenAIExperienceHealthFirstStickyAndDisable(t *testing.T) {
	s, scheduler, pool, req := experienceTestPool(t, 0.05, 0.06)
	experienceSeed(s, pool[0].account, 40000, 5)
	experienceSeed(s, pool[1].account, 5000, 5)
	req.StickyAccountID = 1
	ordered := scheduler.buildHealthFirstSelectionOrder(pool, req)
	require.Equal(t, int64(2), ordered[0].account.ID)
	require.Len(t, ordered, 2, "slow account remains a capacity fallback")
	filtered := filterOpenAIAdaptiveCandidateScoresToPolicyTier(ordered, req.AccountSchedulingStrategy, time.Now())
	require.Len(t, filtered, 2)
	off := false
	s.cfg = &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{StreamExperienceEnabled: &off}}}
	ordered = scheduler.buildHealthFirstSelectionOrder(pool, req)
	require.Equal(t, int64(1), ordered[0].account.ID)
}

func TestOpenAIExperienceCostTierAndAllSlow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		gaps   []int64
		counts []int
		want   int64
	}{
		{"cheapest has smooth account", []int64{40000, 5000, 3000}, []int{5, 5, 5}, 2},
		{"whole layer slow proven improvement", []int64{40000, 40000, 5000}, []int{5, 5, 5}, 3},
		{"unknown expensive cannot escalate", []int64{40000, 40000, 5000}, []int{5, 5, 0}, 1},
		{"unknown cheap prevents escalation", []int64{40000, 40000, 5000}, []int{5, 0, 5}, 2},
		{"all slow preserves cheapest", []int64{40000, 40000, 40000}, []int{5, 5, 5}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, scheduler, pool, req := experienceTestPool(t, 0.05, 0.05, 0.06)
			req.AccountSchedulingStrategy = AccountSchedulingStrategyHealthCostBalanced
			for i, gap := range tc.gaps {
				experienceSeed(s, pool[i].account, gap, tc.counts[i])
			}
			ordered := scheduler.buildHealthFirstSelectionOrder(pool, req)
			filtered := filterOpenAIAdaptiveCandidateScoresToPolicyTier(ordered, req.AccountSchedulingStrategy, time.Now())
			require.NotEmpty(t, filtered)
			require.Equal(t, tc.want, filtered[0].account.ID)
		})
	}
}

func TestOpenAIExperienceOAuthStrictAndErrorProtection(t *testing.T) {
	s, scheduler, pool, req := experienceTestPool(t, 0.05, 0.06)
	pool[0].account.Type = AccountTypeOAuth
	experienceSeed(s, pool[0].account, 40000, 5)
	experienceSeed(s, pool[1].account, 5000, 5)
	ordered := scheduler.buildHealthFirstSelectionOrder(pool, req)
	require.Equal(t, int64(1), ordered[0].account.ID, "OAuth remains primary")
	pool[1].account.Type = AccountTypeOAuth
	pool[1].errorRate = 0.8
	ordered = scheduler.buildHealthFirstSelectionOrder(pool, req)
	require.Equal(t, int64(1), ordered[0].account.ID, "experience does not bypass error health")
	// A request without new metadata has precisely the old ranking.
	req.Experience = nil
	ordered = scheduler.buildHealthFirstSelectionOrder(pool, req)
	require.Equal(t, int64(1), ordered[0].account.ID)
}

func TestOpenAIExperienceStoreFreshnessBoundsAndReplay(t *testing.T) {
	s, _, pool, _ := experienceTestPool(t, 0.05)
	account := pool[0].account
	sample := experienceTestSample(30000)
	for i := 0; i < 20; i++ {
		s.RecordOpenAIStreamExperience(account, OpenAIUpstreamTransportHTTPSSE, &sample, "same-event")
	}
	x := s.experienceStore()
	key := openAIExperienceKey{account.ID, sample.Model, OpenAIUpstreamTransportHTTPSSE, sample.Request}
	require.Len(t, x.snapshot(key, time.Now(), time.Minute), 1)
	require.Empty(t, x.snapshot(key, time.Now().Add(2*time.Minute), time.Minute))
	base := sample.AtMS
	for i := 0; i < 30; i++ {
		sample.AtMS = base + int64(i+1)
		s.RecordOpenAIStreamExperience(account, OpenAIUpstreamTransportHTTPSSE, &sample, fmt.Sprint(i))
	}
	require.Len(t, x.snapshot(key, time.Now(), time.Minute), 10)
	invalid := sample
	invalid.MaxGapMS = sample.WaitMS + 1
	s.RecordOpenAIStreamExperience(account, OpenAIUpstreamTransportHTTPSSE, &invalid, "invalid")
	require.Equal(t, uint64(31), s.OpenAIStreamExperienceStats()["samples"])
	x.mu.Lock()
	for i := 0; i < experienceMaxKeys+10; i++ {
		x.entry(openAIExperienceKey{Account: int64(i)})
	}
	require.LessOrEqual(t, len(x.entries), experienceMaxKeys)
	x.mu.Unlock()
}

func TestOpenAIExperienceRedisCrossReplicaAndDeduplication(t *testing.T) {
	mr, client := newOpenAIExperienceRedisTestClient(t)
	s := &OpenAIGatewayService{openaiAccountHealthRedis: client}
	other := &OpenAIGatewayService{openaiAccountHealthRedis: client}
	t.Cleanup(s.closeExperience)
	t.Cleanup(other.closeExperience)
	account := &Account{ID: 3, Platform: PlatformOpenAI}
	sample := experienceTestSample(30000)
	key := openAIExperienceKey{3, sample.Model, OpenAIUpstreamTransportHTTPSSE, sample.Request}
	for i := 0; i < 8; i++ {
		s.RecordOpenAIStreamExperience(account, OpenAIUpstreamTransportHTTPSSE, &sample, "replay")
	}
	require.Eventually(t, func() bool {
		for _, k := range mr.Keys() {
			rows, err := mr.List(k)
			if err == nil && len(rows) == 1 {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)
	x := other.experienceStore()
	x.snapshot(key, time.Now(), time.Minute)
	require.Eventually(t, func() bool { return len(x.snapshot(key, time.Now(), time.Minute)) == 1 }, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, uint64(0), s.OpenAIStreamExperienceStats()["redis_errors"])
}

func TestOpenAIExperienceShutdownAndConcurrency(t *testing.T) {
	s := &OpenAIGatewayService{}
	account := &Account{ID: 1, Platform: PlatformOpenAI}
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				sample := experienceTestSample(30000)
				s.RecordOpenAIStreamExperience(account, OpenAIUpstreamTransportHTTPSSE, &sample, fmt.Sprint(i))
				s.OpenAIStreamExperienceStats()
			}
		}()
	}
	s.closeExperience()
	wg.Wait()
	require.Nil(t, s.experienceStore())
	s.closeExperience()
}

func TestOpenAIExperienceRecoveryBound(t *testing.T) {
	s, _, pool, req := experienceTestPool(t, 0.05, 0.06)
	experienceSeed(s, pool[0].account, 40000, 5)
	experienceSeed(s, pool[1].account, 5000, 5)
	now := time.Now()
	count := 0
	for i := 0; i < 100; i++ {
		candidates := append([]openAIAccountCandidateScore(nil), pool...)
		s.decorateExperience(candidates, req, now, defaultAdaptiveTTFTSwitchPolicy())
		if candidates[0].experienceRecovery {
			count++
		}
	}
	require.Equal(t, 1, count, "recovery cannot promote every request")
}

type experienceFrameStub struct {
	read   func() (coderws.MessageType, []byte, error)
	write  func()
	writes [][]byte
}

func (c *experienceFrameStub) ReadFrame(context.Context) (coderws.MessageType, []byte, error) {
	return c.read()
}
func (c *experienceFrameStub) WriteFrame(_ context.Context, _ coderws.MessageType, p []byte) error {
	c.writes = append(c.writes, append([]byte(nil), p...))
	if c.write != nil {
		c.write()
	}
	return nil
}
func (c *experienceFrameStub) Close() error { return nil }
func TestOpenAIExperienceWSTurnsAndInheritedSession(t *testing.T) {
	s := &OpenAIGatewayService{}
	defer s.closeExperience()
	account := &Account{ID: 1, Platform: PlatformOpenAI}
	stub := &experienceFrameStub{}
	conn := &openAIExperienceFrameConn{inner: stub, service: s, account: account}
	ctx := context.Background()
	session := []byte(`{"type":"session.update","session":{"model":"gpt-6.1-sol","reasoning":{"effort":"high"},"service_tier":"priority","tools":[{"type":"function"}]}}`)
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, session))
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"session.update","session":{"model":"gpt-6.1-sol"}}`)))
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","input":"hello"}`)))
	require.Equal(t, "high", conn.tracker.sample.Request.Effort)
	require.Equal(t, "priority", conn.tracker.sample.Request.ServiceTier)
	require.True(t, conn.tracker.sample.Request.HasTools)
	require.Equal(t, "gpt-6.1-sol", conn.tracker.sample.Model)
	stub.read = func() (coderws.MessageType, []byte, error) {
		return coderws.MessageText, []byte(`{"type":"response.completed"}`), nil
	}
	_, p, err := conn.ReadFrame(ctx)
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"response.completed"}`, string(p))
	require.Nil(t, conn.tracker)
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"other","reasoning":{"effort":"low"}}`)))
	require.Equal(t, int64(0), conn.tracker.snapshot().WaitMS)
	require.Equal(t, "low", conn.tracker.sample.Request.Effort)
	require.Equal(t, session, stub.writes[0])
}

func TestOpenAIExperienceEdgeOptionalWireCompatibility(t *testing.T) {
	var old OpenAIEdgeCompleteRequest
	require.NoError(t, json.Unmarshal([]byte(`{"success":true,"real_first_token_ms":123}`), &old))
	require.Nil(t, old.StreamExperience)
	require.Equal(t, int64(123), *old.RealFirstTokenMS)
	s := &OpenAIGatewayService{}
	plan := OpenAIEdgePlan{ResponseDialect: OpenAIEdgeDialectResponses, Body: experienceTestBody}
	s.ConfigureOpenAIEdgeStreamExperience(&plan)
	require.NotNil(t, plan.StreamExperience)
	require.Equal(t, "high", plan.StreamExperience.Request.Effort)
	oldDialect := OpenAIEdgePlan{ResponseDialect: OpenAIEdgeDialectChatCompletions, Body: experienceTestBody}
	s.ConfigureOpenAIEdgeStreamExperience(&oldDialect)
	require.Nil(t, oldDialect.StreamExperience)
}

func BenchmarkOpenAIExperienceOrder(b *testing.B) {
	s := &OpenAIGatewayService{}
	defer s.closeExperience()
	scheduler := &defaultOpenAIAccountScheduler{service: s, stats: newOpenAIAccountRuntimeStats()}
	pool := make([]openAIAccountCandidateScore, 500)
	for i := range pool {
		account := &Account{ID: int64(i + 1), Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
		pool[i] = openAIAccountCandidateScore{account: account, loadInfo: &AccountLoadInfo{}, sampleCount: 5, ttftSampleCount: 5, ttft: 1000, ttftP90: 2000, hasTTFT: true}
		gap := int64(5000)
		if i%2 == 0 {
			gap = 40000
		}
		experienceSeed(s, account, gap, 5)
	}
	req := OpenAIAccountScheduleRequest{RequestedModel: "gpt-6.1-sol", AccountSchedulingStrategy: AccountSchedulingStrategyHealthFirst, Experience: &openAIExperienceRoute{Request: experienceRequest(experienceTestBody), Transport: OpenAIUpstreamTransportHTTPSSE}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scheduler.buildHealthFirstSelectionOrder(pool, req)
	}
}

func TestOpenAIExperienceRedisLateAttachAndQueueFailure(t *testing.T) {
	s := &OpenAIGatewayService{}
	defer s.closeExperience()
	x := s.experienceStore()
	mr, client := newOpenAIExperienceRedisTestClient(t)
	s.SetOpenAIAccountHealthRedisClient(client)
	sample := experienceTestSample(30000)
	account := &Account{ID: 1, Platform: PlatformOpenAI}
	s.RecordOpenAIStreamExperience(account, OpenAIUpstreamTransportHTTPSSE, &sample, "late-attach")
	require.Eventually(t, func() bool { return len(mr.Keys()) > 0 }, time.Second, 10*time.Millisecond)
	require.Same(t, x, s.experienceStore())
	// A full observation queue never waits on Redis or changes local eligibility.
	x.mu.Lock()
	for i := 0; i < 2048; i++ {
		x.enqueue(openAIExperienceJob{Key: openAIExperienceKey{Account: int64(i)}})
	}
	x.mu.Unlock()
	require.Greater(t, s.OpenAIStreamExperienceStats()["queue_drops"], uint64(0))
}

func TestOpenAIExperienceCommonSlowPreservesCapacity(t *testing.T) {
	s, scheduler, pool, req := experienceTestPool(t, 0.05, 0.05)
	for _, candidate := range pool {
		experienceSeed(s, candidate.account, 40000, 5)
	}
	ordered := scheduler.buildHealthFirstSelectionOrder(pool, req)
	require.Len(t, ordered, 2)
	require.False(t, ordered[0].experienceDegraded)
	require.False(t, ordered[1].experienceDegraded)
	require.Equal(t, uint64(1), s.OpenAIStreamExperienceStats()["common_slow"])
}

func TestOpenAIExperienceTerminalTimestampDoesNotRefresh(t *testing.T) {
	x := newOpenAIExperienceTracker(experienceTestBody)
	experienceEvent(x, "response.completed", "")
	require.Positive(t, x.sample.AtMS)
	// Simulate delayed downstream completion without waiting in the test.
	x.sample.AtMS = time.Now().Add(-time.Hour).UnixMilli()
	expected := x.sample.AtMS
	x.advance(time.Hour)
	experienceEvent(x, "response.completed", "")
	require.Equal(t, expected, x.snapshot().AtMS)
	require.Equal(t, int64(0), x.snapshot().WaitMS)
}

func TestOpenAIExperienceRedisReplayKeepsLatestObservations(t *testing.T) {
	mr, client := newOpenAIExperienceRedisTestClient(t)
	b := newOpenAIExperienceBackend(client)
	sample := experienceTestSample(5000)
	key := openAIExperienceKey{7, sample.Model, OpenAIUpstreamTransportHTTPSSE, sample.Request}
	ctx := context.Background()
	base := sample.AtMS
	var expected []string
	for i := 0; i < 10; i++ {
		sample.AtMS = base - int64(10-i)
		var err error
		expected, err = b.apply(ctx, openAIExperienceJob{key, &openAIExperienceRecord{fmt.Sprintf("fresh-%02d", i), sample}})
		require.NoError(t, err)
	}
	for i := 0; i < 20; i++ {
		sample.AtMS = base - time.Hour.Milliseconds() - int64(i)
		rows, err := b.apply(ctx, openAIExperienceJob{key, &openAIExperienceRecord{fmt.Sprintf("late-%02d", i), sample}})
		require.NoError(t, err)
		require.Equal(t, expected, rows)
	}
	// The same timestamp must also retain a deterministic window across
	// replicas and late replay, without a receipt key for each request.
	sample.AtMS = base - 1
	latest := openAIExperienceRecord{"zz-latest", sample}
	expected, err := b.apply(ctx, openAIExperienceJob{key, &latest})
	require.NoError(t, err)
	for i := 0; i < 100; i++ {
		rows, err := b.apply(ctx, openAIExperienceJob{key, &latest})
		require.NoError(t, err)
		require.Equal(t, expected, rows)
	}
	require.Len(t, mr.Keys(), 1, "Redis key count must not grow per observation")
	other := &OpenAIGatewayService{openaiAccountHealthRedis: client}
	t.Cleanup(other.closeExperience)
	x := other.experienceStore()
	x.snapshot(key, time.Now(), time.Minute)
	require.Eventually(t, func() bool {
		return len(x.snapshot(key, time.Now(), time.Minute)) == 10
	}, time.Second, 10*time.Millisecond)
	for _, s := range x.snapshot(key, time.Now(), time.Minute) {
		require.Greater(t, s.AtMS, base-11)
	}
}

func TestOpenAIExperienceStickyToleratesEmptyCandidate(t *testing.T) {
	s, _, pool, req := experienceTestPool(t, 0.05, 0.05)
	experienceSeed(s, pool[0].account, 40000, 5)
	experienceSeed(s, pool[1].account, 5000, 5)
	s.decorateExperience(pool, req, time.Now(), defaultAdaptiveTTFTSwitchPolicy())
	pool = append([]openAIAccountCandidateScore{{}}, pool...)
	require.False(t, adaptiveOpenAIStickyCanLead(pool[1], pool,
		map[int64]struct{}{1: {}, 2: {}}, false, time.Now(), defaultAdaptiveTTFTSwitchPolicy()))
}

func TestOpenAIExperienceUnknownSnapshotDoesNotAllocateSamples(t *testing.T) {
	s := &OpenAIGatewayService{}
	t.Cleanup(s.closeExperience)
	x := s.experienceStore()
	key := openAIExperienceKey{Account: 1}
	require.Nil(t, x.snapshot(key, time.Now(), time.Minute))
	require.Zero(t, testing.AllocsPerRun(100, func() { x.snapshot(key, time.Now(), time.Minute) }))
}

func TestOpenAIExperienceWSExcludesPendingRequestWrite(t *testing.T) {
	s := &OpenAIGatewayService{}
	t.Cleanup(s.closeExperience)
	stub := &experienceFrameStub{}
	conn := &openAIExperienceFrameConn{inner: stub, service: s, account: &Account{ID: 1, Platform: PlatformOpenAI}}
	ctx := context.Background()
	var sentAt time.Time
	stub.write = func() {
		require.Same(t, conn.tracker, conn.sending)
		sentAt = time.Now()
	}
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, experienceTestBodyWithCreate()))
	require.False(t, conn.started.Before(sentAt))
	require.Nil(t, conn.sending)
	// A response that races an outbound write still passes through unchanged,
	// but cannot contribute an ambiguous latency sample.
	conn.tracker = nil
	conn.active = false
	stub.read = func() (coderws.MessageType, []byte, error) {
		return coderws.MessageText, []byte(`{"type":"response.completed"}`), nil
	}
	stub.write = func() {
		_, payload, err := conn.ReadFrame(ctx)
		require.NoError(t, err)
		require.JSONEq(t, `{"type":"response.completed"}`, string(payload))
	}
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, experienceTestBodyWithCreate()))
	require.Nil(t, conn.tracker)
	require.Equal(t, uint64(0), s.OpenAIStreamExperienceStats()["samples"])
}

func experienceTestBodyWithCreate() []byte {
	return []byte(`{"type":"response.create","model":"gpt-6.1-sol","input":"hello"}`)
}

func TestOpenAIExperienceWSOverlapAfterIneligibleTurnStaysNeutral(t *testing.T) {
	s := &OpenAIGatewayService{}
	t.Cleanup(s.closeExperience)
	stub := &experienceFrameStub{read: func() (coderws.MessageType, []byte, error) {
		return coderws.MessageText, []byte(`{"type":"response.completed"}`), nil
	}}
	conn := &openAIExperienceFrameConn{inner: stub, service: s, account: &Account{ID: 1, Platform: PlatformOpenAI}}
	ctx := context.Background()
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6.1-sol","tools":[{"type":"image_generation"}]}`)))
	require.Nil(t, conn.tracker)
	require.True(t, conn.active)
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, experienceTestBodyWithCreate()))
	require.True(t, conn.overlap)
	require.Nil(t, conn.tracker)
	_, _, err := conn.ReadFrame(ctx)
	require.NoError(t, err)
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, experienceTestBodyWithCreate()))
	require.Nil(t, conn.tracker)
}

func TestOpenAIExperienceExplicitImagesStayOutsideTextCohorts(t *testing.T) {
	for _, body := range []string{
		`{"type":"response.create","model":"gpt-image-2"}`,
		`{"type":"response.create","model":"gpt-test","tools":[{"type":" image_generation "}]}`,
		`{"type":"response.create","model":"gpt-test","tool_choice":{"type":"function","function":{"name":"image_gen.imagegen"}}}`,
		`{"type":"response.create","model":"gpt-test","input":[{"type":"function_call","name":"image_gen.imagegen"}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			require.Nil(t, experienceRoute(WithOpenAIStreamExperienceRequest(context.Background(), []byte(body), OpenAIUpstreamTransportHTTPSSE)))
			stub := &experienceFrameStub{}
			conn := &openAIExperienceFrameConn{inner: stub}
			require.NoError(t, conn.WriteFrame(context.Background(), coderws.MessageText, []byte(body)))
			require.Nil(t, conn.tracker)
			require.Equal(t, []byte(body), stub.writes[0])
		})
	}
	s, _, pool, req := experienceTestPool(t, 0.05, 0.05)
	experienceSeed(s, pool[0].account, 40000, 5)
	experienceSeed(s, pool[1].account, 5000, 5)
	req.RequiredImageCapability = OpenAIImagesCapabilityNative
	s.decorateExperience(pool, req, time.Now(), defaultAdaptiveTTFTSwitchPolicy())
	require.Empty(t, pool[0].experience)
	require.False(t, pool[0].experienceDegraded)
}

func TestOpenAIExperienceWSSessionImageChoiceAndModelBounds(t *testing.T) {
	stub := &experienceFrameStub{}
	conn := &openAIExperienceFrameConn{inner: stub}
	ctx := context.Background()
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"session.update","session":{"model":"gpt-test","tool_choice":"image_generation"}}`)))
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, experienceTestBodyWithCreate()))
	require.Nil(t, conn.tracker)
	conn.active = false
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"`+strings.Repeat("x", 201)+`"}`)))
	require.Nil(t, conn.tracker)
	require.Empty(t, conn.model)
	conn.active = false
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","tool_choice":"auto"}`)))
	require.Nil(t, conn.tracker, "an invalid session model must not inherit the previous valid model")
}

func TestOpenAIExperienceLRUPreservesRecentlyUsedCohort(t *testing.T) {
	s := &OpenAIGatewayService{}
	t.Cleanup(s.closeExperience)
	x := s.experienceStore()
	x.mu.Lock()
	defer x.mu.Unlock()
	for i := 1; i <= experienceMaxKeys; i++ {
		x.entry(openAIExperienceKey{Account: int64(i)})
	}
	x.entry(openAIExperienceKey{Account: 1})
	x.entry(openAIExperienceKey{Account: experienceMaxKeys + 1})
	require.Contains(t, x.entries, openAIExperienceKey{Account: 1})
	require.NotContains(t, x.entries, openAIExperienceKey{Account: 2})
	require.Len(t, x.entries, experienceMaxKeys)
	require.Equal(t, experienceMaxKeys, x.order.Len())
}

func BenchmarkOpenAIExperienceSnapshotChurn(b *testing.B) {
	s := &OpenAIGatewayService{}
	defer s.closeExperience()
	x := s.experienceStore()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		x.snapshot(openAIExperienceKey{Account: int64(i%5000 + 1)}, time.Now(), time.Minute)
	}
}

func TestOpenAIExperienceStrictAndOtherPlatformsStayUnchanged(t *testing.T) {
	s, scheduler, pool, req := experienceTestPool(t, 0.05, 0.05)
	pool[0].account.Priority = 0
	pool[1].account.Priority = 10
	experienceSeed(s, pool[0].account, 40000, 5)
	experienceSeed(s, pool[1].account, 5000, 5)
	req.AccountSchedulingStrategy = AccountSchedulingStrategyStrictPriority
	ordered := scheduler.buildOpenAISelectionOrder(req, openAIAccountLoadPlan{candidates: pool})
	require.Equal(t, int64(1), ordered[0].account.ID)
	require.Empty(t, ordered[0].experience)
	for _, platform := range []string{PlatformAnthropic, PlatformGemini, PlatformGrok} {
		pool[0].account.Platform = platform
		s.decorateExperience(pool, req, time.Now(), defaultAdaptiveTTFTSwitchPolicy())
		require.Empty(t, pool[0].experience)
		require.False(t, pool[0].experienceDegraded)
	}
}

func TestOpenAIExperienceLegacyBalancingPreservesDeprioritization(t *testing.T) {
	s, _, pool, req := experienceTestPool(t, 0.05, 0.05)
	for i := range pool {
		pool[i].account.Credentials = map[string]any{"pool_mode": true}
	}
	pool[0].account.Credentials["prompt_cache_boost_enabled"] = true
	pool[0].account.Credentials["prompt_cache_boost_level"] = OpenAIPromptCacheBoostLevelAggressive
	pool[0].account.Credentials["prompt_cache_boost_upstream_hit_priority_enabled"] = true
	experienceSeed(s, pool[0].account, 40000, 5)
	experienceSeed(s, pool[1].account, 5000, 5)
	ctx := context.WithValue(context.Background(), openAIExperienceContextKey{}, *req.Experience)
	items := []accountWithLoad{
		{account: pool[0].account, loadInfo: &AccountLoadInfo{}},
		{account: pool[1].account, loadInfo: &AccountLoadInfo{}},
	}
	ordered := s.orderOpenAIAvailableCandidatesForStrategyWithContext(ctx, items, req.RequestedModel,
		config.GatewaySchedulingConfig{}, true, req.AccountSchedulingStrategy, nil, 0)
	require.Equal(t, int64(2), ordered[0].account.ID)
	require.Equal(t, 1, ordered[1].openAIExperienceGrade)
	for i := 0; i < 100; i++ {
		balanceAdaptiveOpenAIAccountTies(ordered, openAIPromptCacheBoostUpstreamAffinitySessionPrefix+"experience", false, time.Now())
		require.Equal(t, int64(2), ordered[0].account.ID, "load spreading and cache affinity must preserve stream health")
	}
	// With no experience penalty, the existing cache affinity still applies.
	ordered[1].openAIExperienceGrade = 0
	balanceAdaptiveOpenAIAccountTies(ordered, openAIPromptCacheBoostUpstreamAffinitySessionPrefix+"experience", false, time.Now())
	require.Equal(t, int64(1), ordered[0].account.ID)
}

func TestOpenAIExperienceMappedModelUsesForwardedCohort(t *testing.T) {
	for _, composite := range []bool{false, true} {
		t.Run(fmt.Sprintf("composite=%v", composite), func(t *testing.T) {
			s, scheduler, pool, req := experienceTestPool(t, 0.05, 0.05)
			for i := range pool {
				pool[i].account.Credentials = map[string]any{"model_mapping": map[string]any{"channel-model": "gpt-6.1-sol"}}
			}
			experienceSeed(s, pool[0].account, 40000, 5)
			experienceSeed(s, pool[1].account, 5000, 5)
			req.RequestedModel = "public-alias"
			ctx := context.Background()
			body := []byte(`{"model":"channel-model","reasoning":{"effort":"high"},"service_tier":"default","input":"hello"}`)
			if composite {
				ctx = WithResolvedUpstreamModel(ctx, "channel-model")
				body = []byte(`{"model":"public-alias","reasoning":{"effort":"high"},"service_tier":"default","input":"hello"}`)
			}
			req.Experience = experienceRoute(WithOpenAIStreamExperienceRequest(ctx, body, OpenAIUpstreamTransportHTTPSSE))
			require.Equal(t, "channel-model", req.Experience.Model)
			ordered := scheduler.buildHealthFirstSelectionOrder(pool, req)
			require.Equal(t, int64(2), ordered[0].account.ID)
			require.True(t, ordered[1].experienceDegraded)
			require.Equal(t, "gpt-6.1-sol", ordered[1].experience[0].Model)
		})
	}
}

func TestOpenAIExperienceOAuthNormalizationUsesForwardedModel(t *testing.T) {
	s, scheduler, pool, req := experienceTestPool(t, 0.05, 0.05)
	req.RequestedModel = "gpt-5.1-codex-max"
	for i := range pool {
		pool[i].account.Type = AccountTypeOAuth
		gap := int64(5000)
		if i == 0 {
			gap = 40000
		}
		for j := 0; j < 5; j++ {
			sample := experienceTestSample(gap)
			sample.Model = "gpt-5.3-codex"
			s.RecordOpenAIStreamExperience(pool[i].account, OpenAIUpstreamTransportHTTPSSE, &sample, fmt.Sprintf("normalized-%d-%d", i, j))
		}
	}
	ordered := scheduler.buildHealthFirstSelectionOrder(pool, req)
	require.Equal(t, int64(2), ordered[0].account.ID)
	require.True(t, ordered[1].experienceDegraded)
}

func TestOpenAIExperiencePassthroughUsesActualBodyModel(t *testing.T) {
	for _, transport := range []OpenAIUpstreamTransport{OpenAIUpstreamTransportHTTPSSE, OpenAIUpstreamTransportResponsesWebsocketV2} {
		for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
			t.Run(string(transport)+"/"+accountType, func(t *testing.T) {
				s, scheduler, pool, req := experienceTestPool(t, 0.05, 0.05)
				s.cfg = &config.Config{}
				s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
				for i := range pool {
					pool[i].account.Type = accountType
					pool[i].account.Extra = map[string]any{
						"openai_passthrough":                         true,
						"openai_oauth_responses_websockets_v2_mode":  OpenAIWSIngressModePassthrough,
						"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough,
					}
					pool[i].account.Credentials = map[string]any{"model_mapping": map[string]any{"gpt-5.1-codex-max": "other-account-model"}}
					gap := int64(5000)
					if i == 0 {
						gap = 40000
					}
					for j := 0; j < 5; j++ {
						sample := experienceTestSample(gap)
						sample.Model = "gpt-5.1-codex-max"
						s.RecordOpenAIStreamExperience(pool[i].account, transport, &sample, fmt.Sprintf("raw-%d-%d", i, j))
					}
				}
				body := []byte(`{"model":"gpt-5.1-codex-max","reasoning":{"effort":"high"},"service_tier":"default","input":"hello"}`)
				req.Experience = experienceRoute(WithOpenAIStreamExperienceRequest(context.Background(), body, transport))
				req.RequestedModel = "public-alias"
				ordered := scheduler.buildHealthFirstSelectionOrder(pool, req)
				require.Equal(t, int64(2), ordered[0].account.ID)
				require.True(t, ordered[1].experienceDegraded)
				if transport == OpenAIUpstreamTransportHTTPSSE {
					// Forward's ordinary passthrough mapped-model argument is empty.
					ctx := WithResolvedUpstreamModel(context.Background(), "different-composite-model")
					ctx = WithOpenAIStreamExperienceRequest(ctx, body, transport)
					reader, finish := s.observeOpenAIExperienceBody(ctx, io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n")), pool[0].account, "")
					_, err := io.ReadAll(reader)
					require.NoError(t, err)
					finish()
					key := openAIExperienceKey{pool[0].account.ID, "gpt-5.1-codex-max", transport, experienceRequest(body)}
					samples := s.experienceStore().snapshot(key, time.Now(), time.Hour)
					require.Len(t, samples, 6, "empty mapped model must not discard passthrough samples")
				}
			})
		}
	}
}

func TestOpenAIExperienceUsesActualTransportAndKeepsBridgeNeutral(t *testing.T) {
	s, scheduler, pool, req := experienceTestPool(t, 0.05, 0.05)
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	s.cfg = cfg
	s.openaiWSResolver = NewOpenAIWSProtocolResolver(cfg)
	for i := range pool {
		pool[i].account.Extra = map[string]any{"responses_websockets_v2_enabled": true}
		pool[i].account.Credentials = map[string]any{"pool_mode": true}
		require.Equal(t, OpenAIUpstreamTransportResponsesWebsocketV2, s.openaiWSResolver.Resolve(pool[i].account).Transport)
	}
	experienceSeed(s, pool[0].account, 40000, 5)
	experienceSeed(s, pool[1].account, 5000, 5)
	// HTTP ingress stays SSE despite the account's WS configuration.
	req.Experience = experienceRoute(WithOpenAIStreamExperienceRequest(context.Background(), experienceTestBody, OpenAIUpstreamTransportHTTPSSE))
	ordered := scheduler.buildHealthFirstSelectionOrder(pool, req)
	require.Equal(t, int64(2), ordered[0].account.ID)
	require.True(t, ordered[1].experienceDegraded)
	// WS ingress must not borrow the opposite transport's measurements.
	req.Experience = experienceRoute(WithOpenAIStreamExperienceRequest(context.Background(), experienceTestBody, OpenAIUpstreamTransportResponsesWebsocketV2Ingress))
	ordered = scheduler.buildHealthFirstSelectionOrder(pool, req)
	require.Empty(t, ordered[0].experience)
	for i := range pool {
		gap := int64(40000)
		if i == 1 {
			gap = 5000
		}
		for j := 0; j < 5; j++ {
			sample := experienceTestSample(gap)
			s.RecordOpenAIStreamExperience(pool[i].account, OpenAIUpstreamTransportResponsesWebsocketV2, &sample, fmt.Sprintf("ws-%d-%d", i, j))
			sample.Request.InputBucket = 4
			s.RecordOpenAIStreamExperience(pool[i].account, OpenAIUpstreamTransportResponsesWebsocketV2, &sample, fmt.Sprintf("ws-large-%d-%d", i, j))
		}
	}
	ordered = scheduler.buildHealthFirstSelectionOrder(pool, req)
	require.Equal(t, int64(2), ordered[0].account.ID)
	cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
	ordered = scheduler.buildHealthFirstSelectionOrder(pool, req)
	require.Equal(t, int64(2), ordered[0].account.ID, "default dynamic bridge must not disable ordinary WS scoring")
	for _, bridge := range []string{"dynamic", "account_mode", "isolation"} {
		t.Run(bridge, func(t *testing.T) {
			request := req
			route := *req.Experience
			request.Experience = &route
			cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = bridge == "dynamic"
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = bridge == "account_mode"
			if bridge == "dynamic" {
				request.Experience.Request.InputBucket = 4
			}
			for i := range pool {
				pool[i].account.Credentials["upstream_strong_isolation_enabled"] = bridge == "isolation"
				if bridge == "account_mode" {
					pool[i].account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeHTTPBridge
				}
			}
			ordered := scheduler.buildHealthFirstSelectionOrder(pool, request)
			for _, candidate := range ordered {
				require.Empty(t, candidate.experience, "uncertain bridge paths must not consume a WS baseline")
				require.False(t, candidate.experienceDegraded)
			}
		})
	}
}

func TestOpenAIExperienceDynamicBridgeKeepsPassthroughAndContinuationActive(t *testing.T) {
	s, scheduler, pool, req := experienceTestPool(t, 0.05, 0.05)
	s.cfg = &config.Config{}
	s.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
	s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	req.Experience.Transport = OpenAIUpstreamTransportResponsesWebsocketV2Ingress
	req.Experience.Request.InputBucket = 4
	for i := range pool {
		pool[i].account.Concurrency = 1
		pool[i].account.Extra = map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough}
		gap := int64(40000)
		if i == 1 {
			gap = 5000
		}
		for j := 0; j < 5; j++ {
			sample := experienceTestSample(gap)
			sample.Request = req.Experience.Request
			s.RecordOpenAIStreamExperience(pool[i].account, OpenAIUpstreamTransportResponsesWebsocketV2, &sample, fmt.Sprintf("large-passthrough-%d-%d", i, j))
		}
	}
	s.cfg.Gateway.OpenAIWS.Enabled = true
	s.cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	s.cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	ordered := scheduler.buildHealthFirstSelectionOrder(pool, req)
	require.Equal(t, int64(2), ordered[0].account.ID)
	require.True(t, ordered[1].experienceDegraded, "passthrough returns before the size bridge")
	request := req.Experience.Request
	request.Continuation = true
	pool[0].account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
	require.False(t, s.experienceMayUseHTTPBridge(pool[0].account, request), "continuation never triggers dynamic bridge")
}
