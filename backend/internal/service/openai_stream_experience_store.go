package service

import (
	"container/list"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	experienceMaxKeys   = 4096
	experienceIOTimeout = 250 * time.Millisecond
)

type openAIExperienceKey struct {
	Account   int64
	Model     string
	Transport OpenAIUpstreamTransport
	Request   OpenAIExperienceRequest
}
type openAIExperienceRecord struct {
	ID     string                       `json:"id"`
	Sample OpenAIStreamExperienceSample `json:"sample"`
}
type openAIExperienceEntry struct {
	Records          []openAIExperienceRecord
	Refreshed, Probe time.Time
	position         *list.Element
}
type openAIExperienceJob struct {
	Key    openAIExperienceKey
	Record *openAIExperienceRecord
}
type openAIExperienceBackend interface {
	apply(context.Context, openAIExperienceJob) ([]string, error)
}
type openAIExperienceStore struct {
	mu                                sync.Mutex
	entries                           map[openAIExperienceKey]*openAIExperienceEntry
	order                             list.List
	jobs                              chan openAIExperienceJob
	ctx                               context.Context
	cancel                            context.CancelFunc
	wg                                sync.WaitGroup
	closed                            bool
	probeAt                           time.Time
	probeTurns                        uint64
	decisionLogAt                     time.Time
	backend                           openAIExperienceBackend
	drops, errors, samples, decisions atomic.Uint64
	commonSlow                        atomic.Uint64
}

func (s *OpenAIGatewayService) OpenAIStreamExperienceEnabled() bool {
	return s != nil && (s.cfg == nil || s.cfg.Gateway.Scheduling.StreamExperienceEnabled == nil || *s.cfg.Gateway.Scheduling.StreamExperienceEnabled)
}
func (s *OpenAIGatewayService) experienceStore() *openAIExperienceStore {
	if !s.OpenAIStreamExperienceEnabled() {
		return nil
	}
	s.openaiExperienceMu.Lock()
	defer s.openaiExperienceMu.Unlock()
	if s.openaiExperienceClosed {
		return nil
	}
	if s.openaiExperience == nil {
		ctx, cancel := context.WithCancel(context.Background())
		x := &openAIExperienceStore{entries: make(map[openAIExperienceKey]*openAIExperienceEntry), jobs: make(chan openAIExperienceJob, 1024), ctx: ctx, cancel: cancel, backend: newOpenAIExperienceBackend(s.openaiAccountHealthRedis)}
		s.openaiExperience = x
		if x.backend != nil {
			x.wg.Add(1)
			go x.run()
		}
	}
	return s.openaiExperience
}
func (s *OpenAIGatewayService) closeExperience() {
	s.openaiExperienceMu.Lock()
	s.openaiExperienceClosed = true
	x := s.openaiExperience
	s.openaiExperienceMu.Unlock()
	if x != nil {
		x.mu.Lock()
		x.closed = true
		x.cancel()
		x.mu.Unlock()
		x.wg.Wait()
	}
}

// Diagnostics never contain prompts, tokens, tool arguments or credentials.
func (s *OpenAIGatewayService) OpenAIStreamExperienceStats() map[string]uint64 {
	s.openaiExperienceMu.Lock()
	x := s.openaiExperience
	s.openaiExperienceMu.Unlock()
	if x == nil {
		return map[string]uint64{}
	}
	x.mu.Lock()
	keys := len(x.entries)
	x.mu.Unlock()
	return map[string]uint64{"keys": uint64(keys), "samples": x.samples.Load(), "decisions": x.decisions.Load(), "queue_drops": x.drops.Load(), "redis_errors": x.errors.Load(), "common_slow": x.commonSlow.Load()}
}

