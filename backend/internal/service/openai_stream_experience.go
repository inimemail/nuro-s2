package service

// Stream experience is independent of TTFT, billing and the semantic timeout
// learner. Its clock advances only while reading the provider, never while
// writing to a slow client or waiting for the next WS turn.
import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

type OpenAIExperienceRequest struct {
	HasTools     bool   `json:"has_tools"`
	Effort       string `json:"effort"`
	Continuation bool   `json:"continuation"`
	InputBucket  int    `json:"input_bucket"`
	ServiceTier  string `json:"service_tier"`
}

type OpenAIStreamExperienceSample struct {
	Request      OpenAIExperienceRequest `json:"request"`
	Transport    OpenAIUpstreamTransport `json:"transport,omitempty"`
	Model        string                  `json:"model"`
	FirstReadyMS *int64                  `json:"first_ready_ms,omitempty"`
	MaxGapMS     int64                   `json:"max_gap_ms"`
	ToolGapMS    int64                   `json:"tool_gap_ms"`
	LongGaps     int                     `json:"long_gaps"`
	LongWaitMS   int64                   `json:"long_wait_ms"`
	WaitMS       int64                   `json:"wait_ms"`
	Complete     bool                    `json:"complete"`
	AtMS         int64                   `json:"at_ms"`
}

type openAIExperienceContextKey struct{}
type openAIExperienceRoute struct {
	Request   OpenAIExperienceRequest
	Transport OpenAIUpstreamTransport
	Model     string
	WireModel string
}

// WithOpenAIStreamExperienceRequest only stores bounded, content-free metadata.
// Images/compaction and ambiguous requests deliberately have no cohort.
func WithOpenAIStreamExperienceRequest(ctx context.Context, body []byte, transport OpenAIUpstreamTransport) context.Context {
	if !json.Valid(body) || (gjson.GetBytes(body, "model").String() == "" || len(gjson.GetBytes(body, "model").String()) > 200) || gjson.GetBytes(body, "context_management").Exists() {
		return context.WithValue(ctx, openAIExperienceContextKey{}, nil)
	}
	if IsExplicitImageGenerationIntent(openAIResponsesEndpoint, "", body) {
		return context.WithValue(ctx, openAIExperienceContextKey{}, nil)
	}
	r := experienceRequest(body)
	model := gjson.GetBytes(body, "model").String()
	if upstream, ok := ResolvedUpstreamModelFromContext(ctx); ok {
		model = upstream
	}
	if model == "" || len(model) > 200 || isOpenAIImageGenerationModel(model) {
		return context.WithValue(ctx, openAIExperienceContextKey{}, nil)
	}
	return context.WithValue(ctx, openAIExperienceContextKey{}, openAIExperienceRoute{Request: r, Transport: transport, Model: model, WireModel: gjson.GetBytes(body, "model").String()})
}

func experienceRoute(ctx context.Context) *openAIExperienceRoute {
	if ctx == nil {
		return nil
	}
	r, ok := ctx.Value(openAIExperienceContextKey{}).(openAIExperienceRoute)
	if !ok {
		return nil
	}
	return &r
}

func experienceRequest(body []byte) OpenAIExperienceRequest {
	effort := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String()))
	if effort == "" {
		effort = strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "reasoning_effort").String()))
	}
	switch effort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		effort = "unspecified"
	}
	tier := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "service_tier").String()))
	switch tier {
	case "auto", "default", "flex", "priority":
	default:
		tier = "unspecified"
	}
	bucket := 0
	for _, n := range []int{16 << 10, 64 << 10, 256 << 10, 1 << 20} {
		if len(body) > n {
			bucket++
		}
	}
	return OpenAIExperienceRequest{HasTools: len(gjson.GetBytes(body, "tools").Array()) > 0, Effort: effort, Continuation: gjson.GetBytes(body, "previous_response_id").String() != "", InputBucket: bucket, ServiceTier: tier}
}

type openAIExperienceTracker struct {
	mu       sync.Mutex
	sample   OpenAIStreamExperienceSample
	clock    time.Duration
	last     time.Duration
	progress bool
	terminal bool
	tools    map[string]bool
}

func newOpenAIExperienceTracker(body []byte) *openAIExperienceTracker {
	return &openAIExperienceTracker{sample: OpenAIStreamExperienceSample{Request: experienceRequest(body), Model: gjson.GetBytes(body, "model").String()}, tools: make(map[string]bool)}
}

