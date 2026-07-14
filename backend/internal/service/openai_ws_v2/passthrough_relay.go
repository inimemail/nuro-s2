package openai_ws_v2

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

var relayUpstreamEndpointPattern = regexp.MustCompile(`(?i)(?:https?|wss?)://|(?:[a-z0-9-]+\.)+[a-z]{2,24}(?::\d+)?`)

type FrameConn interface {
	ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error)
	WriteFrame(ctx context.Context, msgType coderws.MessageType, payload []byte) error
	Close() error
}

type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	ImageOutputTokens        int
}

type RelayResult struct {
	RequestModel            string
	Usage                   Usage
	RequestID               string
	TerminalEventType       string
	FirstTokenMs            *int
	Duration                time.Duration
	ClientToUpstreamFrames  int64
	UpstreamToClientFrames  int64
	DroppedDownstreamFrames int64
	ClientDisconnected      bool
	CyberBlocked            bool
}

type RelayTurnResult struct {
	RequestModel       string
	Usage              Usage
	RequestID          string
	TerminalEventType  string
	Duration           time.Duration
	FirstTokenMs       *int
	ClientDisconnected bool
	CyberBlocked       bool
}

type RelayExit struct {
	Stage           string
	Err             error
	WroteDownstream bool
}

type RelayOptions struct {
	WriteTimeout                    time.Duration
	IdleTimeout                     time.Duration
	UpstreamDrainTimeout            time.Duration
	FirstMessageType                coderws.MessageType
	FirstMessageSent                bool
	StartClientAfterFirstDownstream bool
	OnUsageParseFailure             func(eventType string, usageRaw string)
	OnTurnComplete                  func(turn RelayTurnResult)
	BeforeWriteClient               func(msgType coderws.MessageType, payload []byte, wroteDownstream bool) error
	ReadClientFrame                 func(ctx context.Context, clientConn FrameConn) (coderws.MessageType, []byte, error)
	OnTrace                         func(event RelayTraceEvent)
	Now                             func() time.Time
}

type RelayTraceEvent struct {
	Stage           string
	Direction       string
	MessageType     string
	PayloadBytes    int
	Graceful        bool
	WroteDownstream bool
	Error           string
}

type relayState struct {
	usage             Usage
	requestModel      string
	lastResponseID    string
	terminalEventType string
	firstTokenMs      *int
	turnTimingByID    map[string]*relayTurnTiming
	activeTurn        *relayTurnTiming
	cyberBlocked      bool
	turnFailed        bool
	turnCyberBlocked  bool
	terminalByID      map[string]string
}

type relayExitSignal struct {
	stage           string
	err             error
	graceful        bool
	wroteDownstream bool
}

type observedUpstreamEvent struct {
	terminal     bool
	eventType    string
	responseID   string
	usage        Usage
	duration     time.Duration
	firstToken   *int
	cyberBlocked bool
	dropClient   bool
}

type relayTurnTiming struct {
	startAt      time.Time
	firstTokenMs *int
}

