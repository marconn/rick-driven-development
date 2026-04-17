package backend

import (
	"context"
	"time"
)

// Recorder observes concurrency limiter events. Nil is valid — limiters with
// no recorder simply skip the observation calls. Intended as the seam for
// internal/observe to see per-backend inflight + wait-time saturation without
// the backend package taking a hard dep on it.
//
// Acquired fires after a slot has been successfully taken; waited is the time
// spent blocked on the semaphore (zero when the fast path hit). Released fires
// after the underlying Run returns (success or error).
type Recorder interface {
	Acquired(name string, waited time.Duration)
	Released(name string)
}

// Limiter is a backend-name-scoped counting semaphore. A nil *Limiter is a
// no-op (Acquire returns immediately, Release is ignored) so the unlimited
// default path pays no allocation or synchronization cost.
type Limiter struct {
	slots    chan struct{}
	recorder Recorder
}

// NewLimiter constructs a Limiter with the given slot count. size <= 0 returns
// nil, which callers should treat as "unlimited" via the nil-receiver methods.
func NewLimiter(size int, recorder Recorder) *Limiter {
	if size <= 0 {
		return nil
	}
	return &Limiter{
		slots:    make(chan struct{}, size),
		recorder: recorder,
	}
}

// Acquire blocks until a slot is available or ctx is cancelled. On ctx
// cancellation, returns ctx.Err() without holding a slot. A nil Limiter
// always returns nil without blocking.
func (l *Limiter) Acquire(ctx context.Context, name string) error {
	if l == nil {
		return nil
	}
	// Fast path: no blocking, no wall-clock read.
	select {
	case l.slots <- struct{}{}:
		if l.recorder != nil {
			l.recorder.Acquired(name, 0)
		}
		return nil
	default:
	}
	start := time.Now()
	select {
	case l.slots <- struct{}{}:
		if l.recorder != nil {
			l.recorder.Acquired(name, time.Since(start))
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release returns a slot to the semaphore. Must be paired with a successful
// Acquire. A nil Limiter is a no-op.
func (l *Limiter) Release(name string) {
	if l == nil {
		return
	}
	<-l.slots
	if l.recorder != nil {
		l.recorder.Released(name)
	}
}
