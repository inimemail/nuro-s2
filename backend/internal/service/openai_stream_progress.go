package service

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	openAIStreamProgressMinSamples  = 8
	openAIStreamProgressMaxSamples  = 64
	openAIStreamProgressMaxRoutes   = 4096
	openAIStreamSemanticStallReason = GatewayFailureReason("openai_stream_semantic_stall")
	openAIStreamProgressPolicyKey   = "openai_stream_progress_policy"
)

type openAIStreamProgressKey struct {
	accountID int64
	model     string
	transport OpenAIUpstreamTransport
	effort    string
}

type openAIStreamProgressStat struct {
	mu             sync.Mutex
	samples        []time.Duration
	actions        int
	actionFailures int
	suspendedUntil time.Time
}

type openAIStreamProgressLearner struct {
	stats sync.Map
	count atomic.Int64
}

type openAIStreamProgressObservation struct {
	semantic  bool
	reasoning bool
	visible   bool
	terminal  bool
}

type openAIStreamProgressTracker struct {
	lastSemanticAt time.Time
	maxSemanticGap time.Duration
	reasoningSeen  bool
	visibleSeen    bool
	terminalSeen   bool
}

type openAIStreamProgressPolicy struct {
	effort   string
	disabled bool
}

func setOpenAIStreamProgressPolicy(c *gin.Context, effort *string, disabled bool) {
	if c == nil {
		return
	}
	value := ""
	if effort != nil {
		value = *effort
	}
	c.Set(openAIStreamProgressPolicyKey, openAIStreamProgressPolicy{effort: value, disabled: disabled})
}

func openAIStreamProgressPolicyFromContext(c *gin.Context) openAIStreamProgressPolicy {
	if c == nil {
		return openAIStreamProgressPolicy{}
	}
	value, ok := c.Get(openAIStreamProgressPolicyKey)
	if !ok {
		return openAIStreamProgressPolicy{}
	}
	policy, _ := value.(openAIStreamProgressPolicy)
	return policy
}

func newOpenAIStreamProgressTracker(now time.Time) *openAIStreamProgressTracker {
	if now.IsZero() {
		now = time.Now()
	}
	return &openAIStreamProgressTracker{lastSemanticAt: now}
}