func Relay(
	ctx context.Context,
	clientConn FrameConn,
	upstreamConn FrameConn,
	firstClientMessage []byte,
	options RelayOptions,
) (RelayResult, *RelayExit) {
	result := RelayResult{RequestModel: strings.TrimSpace(gjson.GetBytes(firstClientMessage, "model").String())}
	if clientConn == nil || upstreamConn == nil {
		return result, &RelayExit{Stage: "relay_init", Err: errors.New("relay connection is nil")}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	nowFn := options.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	writeTimeout := options.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = 2 * time.Minute
	}
	drainTimeout := options.UpstreamDrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = 1200 * time.Millisecond
	}
	firstMessageType := options.FirstMessageType
	if firstMessageType != coderws.MessageBinary {
		firstMessageType = coderws.MessageText
	}
	startAt := nowFn()
	state := &relayState{requestModel: result.RequestModel}
	onTrace := options.OnTrace

	relayCtx, relayCancel := context.WithCancel(ctx)
	defer relayCancel()

	lastActivity := atomic.Int64{}
	lastActivity.Store(nowFn().UnixNano())
	markActivity := func() {
		lastActivity.Store(nowFn().UnixNano())
	}

	writeUpstream := func(msgType coderws.MessageType, payload []byte) error {
		writeCtx, cancel := context.WithTimeout(relayCtx, writeTimeout)
		defer cancel()
		return upstreamConn.WriteFrame(writeCtx, msgType, payload)
	}
	writeClient := func(msgType coderws.MessageType, payload []byte) error {
		writeCtx, cancel := context.WithTimeout(relayCtx, writeTimeout)
		defer cancel()
		return clientConn.WriteFrame(writeCtx, msgType, payload)
	}

	clientToUpstreamFrames := &atomic.Int64{}
	upstreamToClientFrames := &atomic.Int64{}
	droppedDownstreamFrames := &atomic.Int64{}
	emitRelayTrace(onTrace, RelayTraceEvent{
		Stage:        "relay_start",
		PayloadBytes: len(firstClientMessage),
		MessageType:  relayMessageTypeString(firstMessageType),
	})

	if options.FirstMessageSent {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:        "write_first_message_skipped",
			Direction:    "client_to_upstream",
			MessageType:  relayMessageTypeString(firstMessageType),
			PayloadBytes: len(firstClientMessage),
		})
	} else {
		if err := writeUpstream(firstMessageType, firstClientMessage); err != nil {
			result.Duration = nowFn().Sub(startAt)
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:        "write_first_message_failed",
				Direction:    "client_to_upstream",
				MessageType:  relayMessageTypeString(firstMessageType),
				PayloadBytes: len(firstClientMessage),
				Error:        err.Error(),
			})
			return result, &RelayExit{Stage: "write_upstream", Err: err}
		}
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:        "write_first_message_ok",
			Direction:    "client_to_upstream",
			MessageType:  relayMessageTypeString(firstMessageType),
			PayloadBytes: len(firstClientMessage),
		})
	}
	clientToUpstreamFrames.Add(1)
	markActivity()

	exitCh := make(chan relayExitSignal, 3)
	dropDownstreamWrites := atomic.Bool{}
	clientReaderStarted := atomic.Bool{}
	startClientReader := func() {
		if !clientReaderStarted.CompareAndSwap(false, true) {
			return
		}
		go runClientToUpstream(relayCtx, clientConn, options.ReadClientFrame, writeUpstream, markActivity, clientToUpstreamFrames, onTrace, exitCh)
	}
	if !options.StartClientAfterFirstDownstream {
		startClientReader()
	}
	go runUpstreamToClient(
		relayCtx,
		upstreamConn,
		writeClient,
		startAt,
		nowFn,
		state,
		options.OnUsageParseFailure,
		options.OnTurnComplete,
		options.BeforeWriteClient,
		func() {
			if options.StartClientAfterFirstDownstream {
				startClientReader()
			}
		},
		&dropDownstreamWrites,
		upstreamToClientFrames,
		droppedDownstreamFrames,
		markActivity,
		onTrace,
		exitCh,
	)
	go runIdleWatchdog(relayCtx, nowFn, options.IdleTimeout, &lastActivity, onTrace, exitCh)

	firstExit := <-exitCh
	emitRelayTrace(onTrace, RelayTraceEvent{
		Stage:           "first_exit",
		Direction:       relayDirectionFromStage(firstExit.stage),
		Graceful:        firstExit.graceful,
		WroteDownstream: firstExit.wroteDownstream,
		Error:           relayErrorString(firstExit.err),
	})
	combinedWroteDownstream := firstExit.wroteDownstream
	secondExit := relayExitSignal{graceful: true}
	hasSecondExit := false

	// 客户端断开后尽力继续读取上游短窗口，捕获延迟 usage/terminal 事件用于计费。
	if firstExit.stage == "read_client" && firstExit.graceful {
		dropDownstreamWrites.Store(true)
		secondExit, hasSecondExit = waitRelayExit(exitCh, drainTimeout)
	} else {
		relayCancel()
		_ = upstreamConn.Close()
		if clientReaderStarted.Load() {
			secondExit, hasSecondExit = waitRelayExit(exitCh, 200*time.Millisecond)
		}
	}
	if hasSecondExit {
		combinedWroteDownstream = combinedWroteDownstream || secondExit.wroteDownstream
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "second_exit",
			Direction:       relayDirectionFromStage(secondExit.stage),
			Graceful:        secondExit.graceful,
			WroteDownstream: secondExit.wroteDownstream,
			Error:           relayErrorString(secondExit.err),
		})
	}

	relayCancel()
	_ = upstreamConn.Close()

	enrichResult(&result, state, nowFn().Sub(startAt))
	result.ClientToUpstreamFrames = clientToUpstreamFrames.Load()
	result.UpstreamToClientFrames = upstreamToClientFrames.Load()
	result.DroppedDownstreamFrames = droppedDownstreamFrames.Load()
	if firstExit.stage == "read_client" && firstExit.graceful {
		result.ClientDisconnected = true
	}
	if options.FirstMessageSent && firstExit.stage == "read_client" && firstExit.graceful {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_client_closed",
			Graceful:        true,
			WroteDownstream: combinedWroteDownstream,
		})
		return result, nil
	}
	if firstExit.stage == "read_client" && firstExit.graceful {
		stage := "client_disconnected"
		exitErr := firstExit.err
		if hasSecondExit && !secondExit.graceful {
			stage = secondExit.stage
			exitErr = secondExit.err
		}
		if exitErr == nil {
			exitErr = io.EOF
		}
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_exit",
			Direction:       relayDirectionFromStage(stage),
			Graceful:        false,
			WroteDownstream: combinedWroteDownstream,
			Error:           relayErrorString(exitErr),
		})
		return result, &RelayExit{
			Stage:           stage,
			Err:             exitErr,
			WroteDownstream: combinedWroteDownstream,
		}
	}
	if firstExit.graceful && (!hasSecondExit || secondExit.graceful) {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_complete",
			Graceful:        true,
			WroteDownstream: combinedWroteDownstream,
		})
		_ = clientConn.Close()
		return result, nil
	}
	if !firstExit.graceful {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_exit",
			Direction:       relayDirectionFromStage(firstExit.stage),
			Graceful:        false,
			WroteDownstream: combinedWroteDownstream,
			Error:           relayErrorString(firstExit.err),
		})
		return result, &RelayExit{
			Stage:           firstExit.stage,
			Err:             firstExit.err,
			WroteDownstream: combinedWroteDownstream,
		}
	}
	if hasSecondExit && !secondExit.graceful {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_exit",
			Direction:       relayDirectionFromStage(secondExit.stage),
			Graceful:        false,
			WroteDownstream: combinedWroteDownstream,
			Error:           relayErrorString(secondExit.err),
		})
		return result, &RelayExit{
			Stage:           secondExit.stage,
			Err:             secondExit.err,
			WroteDownstream: combinedWroteDownstream,
		}
	}
	if options.FirstMessageSent {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_client_closed",
			Graceful:        true,
			WroteDownstream: combinedWroteDownstream,
		})
		return result, nil
	}
	emitRelayTrace(onTrace, RelayTraceEvent{
		Stage:           "relay_complete",
		Graceful:        true,
		WroteDownstream: combinedWroteDownstream,
	})
	_ = clientConn.Close()
	return result, nil
}