func validExperience(s *OpenAIStreamExperienceSample) bool {
	if s == nil || s.Model == "" || len(s.Model) > 200 || s.AtMS <= 0 || s.AtMS > time.Now().Add(time.Minute).UnixMilli() || s.WaitMS < 0 || s.WaitMS > 24*60*60*1000 || s.MaxGapMS < 0 || s.MaxGapMS > s.WaitMS || s.ToolGapMS < 0 || s.ToolGapMS > s.MaxGapMS || s.LongWaitMS < 0 || s.LongWaitMS > s.WaitMS || s.LongGaps < 0 || s.LongGaps > 1_000_000 || s.Request.InputBucket < 0 || s.Request.InputBucket > 4 {
		return false
	}
	if s.FirstReadyMS != nil && (*s.FirstReadyMS < 0 || *s.FirstReadyMS > s.WaitMS) {
		return false
	}
	switch s.Request.Effort {
	case "unspecified", "none", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return false
	}
	switch s.Request.ServiceTier {
	case "unspecified", "auto", "default", "flex", "priority":
	default:
		return false
	}
	return true
}
func (s *OpenAIGatewayService) RecordOpenAIStreamExperience(account *Account, transport OpenAIUpstreamTransport, sample *OpenAIStreamExperienceSample, eventID string) {
	if account == nil || account.Platform != PlatformOpenAI || !validExperience(sample) || (transport != OpenAIUpstreamTransportHTTPSSE && transport != OpenAIUpstreamTransportResponsesWebsocketV2) {
		return
	}
	x := s.experienceStore()
	if x == nil {
		return
	}
	if eventID == "" {
		eventID = uuid.NewString()
	}
	copySample := *sample
	copySample.Transport = transport
	if sample.FirstReadyMS != nil {
		n := *sample.FirstReadyMS
		copySample.FirstReadyMS = &n
	}
	record := openAIExperienceRecord{eventID, copySample}
	key := openAIExperienceKey{account.ID, sample.Model, transport, sample.Request}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.closed {
		return
	}
	e := x.entry(key)
	if mergeExperience(e, record) {
		x.samples.Add(1)
	}
	x.enqueue(openAIExperienceJob{key, &record})
}
func (x *openAIExperienceStore) entry(key openAIExperienceKey) *openAIExperienceEntry {
	e := x.entries[key]
	if e == nil {
		if len(x.entries) >= experienceMaxKeys {
			front := x.order.Front()
			oldest, _ := front.Value.(openAIExperienceKey)
			delete(x.entries, oldest)
			x.order.Remove(front)
		}
		e = &openAIExperienceEntry{position: x.order.PushBack(key)}
		x.entries[key] = e
	} else {
		x.order.MoveToBack(e.position)
	}
	return e
}
func mergeExperience(e *openAIExperienceEntry, r openAIExperienceRecord) bool {
	if len(e.Records) == 10 {
		oldest := e.Records[0]
		if r.Sample.AtMS < oldest.Sample.AtMS || r.Sample.AtMS == oldest.Sample.AtMS && r.ID < oldest.ID {
			return false
		}
	}
	for _, old := range e.Records {
		if old.ID == r.ID {
			return false
		}
	}
	e.Records = append(e.Records, r)
	sort.Slice(e.Records, func(i, j int) bool {
		if e.Records[i].Sample.AtMS != e.Records[j].Sample.AtMS {
			return e.Records[i].Sample.AtMS < e.Records[j].Sample.AtMS
		}
		return e.Records[i].ID < e.Records[j].ID
	})
	if len(e.Records) > 10 {
		e.Records = append([]openAIExperienceRecord(nil), e.Records[len(e.Records)-10:]...)
	}
	return true
}
func (x *openAIExperienceStore) enqueue(j openAIExperienceJob) {
	if x.backend == nil || x.closed {
		return
	}
	select {
	case x.jobs <- j:
	default:
		x.drops.Add(1)
	}
}
func (x *openAIExperienceStore) snapshot(key openAIExperienceKey, now time.Time, fresh time.Duration) []OpenAIStreamExperienceSample {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.closed {
		return nil
	}
	e := x.entry(key)
	if now.Sub(e.Refreshed) >= 15*time.Second {
		e.Refreshed = now
		x.enqueue(openAIExperienceJob{Key: key})
	}
	var out []OpenAIStreamExperienceSample
	for _, r := range e.Records {
		if r.Sample.Complete && now.Sub(time.UnixMilli(r.Sample.AtMS)) < fresh {
			if out == nil {
				out = make([]OpenAIStreamExperienceSample, 0, len(e.Records))
			}
			out = append(out, r.Sample)
		}
	}
	return out
}