func (t *openAIStreamProgressTracker) observe(now time.Time, observation openAIStreamProgressObservation) {
	if t == nil {
		return
	}
	t.terminalSeen = t.terminalSeen || observation.terminal
	if t.visibleSeen {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if observation.semantic {
		gap := now.Sub(t.lastSemanticAt)
		if gap > t.maxSemanticGap {
			t.maxSemanticGap = gap
		}
		t.lastSemanticAt = now
	}
	t.reasoningSeen = t.reasoningSeen || observation.reasoning
	t.visibleSeen = t.visibleSeen || observation.visible
}

func (t *openAIStreamProgressTracker) successfulSample() time.Duration {
	if t == nil || !t.terminalSeen {
		return 0
	}
	return t.maxSemanticGap
}

func normalizeOpenAIStreamProgressKey(account *Account, model string, transport OpenAIUpstreamTransport, effort string) openAIStreamProgressKey {
	accountID := int64(0)
	if account != nil {
		accountID = account.ID
	}
	return openAIStreamProgressKey{
		accountID: accountID,
		model:     strings.ToLower(strings.TrimSpace(model)),
		transport: transport,
		effort:    normalizeOpenAIReasoningEffort(effort),
	}
}

func (l *openAIStreamProgressLearner) load(key openAIStreamProgressKey) *openAIStreamProgressStat {
	if l == nil || key.accountID <= 0 {
		return nil
	}
	if value, ok := l.stats.Load(key); ok {
		stat, _ := value.(*openAIStreamProgressStat)
		return stat
	}
	for {
		count := l.count.Load()
		if count >= openAIStreamProgressMaxRoutes {
			return nil
		}
		if l.count.CompareAndSwap(count, count+1) {
			break
		}
	}
	value, loaded := l.stats.LoadOrStore(key, &openAIStreamProgressStat{})
	if loaded {
		l.count.Add(-1)
	}
	stat, _ := value.(*openAIStreamProgressStat)
	return stat
}

func (l *openAIStreamProgressLearner) observeSuccess(key openAIStreamProgressKey, sample time.Duration) {
	if sample <= 0 {
		return
	}
	stat := l.load(key)
	if stat == nil {
		return
	}
	stat.mu.Lock()
	defer stat.mu.Unlock()
	if len(stat.samples) >= openAIStreamProgressMaxSamples {
		copy(stat.samples, stat.samples[len(stat.samples)-openAIStreamProgressMaxSamples+1:])
		stat.samples = stat.samples[:openAIStreamProgressMaxSamples-1]
	}
	stat.samples = append(stat.samples, sample)
}

func (l *openAIStreamProgressLearner) deadline(key openAIStreamProgressKey, fallback time.Duration) time.Duration {
	if l == nil || key.accountID <= 0 {
		return 0
	}
	value, ok := l.stats.Load(key)
	if !ok && fallback <= 0 {
		return 0
	}
	// A legacy TTFT fallback is sufficient to derive the first deadline. Do not
	// allocate a learner route until a semantic-gap sample is actually observed;
	// otherwise high-cardinality model/effort combinations can consume the route
	// budget without ever learning a gap.
	stat, _ := value.(*openAIStreamProgressStat)
	if stat == nil {
		return clampOpenAIStreamProgressDeadline(fallback+fallback/2+2*time.Second, key.effort)
	}
	stat.mu.Lock()
	defer stat.mu.Unlock()
	if time.Now().Before(stat.suspendedUntil) {
		return 0
	}
	learned := time.Duration(0)
	if len(stat.samples) >= openAIStreamProgressMinSamples {
		samples := append([]time.Duration(nil), stat.samples...)
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		index := int(math.Ceil(float64(len(samples))*0.95)) - 1
		if index < 0 {
			index = 0
		}
		learned = samples[index] + samples[index]/4 + 2*time.Second
	} else if fallback > 0 {
		learned = fallback + fallback/2 + 2*time.Second
	}
	return clampOpenAIStreamProgressDeadline(learned, key.effort)
}

func (l *openAIStreamProgressLearner) recordActionResult(key openAIStreamProgressKey, success bool) {
	stat := l.load(key)
	if stat == nil {
		return
	}
	stat.mu.Lock()
	defer stat.mu.Unlock()
	stat.actions++
	if !success {
		stat.actionFailures++
	}
	if stat.actions >= 5 {
		if stat.actionFailures >= 3 {
			stat.suspendedUntil = time.Now().Add(10 * time.Minute)
		}
		stat.actions = 0
		stat.actionFailures = 0
	}
}

func clampOpenAIStreamProgressDeadline(value time.Duration, effort string) time.Duration {
	if value <= 0 {
		return 0
	}
	minimum, maximum := 12*time.Second, 30*time.Second
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "high", "xhigh", "max":
		minimum, maximum = 20*time.Second, 45*time.Second
	}
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func classifyOpenAIStreamProgress(payload []byte, eventType string) openAIStreamProgressObservation {
	eventType = strings.ToLower(strings.TrimSpace(eventType))
	observation := openAIStreamProgressObservation{terminal: openAIResponsesTerminalEventType(eventType) != ""}
	if eventType == "response.transport_progress.delta" || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return observation
	}
	nonEmptyDelta := func() bool {
		delta := gjson.GetBytes(payload, "delta")
		return delta.Type == gjson.String && strings.TrimSpace(delta.String()) != ""
	}
	switch eventType {
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		observation.reasoning = nonEmptyDelta()
		observation.semantic = observation.reasoning
	case "response.output_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		observation.visible = nonEmptyDelta()
		observation.semantic = observation.visible
	case "response.output_item.added":
		itemType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "item.type").String()))
		observation.visible = itemType == "function_call" || itemType == "custom_tool_call"
		observation.semantic = observation.visible
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		observation.semantic = true
	default:
		if strings.HasSuffix(eventType, ".delta") {
			observation.semantic = nonEmptyDelta()
		}
	}
	return observation
}