func runClientToUpstream(
	ctx context.Context,
	clientConn FrameConn,
	readClientFrame func(context.Context, FrameConn) (coderws.MessageType, []byte, error),
	writeUpstream func(msgType coderws.MessageType, payload []byte) error,
	markActivity func(),
	forwardedFrames *atomic.Int64,
	onTrace func(event RelayTraceEvent),
	exitCh chan<- relayExitSignal,
) {
	if readClientFrame == nil {
		readClientFrame = func(ctx context.Context, conn FrameConn) (coderws.MessageType, []byte, error) {
			return conn.ReadFrame(ctx)
		}
	}
	for {
		msgType, payload, err := readClientFrame(ctx, clientConn)
		if err != nil {
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:     "read_client_failed",
				Direction: "client_to_upstream",
				Error:     err.Error(),
				Graceful:  isDisconnectError(err),
			})
			exitCh <- relayExitSignal{stage: "read_client", err: err, graceful: isDisconnectError(err)}
			return
		}
		markActivity()
		if err := writeUpstream(msgType, payload); err != nil {
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:        "write_upstream_failed",
				Direction:    "client_to_upstream",
				MessageType:  relayMessageTypeString(msgType),
				PayloadBytes: len(payload),
				Error:        err.Error(),
			})
			exitCh <- relayExitSignal{stage: "write_upstream", err: err}
			return
		}
		if forwardedFrames != nil {
			forwardedFrames.Add(1)
		}
		markActivity()
	}
}