func (t *openAIExperienceTracker) advance(d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.terminal && d > 0 {
		t.clock += d
	}
}
func (t *openAIExperienceTracker) observe(payload []byte) {
	if t == nil {
		return
	}
	if !gjson.ValidBytes(payload) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.terminal {
		return
	}
	kind := gjson.GetBytes(payload, "type").String()
	obs := classifyOpenAIStreamProgress(payload, kind)
	if obs.semantic {
		if t.progress {
			gap := (t.clock - t.last).Milliseconds()
			if gap > t.sample.MaxGapMS {
				t.sample.MaxGapMS = gap
			}
			if len(t.tools) > 0 && gap > t.sample.ToolGapMS {
				t.sample.ToolGapMS = gap
			}
			if gap >= 20_000 {
				t.sample.LongGaps++
				t.sample.LongWaitMS += gap
			}
		}
		t.last = t.clock
		t.progress = true
	}
	if t.sample.FirstReadyMS == nil && ((kind == "response.output_text.delta" || kind == "response.function_call_arguments.delta" || kind == "response.custom_tool_call_input.delta") && gjson.GetBytes(payload, "delta").String() != "") {
		n := t.clock.Milliseconds()
		t.sample.FirstReadyMS = &n
	}
	if kind == "response.output_item.added" {
		typeName := gjson.GetBytes(payload, "item.type").String()
		id := gjson.GetBytes(payload, "item.id").String()
		if (typeName == "function_call" || typeName == "custom_tool_call") && id != "" && len(id) <= 256 && len(t.tools) < 128 {
			t.tools[id] = true
		}
	}
	if kind == "response.output_item.done" {
		delete(t.tools, gjson.GetBytes(payload, "item.id").String())
	}
	if obs.terminal {
		// Include a final silence before terminal, but structural events never
		// reset the progress clock and reasoning-only work is not a failure.
		if t.progress {
			gap := (t.clock - t.last).Milliseconds()
			if gap > t.sample.MaxGapMS {
				t.sample.MaxGapMS = gap
			}
			if gap >= 20_000 {
				t.sample.LongGaps++
				t.sample.LongWaitMS += gap
			}
		}
		t.terminal = true
		t.sample.AtMS = time.Now().UnixMilli()
		t.sample.Complete = kind == "response.completed" || kind == "response.done"
		status := gjson.GetBytes(payload, "response.status").String()
		if status != "" && status != "completed" {
			t.sample.Complete = false
		}
	}
}

func (t *openAIExperienceTracker) snapshot() OpenAIStreamExperienceSample {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.sample
	s.WaitMS = t.clock.Milliseconds()
	if s.AtMS == 0 {
		s.AtMS = time.Now().UnixMilli()
	}
	if s.FirstReadyMS != nil {
		n := *s.FirstReadyMS
		s.FirstReadyMS = &n
	}
	return s
}

// This reader observes provider waits and leaves every returned byte unchanged.
// Parsing is bounded; an oversized frame invalidates sampling, not the relay.
type openAIExperienceReader struct {
	io.ReadCloser
	tracker    *openAIExperienceTracker
	line, data []byte
	invalid    atomic.Bool
	now        func() time.Time
}

func (r *openAIExperienceReader) Read(p []byte) (int, error) {
	now := r.now
	if now == nil {
		now = time.Now
	}
	start := now()
	n, err := r.ReadCloser.Read(p)
	r.tracker.advance(now().Sub(start))
	if r.invalid.Load() {
		return n, err
	}
	for _, b := range p[:n] {
		if b != '\n' {
			r.line = append(r.line, b)
			if len(r.line)+len(r.data) > 1<<20 {
				r.invalid.Store(true)
				r.line = nil
				r.data = nil
				break
			}
			continue
		}
		line := bytes.TrimSuffix(r.line, []byte{'\r'})
		if len(line) == 0 {
			if len(r.data) > 0 {
				r.tracker.observe(r.data)
			}
			r.data = r.data[:0]
		} else if bytes.HasPrefix(line, []byte("data:")) {
			if len(r.data) > 0 {
				r.data = append(r.data, '\n')
			}
			r.data = append(r.data, bytes.TrimPrefix(line[5:], []byte{' '})...)
		}
		r.line = r.line[:0]
	}
	return n, err
}

func (s *OpenAIGatewayService) observeOpenAIExperienceBody(ctx context.Context, body io.ReadCloser, account *Account, model string) (io.ReadCloser, func()) {
	route := experienceRoute(ctx)
	if !s.OpenAIStreamExperienceEnabled() || route == nil || body == nil || account == nil || account.Platform != PlatformOpenAI {
		return body, func() {}
	}
	if model == "" {
		// Ordinary SSE passthrough intentionally has no mapped-model value.
		// The refreshed route carries the model from the actual outbound body.
		model = route.WireModel
	}
	t := &openAIExperienceTracker{sample: OpenAIStreamExperienceSample{Request: route.Request, Model: model}, tools: make(map[string]bool)}
	r := &openAIExperienceReader{ReadCloser: body, tracker: t}
	return r, func() {
		sample := t.snapshot()
		if ctx.Err() != nil || r.invalid.Load() {
			sample.Complete = false
		}
		s.RecordOpenAIStreamExperience(account, OpenAIUpstreamTransportHTTPSSE, &sample, "")
	}
}

func (s *OpenAIGatewayService) startExperienceTurn(ctx context.Context, account *Account, body []byte) (*openAIExperienceTracker, func()) {
	if !s.OpenAIStreamExperienceEnabled() || account == nil || account.Platform != PlatformOpenAI || experienceRoute(WithOpenAIStreamExperienceRequest(context.Background(), body, OpenAIUpstreamTransportResponsesWebsocketV2)) == nil {
		return nil, func() {}
	}
	t := newOpenAIExperienceTracker(body)
	return t, func() {
		sample := t.snapshot()
		if ctx.Err() != nil {
			sample.Complete = false
		}
		s.RecordOpenAIStreamExperience(account, OpenAIUpstreamTransportResponsesWebsocketV2, &sample, "")
	}
}