func (s *OpenAIGatewayService) openAIStreamSemanticStallDeadline(
	account *Account,
	model string,
	transport OpenAIUpstreamTransport,
	effort string,
) (openAIStreamProgressKey, time.Duration) {
	key := normalizeOpenAIStreamProgressKey(account, model, transport, effort)
	if s == nil || account == nil {
		return key, 0
	}
	fallback := time.Duration(0)
	if stats := s.getOpenAIAccountRuntimeStats(); stats != nil {
		_, ttft, hasTTFT, _, ttftSamples, _ := stats.snapshotForRouteWithMeta(account.ID, model, transport)
		if hasTTFT && ttftSamples >= openAIStreamProgressMinSamples && ttft > 0 {
			fallback = time.Duration(ttft * float64(time.Millisecond))
		}
	}
	return key, s.openaiStreamProgressLearner.deadline(key, fallback)
}

func (s *OpenAIGatewayService) openAIStreamSemanticStallDeadlineMS(account *Account, model string, effort *string) int {
	value := ""
	if effort != nil {
		value = *effort
	}
	_, deadline := s.openAIStreamSemanticStallDeadline(account, model, OpenAIUpstreamTransportHTTPSSE, value)
	return int(deadline / time.Millisecond)
}

func (s *OpenAIGatewayService) RecordOpenAIStreamSemanticGapSample(account *Account, model string, effort *string, sampleMS int64) {
	if s == nil || sampleMS <= 0 {
		return
	}
	value := ""
	if effort != nil {
		value = *effort
	}
	key := normalizeOpenAIStreamProgressKey(account, model, OpenAIUpstreamTransportHTTPSSE, value)
	s.openaiStreamProgressLearner.observeSuccess(key, time.Duration(sampleMS)*time.Millisecond)
}

func (s *OpenAIGatewayService) RecordOpenAIStreamStallActionResult(
	account *Account,
	model string,
	transport OpenAIUpstreamTransport,
	effort string,
	success bool,
) {
	if s == nil || account == nil {
		return
	}
	key := normalizeOpenAIStreamProgressKey(account, model, transport, effort)
	s.openaiStreamProgressLearner.recordActionResult(key, success)
}

func (s *OpenAIGatewayService) newOpenAIStreamSemanticStallFailoverError(
	c *gin.Context,
	account *Account,
) *UpstreamFailoverError {
	message := "OpenAI stream made no semantic progress before the learned deadline"
	if c != nil {
		setOpsUpstreamError(c, http.StatusGatewayTimeout, message, "")
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: http.StatusGatewayTimeout,
			Kind:               "request_local_stall_failover",
			Message:            message,
		})
	}
	body := []byte(`{"error":{"type":"upstream_timeout","message":"Upstream request failed"}}`)
	return &UpstreamFailoverError{
		StatusCode:                http.StatusGatewayTimeout,
		ResponseBody:              body,
		Message:                   message,
		RetryableOnSameAccount:    false,
		SkipPoolSoftCooldown:      true,
		SkipPromptCacheAvoidance:  true,
		SkipStickySessionEviction: true,
		SkipSchedulePenalty:       true,
		Scope:                     GatewayFailureScopeRequest,
		Reason:                    openAIStreamSemanticStallReason,
		NextAccountAction:         NextAccountRetry,
	}
}

func (s *OpenAIGatewayService) completeOpenAIStreamStallAction(c *gin.Context, success bool) {
	if s == nil {
		return
	}
	coordinator := openAIPlaceholderCoordinatorFromContext(c)
	if key, ok := coordinator.completeStallAction(success); ok {
		s.openaiStreamProgressLearner.recordActionResult(key, success)
	}
}

func (s *OpenAIGatewayService) CompleteOpenAIStreamStallAction(c *gin.Context, success bool) {
	s.completeOpenAIStreamStallAction(c, success)
}
