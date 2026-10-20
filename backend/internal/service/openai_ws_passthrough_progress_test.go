package service

import (
	"context"
	"errors"
	"testing"
	"time"

	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

type openAIWSProgressTestConn struct {
	openaiwsv2.FrameConn
	read  func(context.Context) (coderws.MessageType, []byte, error)
	write func(context.Context, coderws.MessageType, []byte) error
}

func (c *openAIWSProgressTestConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	return c.read(ctx)
}

func (c *openAIWSProgressTestConn) WriteFrame(ctx context.Context, typ coderws.MessageType, payload []byte) error {
	return c.write(ctx, typ, payload)
}

func TestOpenAIWSPassthroughProgressExcludesPendingUpstreamWrite(t *testing.T) {
	writeStarted, finishWrite := make(chan struct{}), make(chan struct{})
	readStarted := make(chan struct{})
	inner := &openAIWSProgressTestConn{
		read: func(ctx context.Context) (coderws.MessageType, []byte, error) {
			close(readStarted)
			<-ctx.Done()
			return coderws.MessageText, nil, ctx.Err()
		},
		write: func(ctx context.Context, _ coderws.MessageType, _ []byte) error {
			close(writeStarted)
			select {
			case <-finishWrite:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	conn := newOpenAIWSPassthroughProgressConn(inner, 60*time.Millisecond)
	defer conn.stopProgress()
	conn.stopProgress() // Idle between turns with a read already pending.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	readErr, writeErr := make(chan error, 1), make(chan error, 1)
	go func() { _, _, err := conn.ReadFrame(ctx); readErr <- err }()
	<-readStarted
	go func() { writeErr <- conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create"}`)) }()
	<-writeStarted
	select {
	case err := <-readErr:
		t.Fatalf("upstream write spent the response progress budget: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(finishWrite)
	require.NoError(t, <-writeErr)
	var timeout *openAIWSUpstreamTimeoutError
	require.ErrorAs(t, <-readErr, &timeout)
	require.True(t, conn.turnActive())
}

func TestOpenAIWSPassthroughProgressKeepsTimeoutEvidence(t *testing.T) {
	inner := &openAIWSProgressTestConn{
		read: func(ctx context.Context) (coderws.MessageType, []byte, error) {
			<-ctx.Done()
			// A terminal frame can race with the deadline. If the deadline
			// wins, retain the active turn for the final health callback.
			return coderws.MessageText, []byte(`{"type":"response.completed"}`), nil
		},
	}
	conn := newOpenAIWSPassthroughProgressConn(inner, 20*time.Millisecond)
	defer conn.stopProgress()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := conn.ReadFrame(ctx)
	var timeout *openAIWSUpstreamTimeoutError
	require.ErrorAs(t, err, &timeout)
	require.True(t, conn.turnActive())
}

func TestOpenAIWSPassthroughProgressFastTerminalDuringWrite(t *testing.T) {
	writeStarted, finishWrite := make(chan struct{}), make(chan struct{})
	inner := &openAIWSProgressTestConn{
		read: func(context.Context) (coderws.MessageType, []byte, error) {
			return coderws.MessageText, []byte(`{"type":"response.completed"}`), nil
		},
		write: func(context.Context, coderws.MessageType, []byte) error {
			close(writeStarted)
			<-finishWrite
			return nil
		},
	}
	conn := newOpenAIWSPassthroughProgressConn(inner, 20*time.Millisecond)
	defer conn.stopProgress()
	writeErr := make(chan error, 1)
	go func() {
		writeErr <- conn.WriteFrame(context.Background(), coderws.MessageText, []byte(`{"type":"response.create"}`))
	}()
	<-writeStarted
	_, _, err := conn.ReadFrame(context.Background())
	require.NoError(t, err)
	close(finishWrite)
	require.NoError(t, <-writeErr)
	require.False(t, conn.turnActive())
	conn.mu.Lock()
	require.Nil(t, conn.timer)
	conn.mu.Unlock()
}

func TestOpenAIWSPassthroughProgressWaitsForNextTurn(t *testing.T) {
	inner := &openAIWSCaptureConn{
		events:     [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_first"}}`), []byte(`{"type":"response.in_progress"}`)},
		readDelays: []time.Duration{0, time.Second},
	}
	conn := newOpenAIWSPassthroughProgressConn(inner, 80*time.Millisecond)
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := conn.ReadFrame(ctx)
	require.NoError(t, err)
	require.False(t, conn.turnActive())

	readErr := make(chan error, 1)
	go func() {
		_, _, err := conn.ReadFrame(ctx)
		readErr <- err
	}()
	select {
	case err := <-readErr:
		t.Fatalf("idle socket was timed out between turns: %v", err)
	case <-time.After(160 * time.Millisecond):
	}
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-test"}`)))
	select {
	case err := <-readErr:
		var timeout *openAIWSUpstreamTimeoutError
		require.ErrorAs(t, err, &timeout)
		require.True(t, timeout.noProgress)
	case <-ctx.Done():
		t.Fatal("next turn did not protect the already-pending read")
	}
	// Relay closes the transport before its final turn callback. Preserve the
	// unfinished-turn evidence for that callback, while stopping its timer.
	require.NoError(t, conn.Close())
	require.True(t, conn.turnActive())
}

func TestOpenAIWSPassthroughProgressExcludesClientWaitAndCancellation(t *testing.T) {
	inner := &openAIWSCaptureConn{
		events: [][]byte{
			[]byte(`{"type":"response.in_progress"}`),
			[]byte(`{"type":"response.reasoning_text.delta","delta":"thinking"}`),
			[]byte(`{"type":"response.in_progress"}`),
		},
		readDelays: []time.Duration{0, 0, time.Second},
	}
	conn := newOpenAIWSPassthroughProgressConn(inner, 80*time.Millisecond)
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, err := conn.ReadFrame(ctx)
	require.NoError(t, err)
	// A slow downstream writer must not spend the account's upstream budget.
	select {
	case <-time.After(160 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal("unexpected cancellation")
	}
	_, _, err = conn.ReadFrame(ctx)
	require.NoError(t, err)
	cancel()
	_, _, err = conn.ReadFrame(ctx)
	require.ErrorIs(t, err, context.Canceled)
	var timeout *openAIWSUpstreamTimeoutError
	require.False(t, errors.As(err, &timeout))
}

func TestOpenAIWSPassthroughProgressCancelDisarmsPendingRead(t *testing.T) {
	inner := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.cancelled"}`)}, readDelays: []time.Duration{200 * time.Millisecond}}
	conn := newOpenAIWSPassthroughProgressConn(inner, 80*time.Millisecond)
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	readErr := make(chan error, 1)
	go func() {
		_, _, err := conn.ReadFrame(ctx)
		readErr <- err
	}()
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.cancel"}`)))
	select {
	case err := <-readErr:
		require.NoError(t, err)
		require.False(t, conn.turnActive())
	case <-ctx.Done():
		t.Fatal("cancelled turn did not finish")
	}
}