func runUpstreamToClient(
	ctx context.Context,
	upstreamConn FrameConn,
	writeClient func(msgType coderws.MessageType, payload []byte) error,
	startAt time.Time,
	nowFn func() time.Time,
	state *relayState,
	onUsageParseFailure func(eventType string, usageRaw string),
	onTurnComplete func(turn RelayTurnResult),
	beforeWriteClient func(msgType coderws.MessageType, payload []byte, wroteDownstream bool) error,
	afterWriteClient func(),
	dropDownstreamWrites *atomic.Bool,
	forwardedFrames *atomic.Int64,
	droppedFrames *atomic.Int64,
	markActivity func(),
	onTrace func(event RelayTraceEvent),
	exitCh chan<- relayExitSignal,
) {
	wroteDownstream := false
	for {
		msgType, payload, err := upstreamConn.ReadFrame(ctx)
		if err != nil {
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:           "read_upstream_failed",
				Direction:       "upstream_to_client",
				Error:           err.Error(),
				Graceful:        isDisconnectError(err),
				WroteDownstream: wroteDownstream,
			})
			exitCh <- relayExitSignal{
				stage:           "read_upstream",
				err:             err,
				graceful:        isDisconnectError(err),
				wroteDownstream: wroteDownstream,
			}
			return
		}
		markActivity()
		if beforeWriteClient != nil {
			if err := beforeWriteClient(msgType, payload, wroteDownstream); err != nil {
				emitRelayTrace(onTrace, RelayTraceEvent{
					Stage:           "upstream_message_rejected",
					Direction:       "upstream_to_client",
					MessageType:     relayMessageTypeString(msgType),
					PayloadBytes:    len(payload),
					WroteDownstream: wroteDownstream,
					Error:           err.Error(),
				})
				exitCh <- relayExitSignal{
					stage:           "upstream_message",
					err:             err,
					wroteDownstream: wroteDownstream,
				}
				return
			}
		}
		observedEvent := observedUpstreamEvent{}
		switch msgType {
		case coderws.MessageText, coderws.MessageBinary:
			observedEvent = observeUpstreamMessage(state, payload, startAt, nowFn, onUsageParseFailure)
		}
		if observedEvent.dropClient {
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:           "drop_duplicate_terminal",
				Direction:       "upstream_to_client",
				MessageType:     relayMessageTypeString(msgType),
				PayloadBytes:    len(payload),
				WroteDownstream: wroteDownstream,
			})
			markActivity()
			continue
		}
		if dropDownstreamWrites != nil && dropDownstreamWrites.Load() {
			emitTurnComplete(onTurnComplete, state, observedEvent, true)
			if droppedFrames != nil {
				droppedFrames.Add(1)
			}
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:           "drop_downstream_frame",
				Direction:       "upstream_to_client",
				MessageType:     relayMessageTypeString(msgType),
				PayloadBytes:    len(payload),
				WroteDownstream: wroteDownstream,
			})
			if observedEvent.terminal {
				exitCh <- relayExitSignal{
					stage:           "drain_terminal",
					graceful:        true,
					wroteDownstream: wroteDownstream,
				}
				return
			}
			markActivity()
			continue
		}
		clientPayload := payload
		if msgType == coderws.MessageText || msgType == coderws.MessageBinary {
			clientPayload = sanitizeUpstreamErrorEventForClient(payload)
			// Once an unsafe frame has failed the current turn, a later nominal
			// terminal frame must not tell the client (or accounting hooks) that
			// the same turn completed successfully.
			if observedEvent.eventType == "error" && isTerminalEvent(strings.TrimSpace(gjson.GetBytes(payload, "type").String())) {
				clientPayload = []byte(`{"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}`)
			}
		}
		if err := writeClient(msgType, clientPayload); err != nil {
			// Preserve terminal usage for billing, but keep the turn health-neutral
			// when the client did not receive the terminal frame.
			emitTurnComplete(onTurnComplete, state, observedEvent, true)
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:           "write_client_failed",
				Direction:       "upstream_to_client",
				MessageType:     relayMessageTypeString(msgType),
				PayloadBytes:    len(clientPayload),
				WroteDownstream: wroteDownstream,
				Error:           err.Error(),
			})
			exitCh <- relayExitSignal{stage: "write_client", err: err, wroteDownstream: wroteDownstream}
			return
		}
		emitTurnComplete(onTurnComplete, state, observedEvent, false)
		wroteDownstream = true
		if afterWriteClient != nil {
			afterWriteClient()
		}
		if forwardedFrames != nil {
			forwardedFrames.Add(1)
		}
		markActivity()
	}
}

