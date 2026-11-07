// Package ssebudget implements opt-in framing limits without ending the
// upstream accounting lifecycle when a display event is too large.
package ssebudget

import (
	"bufio"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

var ErrExceeded = errors.New("upstream stream resource limit exceeded")

type Limits struct{ LineBytes, EventBytes, EventLines int }

func (l Limits) Enabled() bool { return l.LineBytes > 0 || l.EventBytes > 0 || l.EventLines > 0 }

// Reader withholds only the current event. An oversized event is discarded in
// bounded ReadSlice fragments; later valid usage events still reach the existing
// provider parser. The final error is delayed until EOF, never a replay signal.
type Reader struct {
	source   *bufio.Reader
	limits   Limits
	pending  []byte
	terminal error
	overflow atomic.Bool
}

func NewReader(r io.Reader, limits Limits) *Reader {
	return &Reader{source: bufio.NewReaderSize(r, 4096), limits: limits}
}
func (r *Reader) Exceeded() bool { return r != nil && r.overflow.Load() }
func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		if r.terminal != nil {
			err := r.terminal
			r.terminal = io.EOF
			return 0, err
		}
		r.nextEvent()
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
func (r *Reader) nextEvent() {
	var event []byte
	lineBytes, eventBytes, lines := 0, 0, 0
	discard := false
	for {
		fragment, err := r.source.ReadSlice('\n')
		lineBytes += len(fragment)
		eventBytes += len(fragment)
		if (r.limits.LineBytes > 0 && lineBytes > r.limits.LineBytes) || (r.limits.EventBytes > 0 && eventBytes > r.limits.EventBytes) {
			discard = true
			event = nil
			r.overflow.Store(true)
		}
		completeLine := err != bufio.ErrBufferFull
		if completeLine && len(fragment) > 0 {
			lines++
			if r.limits.EventLines > 0 && lines > r.limits.EventLines {
				discard = true
				event = nil
				r.overflow.Store(true)
			}
		}
		if !discard {
			// Cap append growth to the configured event allocation bound.
			if r.limits.EventBytes > 0 && len(event)+len(fragment) > cap(event) {
				capacity := min(r.limits.EventBytes, max(len(event)+len(fragment), 2*cap(event)))
				next := make([]byte, len(event), capacity)
				copy(next, event)
				event = next
			}
			event = append(event, fragment...)
		}
		blank := completeLine && (lineBytes == 1 && len(fragment) == 1 && fragment[0] == '\n' || lineBytes == 2 && len(fragment) == 2 && fragment[0] == '\r' && fragment[1] == '\n')
		if completeLine {
			lineBytes = 0
		}
		if err != nil && err != bufio.ErrBufferFull {
			if err == io.EOF && r.Exceeded() {
				r.terminal = ErrExceeded
			} else {
				r.terminal = err
			}
			r.pending = event
			return
		}
		if blank {
			r.pending = event
			return
		}
	}
}

// Queue counts payload bytes, including an event waiting for its consumer. One
// event larger than the queue limit may own an otherwise empty queue; event/line
// limits bound that event separately. Cancellation only ends this queue wait.
type Queue struct {
	mu          sync.Mutex
	limit, used int64
	changed     chan struct{}
}

func NewQueue(limit int64) *Queue {
	if limit <= 0 {
		return nil
	}
	return &Queue{limit: limit, changed: make(chan struct{})}
}
func (q *Queue) Acquire(n int, done <-chan struct{}) bool {
	if q == nil || n <= 0 {
		return true
	}
	for {
		q.mu.Lock()
		if q.used == 0 || int64(n) <= q.limit-q.used {
			q.used += int64(n)
			q.mu.Unlock()
			return true
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-changed:
		case <-done:
			return false
		}
	}
}
func (q *Queue) Release(n int) {
	if q == nil || n <= 0 {
		return
	}
	q.mu.Lock()
	q.used -= int64(n)
	close(q.changed)
	q.changed = make(chan struct{})
	q.mu.Unlock()
}