func (x *openAIExperienceStore) run() {
	defer x.wg.Done()
	for {
		select {
		case <-x.ctx.Done():
			return
		case j := <-x.jobs:
			ctx, cancel := context.WithTimeout(x.ctx, experienceIOTimeout)
			rows, err := x.backend.apply(ctx, j)
			cancel()
			if err != nil {
				x.errors.Add(1)
				continue
			}
			x.mu.Lock()
			if !x.closed {
				e := x.entry(j.Key)
				for _, row := range rows {
					var r openAIExperienceRecord
					if json.Unmarshal([]byte(row), &r) == nil && validExperience(&r.Sample) {
						mergeExperience(e, r)
					}
				}
			}
			x.mu.Unlock()
		}
	}
}

func experienceMedian(samples []OpenAIStreamExperienceSample, metric func(OpenAIStreamExperienceSample) int64) int64 {
	var scratch [10]int64
	v := scratch[:0]
	if len(samples) > len(scratch) {
		v = make([]int64, 0, len(samples))
	}
	for _, s := range samples {
		v = append(v, metric(s))
	}
	if len(v) == 0 {
		return 0
	}
	slices.Sort(v)
	return v[len(v)/2]
}

// MaxGap includes reasoning, text and tool-argument progress. The separate
// tool metric is diagnostic: scoring it again would double count one pause.
func experienceSeverity(samples []OpenAIStreamExperienceSample) int64 {
	if len(samples) < 5 {
		return 0
	}
	metric := func(s OpenAIStreamExperienceSample) int64 { return s.MaxGapMS }
	overall, recent := experienceMedian(samples, metric), experienceMedian(samples[len(samples)-5:], metric)
	if recent < overall {
		return recent
	}
	return overall
}
func sameExperienceCohort(a, b []OpenAIStreamExperienceSample) bool {
	return len(a) >= 5 && len(b) >= 5 && a[0].Model == b[0].Model && a[0].Request == b[0].Request && a[0].Transport == b[0].Transport
}
func experienceBaseline(samples []OpenAIStreamExperienceSample) int64 {
	metric := func(s OpenAIStreamExperienceSample) int64 { return s.MaxGapMS }
	baseline := experienceMedian(samples, metric)
	if len(samples) >= 5 {
		// Earlier fast observations must not establish a smooth alternative
		// when its recent requests already show the same persistent stalls.
		baseline = experienceMax(baseline, experienceMedian(samples[len(samples)-5:], metric))
	}
	return baseline
}
func experienceThreshold(samples []OpenAIStreamExperienceSample, floor int64) int64 {
	baseline := experienceBaseline(samples)
	return experienceMax(floor, experienceMax(baseline+5_000, baseline+baseline/2))
}
func experienceWorse(a, b []OpenAIStreamExperienceSample, floor int64) bool {
	return sameExperienceCohort(a, b) && experienceSeverity(a) >= experienceThreshold(b, floor)
}

func (s *OpenAIGatewayService) experienceGapFloor() int64 {
	if s != nil && s.cfg != nil && s.cfg.Gateway.Scheduling.StreamExperienceGapSeconds > 0 {
		return int64(s.cfg.Gateway.Scheduling.StreamExperienceGapSeconds) * 1000
	}
	return 20_000
}
func (s *OpenAIGatewayService) experienceDecision(account int64, model, reason string) {
	x := s.experienceStore()
	if x == nil {
		return
	}
	x.decisions.Add(1)
	x.mu.Lock()
	emit := time.Since(x.decisionLogAt) >= time.Minute
	if emit {
		x.decisionLogAt = time.Now()
	}
	x.mu.Unlock()
	if emit {
		slog.Info("openai stream experience deprioritized candidate", "degraded_account_id", account, "model", model, "reason", reason)
	}
}

func (x *openAIExperienceStore) noImprovement(accounts int, model string) {
	x.commonSlow.Add(1)
	x.mu.Lock()
	emit := time.Since(x.decisionLogAt) >= time.Minute
	if emit {
		x.decisionLogAt = time.Now()
	}
	x.mu.Unlock()
	if emit {
		slog.Info("openai stream experience no better candidate", "comparable_accounts", accounts, "model", model, "reason", "common_slow_without_proven_improvement")
	}
}