func sanitizeUpstreamErrorEventForClient(payload []byte) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return []byte(`{"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}`)
	}
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	safeError := map[string]any{
		"type":    "upstream_error",
		"message": "Upstream request failed",
	}
	switch eventType {
	case "error":
		updated, err := json.Marshal(map[string]any{
			"type":  "error",
			"error": safeError,
		})
		if err == nil {
			return updated
		}
	case "response.failed":
		event := map[string]any{"type": "response.failed"}
		if sequenceNumber := gjson.GetBytes(payload, "sequence_number"); sequenceNumber.Exists() && sequenceNumber.Type == gjson.Number {
			event["sequence_number"] = sequenceNumber.Value()
		}
		response := map[string]any{
			"status": "failed",
			"error":  safeError,
		}
		if responseResult := gjson.GetBytes(payload, "response"); responseResult.Exists() && responseResult.IsObject() {
			if value := gjson.Get(responseResult.Raw, "id"); value.Type == gjson.String && relayErrorIDIsSafe(value.String()) {
				response["id"] = value.String()
			}
			if value := gjson.Get(responseResult.Raw, "object"); value.Type == gjson.String && value.String() == "response" {
				response["object"] = "response"
			}
			if value := gjson.Get(responseResult.Raw, "created_at"); value.Exists() && value.Type == gjson.Number {
				response["created_at"] = value.Value()
			}
			if value := gjson.Get(responseResult.Raw, "model"); value.Type == gjson.String && relayErrorMetadataIsSafe(value.String()) {
				response["model"] = value.String()
			}
		}
		event["response"] = response
		updated, err := json.Marshal(event)
		if err == nil {
			return updated
		}
	default:
		if relayUpstreamPayloadIsUnsafe(payload) {
			return []byte(`{"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}`)
		}
		return payload
	}
	return []byte(`{"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}`)
}

func relayUpstreamPayloadIsUnsafe(payload []byte) bool {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || !gjson.Valid(trimmed) || !gjson.Parse(trimmed).IsObject() {
		return true
	}
	eventType := strings.ToLower(strings.TrimSpace(gjson.Get(trimmed, "type").String()))
	if eventType == "error" || eventType == "response.failed" {
		return true
	}
	for _, path := range []string{"error", "response.error"} {
		value := gjson.Get(trimmed, path)
		if value.Exists() && value.Type != gjson.Null {
			return true
		}
	}
	for _, path := range []string{"message", "detail", "reason", "response.message", "response.detail", "response.reason"} {
		value := gjson.Get(trimmed, path)
		if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			return true
		}
	}
	for _, path := range []string{
		"provider", "upstream", "upstream_url", "base_url", "host", "hostname", "server", "traceback", "stack",
		"response.provider", "response.upstream", "response.upstream_url", "response.base_url", "response.host",
		"response.hostname", "response.server", "response.traceback", "response.stack",
	} {
		value := gjson.Get(trimmed, path)
		if value.Exists() && value.Type != gjson.Null && strings.TrimSpace(value.String()) != "" {
			return true
		}
	}
	for _, path := range []string{"status", "response.status"} {
		switch strings.ToLower(strings.TrimSpace(gjson.Get(trimmed, path).String())) {
		case "error", "failed", "failure":
			return true
		}
	}
	for _, path := range []string{"success", "response.success"} {
		if success := gjson.Get(trimmed, path); success.Exists() && success.Type == gjson.False {
			return true
		}
	}
	for _, prefix := range []string{"data", "result"} {
		envelope := gjson.Get(trimmed, prefix)
		if !envelope.IsObject() {
			continue
		}
		for _, field := range []string{"error", "response.error"} {
			value := gjson.Get(trimmed, prefix+"."+field)
			if value.Exists() && value.Type != gjson.Null {
				return true
			}
		}
		for _, field := range []string{"message", "detail", "reason"} {
			value := gjson.Get(trimmed, prefix+"."+field)
			if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
				return true
			}
		}
		for _, field := range []string{
			"provider", "upstream", "upstream_url", "base_url", "host", "hostname", "server", "traceback", "stack",
		} {
			value := gjson.Get(trimmed, prefix+"."+field)
			if value.Exists() && value.Type != gjson.Null && strings.TrimSpace(value.String()) != "" {
				return true
			}
		}
		switch strings.ToLower(strings.TrimSpace(gjson.Get(trimmed, prefix+".status").String())) {
		case "error", "failed", "failure":
			return true
		}
		if success := gjson.Get(trimmed, prefix+".success"); success.Exists() && success.Type == gjson.False {
			return true
		}
	}
	return false
}

func relayErrorMetadataIsSafe(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "<>\r\n") {
		return false
	}
	lower := strings.ToLower(value)
	return !strings.Contains(lower, "cloudflare") && !strings.Contains(lower, "cf-ray") && !relayUpstreamEndpointPattern.MatchString(value)
}

