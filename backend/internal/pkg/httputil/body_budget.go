package httputil

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var ErrBodyBudgetExceeded = errors.New("request body memory budget exhausted")

// BodyBudget bounds registered body buffers, not total process heap. A zero
// budget creates no scope and preserves the original reader path.
type BodyBudget struct {
	limit int64
	used  atomic.Int64
}

func NewBodyBudget(limit int64) *BodyBudget { return &BodyBudget{limit: limit} }
func (b *BodyBudget) Used() int64 {
	if b == nil {
		return 0
	}
	return b.used.Load()
}
func (b *BodyBudget) Scope() *BodyReservation {
	if b == nil || b.limit <= 0 {
		return nil
	}
	return &BodyReservation{budget: b, refs: 1}
}

type bodyReservationKey struct{}
type BodyReservation struct {
	mu          sync.Mutex
	budget      *BodyBudget
	bytes       int64
	refs        int
	releaseOnce sync.Once
}

func WithBodyReservation(ctx context.Context, r *BodyReservation) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, bodyReservationKey{}, r)
}
func BodyReservationFromContext(ctx context.Context) *BodyReservation {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(bodyReservationKey{}).(*BodyReservation)
	return r
}
func (r *BodyReservation) Reserve(n int64) error {
	if r == nil || n <= 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refs <= 0 {
		return ErrBodyBudgetExceeded
	}
	for {
		used := r.budget.used.Load()
		if n > r.budget.limit-used {
			return ErrBodyBudgetExceeded
		}
		if r.budget.used.CompareAndSwap(used, used+n) {
			r.bytes += n
			return nil
		}
	}
}

// Detach explicitly retains ownership after an HTTP handler has returned. The
// returned release function is idempotent, including concurrent completion.
func (r *BodyReservation) Detach() func() {
	if r == nil {
		return func() {}
	}
	r.mu.Lock()
	if r.refs <= 0 {
		r.mu.Unlock()
		return func() {}
	}
	r.refs++
	r.mu.Unlock()
	var once sync.Once
	return func() { once.Do(r.dropRef) }
}
func (r *BodyReservation) Release() {
	if r != nil {
		r.releaseOnce.Do(r.dropRef)
	}
}
func (r *BodyReservation) dropRef() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refs <= 0 {
		return
	}
	r.refs--
	if r.refs == 0 {
		r.budget.used.Add(-r.bytes)
		r.bytes = 0
	}
}
