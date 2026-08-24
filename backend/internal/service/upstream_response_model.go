package service

import (
	"bytes"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	upstreamResponseModelObserverContextKey = "upstream_response_model_observer"
	upstreamResponseModelMaxLength          = 200
)

// upstreamResponseModelObserver is request-local diagnostic state. It never
// changes routing, billing, cache behavior, or the downstream response.
type upstreamResponseModelObserver struct {
	mu          sync.RWMutex
	first       string
	terminal    string
	serviceTier string
	conflict    bool
}

func (o *upstreamResponseModelObserver) Observe(model string, terminal bool) {
	if o == nil {
		return
	}
	model = normalizeObservedUpstreamResponseModel(model)
	if model == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if current := o.modelLocked(); current != "" && !strings.EqualFold(current, model) {
		o.conflict = true
	}
	if terminal {
		o.terminal = model
		return
	}
	if o.first == "" {
		o.first = model
	}
}

func (o *upstreamResponseModelObserver) Model() string {
	if o == nil {
		return ""
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.modelLocked()
}

func (o *upstreamResponseModelObserver) modelLocked() string {
	if o.terminal != "" {
		return o.terminal
	}
	return o.first
}

func (o *upstreamResponseModelObserver) Conflict() bool {
	if o == nil {
		return false
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.conflict
}

func (o *upstreamResponseModelObserver) beginTurn() {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.first = ""
	o.terminal = ""
	o.serviceTier = ""
	o.conflict = false
	o.mu.Unlock()
}

func normalizeObservedUpstreamResponseModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	runes := []rune(model)
	if len(runes) > upstreamResponseModelMaxLength {
		return string(runes[:upstreamResponseModelMaxLength])
	}
	return model
}

func firstTrimmedGJSONModel(values ...gjson.Result) string {
	for _, value := range values {
		if value.Exists() && value.Type == gjson.String {
			if model := strings.TrimSpace(value.String()); model != "" {
				return model
			}
		}
	}
	return ""
}

func (o *upstreamResponseModelObserver) ObserveOpenAI(payload []byte, eventType string) {
	if o == nil || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return
	}
	if strings.TrimSpace(eventType) == "response.created" {
		o.beginTurn()
	}
	if !bytes.Contains(payload, []byte(`"model"`)) {
		return
	}
	model := firstTrimmedGJSONModel(gjson.GetBytes(payload, "response.model"), gjson.GetBytes(payload, "model"))
	o.Observe(model, isUpstreamResponseModelTerminalEvent(eventType))
	if model != "" && strings.TrimSpace(eventType) != "response.created" && (strings.TrimSpace(eventType) == "" || isUpstreamResponseModelTerminalEvent(eventType)) {
		tier := firstTrimmedGJSONModel(gjson.GetBytes(payload, "response.service_tier"), gjson.GetBytes(payload, "service_tier"))
		if tier != "" {
			o.mu.Lock()
			o.serviceTier = strings.ToLower(tier)
			o.mu.Unlock()
		}
	}
}

func observedUpstreamResponseServiceTier(c *gin.Context) string {
	if o := upstreamResponseModelObserverFromContext(c); o != nil {
		o.mu.RLock()
		defer o.mu.RUnlock()
		return o.serviceTier
	}
	return ""
}

func ResolveBillingServiceTier(requested, observed string) string {
	normalize := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "fast" {
			return "priority"
		}
		return value
	}
	requested, observed = normalize(requested), normalize(observed)
	rank := func(value string) (int, bool) {
		switch value {
		case "flex":
			return 0, true
		case "", "default", "standard", "auto", "scale":
			return 1, true
		case "priority":
			return 2, true
		default:
			return 1, false
		}
	}
	observedRank, known := rank(observed)
	requestedRank, _ := rank(requested)
	if known && observed != "" && observedRank < requestedRank {
		return observed
	}
	return requested
}

func ApplyObservedOpenAIServiceTier(c *gin.Context, result *OpenAIForwardResult) {
	if result == nil {
		return
	}
	requested := ""
	if result.ServiceTier != nil {
		requested = *result.ServiceTier
	}
	billing := ResolveBillingServiceTier(requested, observedUpstreamResponseServiceTier(c))
	if billing != strings.TrimSpace(requested) {
		result.ServiceTier = &billing
	}
}

func (o *upstreamResponseModelObserver) ObserveAnthropic(payload []byte) {
	if o == nil || len(payload) == 0 || !bytes.Contains(payload, []byte(`"model"`)) || !gjson.ValidBytes(payload) {
		return
	}
	o.Observe(firstTrimmedGJSONModel(gjson.GetBytes(payload, "message.model"), gjson.GetBytes(payload, "model")), false)
}

func (o *upstreamResponseModelObserver) ObserveGemini(payload []byte) {
	if o == nil || len(payload) == 0 || (!bytes.Contains(payload, []byte(`"modelVersion"`)) && !bytes.Contains(payload, []byte(`"response"`))) || !gjson.ValidBytes(payload) {
		return
	}
	o.Observe(firstTrimmedGJSONModel(gjson.GetBytes(payload, "modelVersion"), gjson.GetBytes(payload, "response.modelVersion")), true)
}

func isUpstreamResponseModelTerminalEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}

func beginUpstreamResponseModelObservation(c *gin.Context) *upstreamResponseModelObserver {
	if existing := upstreamResponseModelObserverFromContext(c); existing != nil {
		return existing
	}
	o := &upstreamResponseModelObserver{}
	if c != nil {
		c.Set(upstreamResponseModelObserverContextKey, o)
	}
	return o
}

func upstreamResponseModelObserverFromContext(c *gin.Context) *upstreamResponseModelObserver {
	if c == nil {
		return nil
	}
	v, ok := c.Get(upstreamResponseModelObserverContextKey)
	if !ok {
		return nil
	}
	o, _ := v.(*upstreamResponseModelObserver)
	return o
}

func observedUpstreamResponseModel(c *gin.Context) string {
	if o := upstreamResponseModelObserverFromContext(c); o != nil {
		return o.Model()
	}
	return ""
}

func observedUpstreamResponseModelConflict(c *gin.Context) bool {
	if o := upstreamResponseModelObserverFromContext(c); o != nil {
		return o.Conflict()
	}
	return false
}

func upstreamSentModel(requestedModel, upstreamModel string) string {
	if model := strings.TrimSpace(upstreamModel); model != "" {
		return model
	}
	return strings.TrimSpace(requestedModel)
}

func upstreamModelMismatch(sentModel, responseModel string) *bool {
	responseModel = strings.TrimSpace(responseModel)
	if responseModel == "" {
		return nil
	}
	mismatch := strings.TrimSpace(sentModel) == "" || !strings.EqualFold(strings.TrimSpace(sentModel), responseModel)
	return &mismatch
}

func upstreamModelMismatchWithConflict(sentModel, responseModel string, conflict bool) *bool {
	if conflict {
		value := true
		return &value
	}
	return upstreamModelMismatch(sentModel, responseModel)
}

func observeOpenAISSEBody(observer *upstreamResponseModelObserver, body string) {
	if observer == nil {
		return
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" || !gjson.Valid(payload) {
			continue
		}
		eventType := strings.TrimSpace(gjson.Get(payload, "type").String())
		observer.ObserveOpenAI([]byte(payload), eventType)
	}
}
