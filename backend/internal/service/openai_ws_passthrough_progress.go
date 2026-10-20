package service

import (
	"context"
	"sync"
	"time"

	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

// A persistent relay can already be reading while the client starts its next
// turn. Arm the deadline after the upstream write, including that pending read.
// Only time actually waiting for upstream frames consumes the progress budget;
// client backpressure and the idle gap between turns do not.
type openAIWSPassthroughProgressConn struct {
	inner openaiwsv2.FrameConn

	mu              sync.Mutex
	timeout         time.Duration
	remaining       time.Duration
	active          bool
	timedOut        bool
	failureRecorded bool
	writePending    bool
	readStarted     time.Time
	readCancel      context.CancelFunc
	timer           *time.Timer
	generation      uint64
}

func newOpenAIWSPassthroughProgressConn(inner openaiwsv2.FrameConn, timeout time.Duration) *openAIWSPassthroughProgressConn {
	return &openAIWSPassthroughProgressConn{inner: inner, timeout: timeout, remaining: timeout, active: true}
}

func (c *openAIWSPassthroughProgressConn) stopTimerLocked() {
	c.generation++
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}

func (c *openAIWSPassthroughProgressConn) armTimerLocked() {
	c.stopTimerLocked()
	if !c.active || c.writePending || c.timeout <= 0 || c.readCancel == nil {
		return
	}
	generation := c.generation
	c.timer = time.AfterFunc(c.remaining, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if generation == c.generation && c.active && c.readCancel != nil {
			c.timedOut = true
			c.readCancel()
		}
	})
}

func (c *openAIWSPassthroughProgressConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.mu.Lock()
	c.readCancel = cancel
	if c.active && !c.writePending {
		c.readStarted = time.Now()
	}
	c.armTimerLocked()
	c.mu.Unlock()

	msgType, payload, err := c.inner.ReadFrame(readCtx)
	c.mu.Lock()
	c.stopTimerLocked()
	if c.active && !c.readStarted.IsZero() {
		c.remaining -= time.Since(c.readStarted)
	}
	c.readCancel = nil
	c.readStarted = time.Time{}
	timedOut := c.timedOut
	if !timedOut && err == nil && (msgType == coderws.MessageText || msgType == coderws.MessageBinary) {
		eventType := gjson.GetBytes(payload, "type").String()
		if isOpenAIWSTerminalEvent(eventType) {
			c.active = false
		} else if classifyOpenAIStreamProgress(payload, eventType).semantic {
			c.remaining = c.timeout
		}
	}
	c.mu.Unlock()
	if timedOut && ctx.Err() == nil {
		return msgType, nil, &openAIWSUpstreamTimeoutError{noProgress: true}
	}
	return msgType, payload, err
}

func (c *openAIWSPassthroughProgressConn) WriteFrame(ctx context.Context, msgType coderws.MessageType, payload []byte) error {
	startsTurn := false
	if msgType == coderws.MessageText || msgType == coderws.MessageBinary {
		switch gjson.GetBytes(payload, "type").String() {
		case "response.create":
			c.mu.Lock()
			c.active = true
			c.writePending = true
			c.timedOut = false
			c.remaining = c.timeout
			c.failureRecorded = false
			c.readStarted = time.Time{}
			c.stopTimerLocked()
			c.mu.Unlock()
			startsTurn = true
		case "response.cancel":
			c.stopProgress()
		}
	}
	err := c.inner.WriteFrame(ctx, msgType, payload)
	if startsTurn {
		c.mu.Lock()
		c.writePending = false
		// A fast response may have already completed during WriteFrame.
		if err == nil && c.active && c.readCancel != nil {
			c.readStarted = time.Now()
			c.armTimerLocked()
		}
		c.mu.Unlock()
	}
	return err
}

func (c *openAIWSPassthroughProgressConn) stopProgress() {
	c.mu.Lock()
	c.active = false
	c.stopTimerLocked()
	c.mu.Unlock()
}

func (c *openAIWSPassthroughProgressConn) turnActive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

func (c *openAIWSPassthroughProgressConn) claimFailure() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failureRecorded {
		return false
	}
	c.failureRecorded = true
	return true
}

func (c *openAIWSPassthroughProgressConn) Close() error {
	c.mu.Lock()
	c.stopTimerLocked()
	c.mu.Unlock()
	return c.inner.Close()
}