func relayErrorIDIsSafe(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func runIdleWatchdog(
	ctx context.Context,
	nowFn func() time.Time,
	idleTimeout time.Duration,
	lastActivity *atomic.Int64,
	onTrace func(event RelayTraceEvent),
	exitCh chan<- relayExitSignal,
) {
	if idleTimeout <= 0 {
		return
	}
	checkInterval := minDuration(idleTimeout/4, 5*time.Second)
	if checkInterval < time.Second {
		checkInterval = time.Second
	}
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := time.Unix(0, lastActivity.Load())
			if nowFn().Sub(last) < idleTimeout {
				continue
			}
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:     "idle_timeout_triggered",
				Direction: "watchdog",
				Error:     context.DeadlineExceeded.Error(),
			})
			exitCh <- relayExitSignal{stage: "idle_timeout", err: context.DeadlineExceeded}
			return
		}
	}
}

func emitRelayTrace(onTrace func(event RelayTraceEvent), event RelayTraceEvent) {
	if onTrace == nil {
		return
	}
	onTrace(event)
}

func relayMessageTypeString(msgType coderws.MessageType) string {
	switch msgType {
	case coderws.MessageText:
		return "text"
	case coderws.MessageBinary:
		return "binary"
	default:
		return "unknown(" + strconv.Itoa(int(msgType)) + ")"
	}
}

func relayDirectionFromStage(stage string) string {
	switch stage {
	case "read_client", "write_upstream":
		return "client_to_upstream"
	case "read_upstream", "write_client", "drain_terminal":
		return "upstream_to_client"
	case "idle_timeout":
		return "watchdog"
	default:
		return ""
	}
}

func relayErrorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func observeUpstreamMessage(
	state *relayState,
	message []byte,
	startAt time.Time,
	nowFn func() time.Time,
	onUsageParseFailure func(eventType string, usageRaw string),
) observedUpstreamEvent {
	if state == nil || len(message) == 0 {
		return observedUpstreamEvent{}
	}
	unsafePayload := relayUpstreamPayloadIsUnsafe(message)
	if unsafePayload {
		state.turnFailed = true
		state.terminalEventType = "error"
	}
	values := gjson.GetManyBytes(message, "type", "response.id", "response_id", "id")
	eventType := strings.TrimSpace(values[0].String())
	if eventType == "" {
		if unsafePayload {
			return observedUpstreamEvent{eventType: "error"}
		}
		return observedUpstreamEvent{}
	}
	responseID := strings.TrimSpace(values[1].String())
	if responseID == "" {
		responseID = strings.TrimSpace(values[2].String())
	}
	// 仅 terminal 事件兜底读取顶层 id，避免把 event_id 当成 response_id 关联到 turn。
	if responseID == "" && isTerminalEvent(eventType) {
		responseID = strings.TrimSpace(values[3].String())
	}
	now := nowFn()
	if isTerminalEvent(eventType) && responseID != "" {
		if previousTerminal, exists := state.terminalByID[responseID]; exists {
			return observedUpstreamEvent{
				eventType:  previousTerminal,
				responseID: responseID,
				dropClient: true,
			}
		}
	}

	if state.firstTokenMs == nil && isTokenEvent(eventType) {
		ms := int(now.Sub(startAt).Milliseconds())
		if ms >= 0 {
			state.firstTokenMs = &ms
		}
		if state.activeTurn != nil && state.activeTurn.firstTokenMs == nil {
			tms := int(now.Sub(state.activeTurn.startAt).Milliseconds())
			if tms >= 0 {
				state.activeTurn.firstTokenMs = &tms
			}
		}
	}
	parsedUsage := parseUsageAndAccumulate(state, message, eventType, onUsageParseFailure)
	cyberBlocked := isOpenAIWSCyberPolicyPayload(message)
	if cyberBlocked {
		state.turnCyberBlocked = true
		state.cyberBlocked = true
	}
	observed := observedUpstreamEvent{
		eventType:    eventType,
		responseID:   responseID,
		usage:        parsedUsage,
		cyberBlocked: cyberBlocked,
	}
	if responseID != "" {
		turnTiming := openAIWSRelayGetOrInitTurnTiming(state, responseID, now)
		if turnTiming != nil && turnTiming.firstTokenMs == nil && isTokenEvent(eventType) {
			ms := int(now.Sub(turnTiming.startAt).Milliseconds())
			if ms >= 0 {
				turnTiming.firstTokenMs = &ms
			}
		}
	}
	if !isTerminalEvent(eventType) {
		if unsafePayload {
			observed.eventType = "error"
		}
		return observed
	}
	observed.terminal = true
	observed.cyberBlocked = observed.cyberBlocked || state.turnCyberBlocked
	if state.turnFailed && (eventType == "response.completed" || eventType == "response.done") {
		observed.eventType = "error"
	}
	if responseID != "" {
		if state.terminalByID == nil {
			state.terminalByID = make(map[string]string, 8)
		} else if len(state.terminalByID) >= 256 {
			clear(state.terminalByID)
		}
		state.terminalByID[responseID] = observed.eventType
	}
	state.terminalEventType = observed.eventType
	state.cyberBlocked = observed.cyberBlocked
	state.turnFailed = false
	state.turnCyberBlocked = false
	if responseID != "" {
		state.lastResponseID = responseID
		if turnTiming, ok := openAIWSRelayDeleteTurnTiming(state, responseID); ok {
			duration := now.Sub(turnTiming.startAt)
			if duration < 0 {
				duration = 0
			}
			observed.duration = duration
			observed.firstToken = openAIWSRelayCloneIntPtr(turnTiming.firstTokenMs)
		}
	}
	return observed
}

func emitTurnComplete(
	onTurnComplete func(turn RelayTurnResult),
	state *relayState,
	observed observedUpstreamEvent,
	clientDisconnected bool,
) {
	if onTurnComplete == nil || !observed.terminal {
		return
	}
	responseID := strings.TrimSpace(observed.responseID)
	if responseID == "" {
		return
	}
	requestModel := ""
	if state != nil {
		requestModel = state.requestModel
	}
	onTurnComplete(RelayTurnResult{
		RequestModel:       requestModel,
		Usage:              observed.usage,
		RequestID:          responseID,
		TerminalEventType:  observed.eventType,
		Duration:           observed.duration,
		FirstTokenMs:       openAIWSRelayCloneIntPtr(observed.firstToken),
		ClientDisconnected: clientDisconnected,
		CyberBlocked:       observed.cyberBlocked,
	})
}

func openAIWSRelayGetOrInitTurnTiming(state *relayState, responseID string, now time.Time) *relayTurnTiming {
	if state == nil {
		return nil
	}
	if state.turnTimingByID == nil {
		state.turnTimingByID = make(map[string]*relayTurnTiming, 8)
	}
	timing, ok := state.turnTimingByID[responseID]
	if !ok || timing == nil || timing.startAt.IsZero() {
		timing = &relayTurnTiming{startAt: now}
		state.turnTimingByID[responseID] = timing
		state.activeTurn = timing
		return timing
	}
	return timing
}

func openAIWSRelayDeleteTurnTiming(state *relayState, responseID string) (relayTurnTiming, bool) {
	if state == nil || state.turnTimingByID == nil {
		return relayTurnTiming{}, false
	}
	timing, ok := state.turnTimingByID[responseID]
	if !ok || timing == nil {
		return relayTurnTiming{}, false
	}
	delete(state.turnTimingByID, responseID)
	if state.activeTurn == timing {
		state.activeTurn = nil
	}
	return *timing, true
}

func openAIWSRelayCloneIntPtr(v *int) *int {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}

func parseUsageAndAccumulate(
	state *relayState,
	message []byte,
	eventType string,
	onParseFailure func(eventType string, usageRaw string),
) Usage {
	if state == nil || len(message) == 0 || !shouldParseUsage(eventType) {
		return Usage{}
	}
	usageResult := gjson.GetBytes(message, "response.usage")
	if !usageResult.Exists() {
		return Usage{}
	}
	usageRaw := strings.TrimSpace(usageResult.Raw)
	if usageRaw == "" || !strings.HasPrefix(usageRaw, "{") {
		recordUsageParseFailure()
		if onParseFailure != nil {
			onParseFailure(eventType, usageRaw)
		}
		return Usage{}
	}

	inputResult := gjson.GetBytes(message, "response.usage.input_tokens")
	if !inputResult.Exists() {
		inputResult = gjson.GetBytes(message, "response.usage.prompt_tokens")
	}
	outputResult := gjson.GetBytes(message, "response.usage.output_tokens")
	if !outputResult.Exists() {
		outputResult = gjson.GetBytes(message, "response.usage.completion_tokens")
	}
	cachedResult := gjson.GetBytes(message, "response.usage.input_tokens_details.cached_tokens")
	if !cachedResult.Exists() {
		cachedResult = gjson.GetBytes(message, "response.usage.prompt_tokens_details.cached_tokens")
	}
	imageTokens := usageResult.Get("output_tokens_details.image_tokens").Int()
	if imageTokens == 0 {
		imageTokens = usageResult.Get("completion_tokens_details.image_tokens").Int()
	}

	inputTokens, inputOK := parseUsageIntField(inputResult, true)
	outputTokens, outputOK := parseUsageIntField(outputResult, true)
	cachedTokens, cachedOK := parseUsageIntField(cachedResult, false)
	if !inputOK || !outputOK || !cachedOK {
		recordUsageParseFailure()
		if onParseFailure != nil {
			onParseFailure(eventType, usageRaw)
		}
		// 解析失败时不做部分字段累加，避免计费 usage 出现“半有效”状态。
		return Usage{}
	}
	parsedUsage := Usage{
		InputTokens:              inputTokens,
		OutputTokens:             outputTokens,
		CacheCreationInputTokens: openAICacheCreationTokensFromUsage(usageResult),
		CacheReadInputTokens:     cachedTokens,
		ImageOutputTokens:        int(imageTokens),
	}

	state.usage.InputTokens += parsedUsage.InputTokens
	state.usage.OutputTokens += parsedUsage.OutputTokens
	state.usage.CacheCreationInputTokens += parsedUsage.CacheCreationInputTokens
	state.usage.CacheReadInputTokens += parsedUsage.CacheReadInputTokens
	state.usage.ImageOutputTokens += parsedUsage.ImageOutputTokens
	return parsedUsage
}

func parseUsageIntField(value gjson.Result, required bool) (int, bool) {
	if !value.Exists() {
		return 0, !required
	}
	if value.Type != gjson.Number {
		return 0, false
	}
	return int(value.Int()), true
}

func openAICacheCreationTokensFromUsage(value gjson.Result) int {
	for _, field := range []string{
		"input_tokens_details.cache_write_tokens",
		"prompt_tokens_details.cache_write_tokens",
		"input_tokens_details.cache_creation_tokens",
		"prompt_tokens_details.cache_creation_tokens",
	} {
		result := value.Get(field)
		if result.Exists() {
			return max(int(result.Int()), 0)
		}
	}
	for _, field := range []string{
		"cache_write_tokens",
		"cache_creation_input_tokens",
		"cache_write_input_tokens",
		"cache_creation_tokens",
	} {
		if tokens := int(value.Get(field).Int()); tokens > 0 {
			return tokens
		}
	}
	return 0
}

func enrichResult(result *RelayResult, state *relayState, duration time.Duration) {
	if result == nil {
		return
	}
	result.Duration = duration
	if state == nil {
		return
	}
	result.RequestModel = state.requestModel
	result.Usage = state.usage
	result.RequestID = state.lastResponseID
	result.TerminalEventType = state.terminalEventType
	result.FirstTokenMs = state.firstTokenMs
	result.CyberBlocked = state.cyberBlocked
}

func isOpenAIWSCyberPolicyPayload(payload []byte) bool {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	for _, path := range []string{"error.code", "error.type", "response.error.code", "response.error.type"} {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, path).String()), "cyber_policy") {
			return true
		}
	}
	message := strings.ToLower(strings.TrimSpace(firstNonEmptyRelayString(
		gjson.GetBytes(payload, "error.message").String(),
		gjson.GetBytes(payload, "response.error.message").String(),
	)))
	return strings.Contains(message, "high-risk cyber activity") ||
		strings.Contains(message, "high risk cyber activity") ||
		strings.Contains(message, "cyber_policy")
}

func firstNonEmptyRelayString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func isDisconnectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return true
	}
	switch coderws.CloseStatus(err) {
	case coderws.StatusNormalClosure, coderws.StatusGoingAway, coderws.StatusNoStatusRcvd, coderws.StatusAbnormalClosure:
		return true
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" {
		return false
	}
	return strings.Contains(message, "failed to read frame header: eof") ||
		strings.Contains(message, "unexpected eof") ||
		strings.Contains(message, "use of closed network connection") ||
		strings.Contains(message, "connection reset by peer") ||
		strings.Contains(message, "broken pipe")
}

func isTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}

func shouldParseUsage(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}

func isTokenEvent(eventType string) bool {
	if eventType == "" {
		return false
	}
	switch eventType {
	case "response.created", "response.in_progress", "response.output_item.added", "response.output_item.done":
		return false
	}
	if strings.Contains(eventType, ".delta") {
		return true
	}
	if strings.HasPrefix(eventType, "response.output_text") {
		return true
	}
	if strings.HasPrefix(eventType, "response.output") {
		return true
	}
	return false
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

func waitRelayExit(exitCh <-chan relayExitSignal, timeout time.Duration) (relayExitSignal, bool) {
	if timeout <= 0 {
		timeout = 200 * time.Millisecond
	}
	select {
	case sig := <-exitCh:
		return sig, true
	case <-time.After(timeout):
		return relayExitSignal{}, false
	}
}
