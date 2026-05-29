package backend

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"
)

// ErrIdleTimeout is the cancellation cause set on contexts derived from
// WithIdleTimeout when the subprocess produces no output within the idle
// window. Use context.Cause(ctx) to distinguish this from a wall-clock
// DeadlineExceeded or a user-initiated cancel.
var ErrIdleTimeout = errors.New("backend: idle timeout exceeded")

// ErrProgressTimeout is the cancellation cause set on contexts derived from
// WithProgressTimeout when the subprocess keeps emitting bytes but produces no
// *substantive completion progress* (assistant answer text) within the window.
// It is the catch for the chatty-but-wedged failure mode: a claude CLI stuck
// in a tool-use loop streams system.task_* / tool_use protocol frames forever,
// which keeps resetting the byte-level ErrIdleTimeout watchdog yet never
// converges to a result (incident 1a332d59 — architect wedged ~27min emitting
// tool chatter with zero text deltas). Distinguish it from ErrIdleTimeout via
// context.Cause(ctx).
var ErrProgressTimeout = errors.New("backend: completion-progress timeout exceeded")

// WithIdleTimeout returns a context that cancels with cause ErrIdleTimeout if
// Progress is not called within the idle duration. Each Progress call resets
// the timer. Call stop when the subprocess completes to release the watcher
// goroutine.
//
// When idle <= 0, the watchdog is disabled: the returned context is ctx
// unchanged, Progress is a no-op, and stop is a no-op.
//
// The watcher checks at idle/4 granularity (minimum 1s) so very long idle
// windows don't incur a fast polling loop. Timing precision is not required
// here — we're detecting wedged subprocesses that sit silent for minutes, not
// sub-second stalls.
func WithIdleTimeout(ctx context.Context, idle time.Duration) (context.Context, func(), context.CancelFunc) {
	return withWatchdog(ctx, idle, ErrIdleTimeout)
}

// WithProgressTimeout is WithIdleTimeout's higher-altitude sibling: it cancels
// with cause ErrProgressTimeout when Progress is not called within the window.
// The caller wires Progress to fire only on *substantive* progress (assistant
// text deltas), not on raw bytes, so it catches a subprocess that is alive and
// chattering but never converging. It is meant to stack on top of the short
// byte-level WithIdleTimeout as a longer, looser net — generous enough to clear
// a legitimate tool-only exploration gap, but well below the wall-clock budget.
// Disabled when window <= 0 (the default), so it never changes behavior unless
// an operator opts in via RICK_BACKEND_PROGRESS_TIMEOUT.
func WithProgressTimeout(ctx context.Context, window time.Duration) (context.Context, func(), context.CancelFunc) {
	return withWatchdog(ctx, window, ErrProgressTimeout)
}

// withWatchdog is the shared timer core behind WithIdleTimeout and
// WithProgressTimeout. cause is the value handed to context.CancelCause when
// the window elapses without a Progress call, letting callers tell the two
// watchdogs apart via context.Cause.
func withWatchdog(ctx context.Context, idle time.Duration, cause error) (context.Context, func(), context.CancelFunc) {
	if idle <= 0 {
		return ctx, func() {}, func() {}
	}

	derived, cancel := context.WithCancelCause(ctx)
	var lastNanos atomic.Int64
	lastNanos.Store(time.Now().UnixNano())

	// Tick at idle/4 with a 10ms floor. The floor prevents pathological spin
	// loops on sub-millisecond idle windows; the production callers use
	// minute-scale values where idle/4 dominates.
	tickEvery := max(idle/4, 10*time.Millisecond)

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(tickEvery)
		defer ticker.Stop()
		for {
			select {
			case <-derived.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				last := time.Unix(0, lastNanos.Load())
				if time.Since(last) >= idle {
					cancel(cause)
					return
				}
			}
		}
	}()

	progress := func() {
		lastNanos.Store(time.Now().UnixNano())
	}

	stop := context.CancelFunc(func() {
		// Tell the watcher goroutine to exit without cancelling the ctx with
		// a cause — if the caller is stopping normally, they've already moved
		// past the subprocess and don't need the cancellation cause set.
		select {
		case <-done:
			// Already closed; nothing to do.
		default:
			close(done)
		}
		cancel(nil)
	})

	return derived, progress, stop
}

// progressWriter wraps an io.Writer and fires progress() on every successful
// Write call. Used to tap the subprocess stdout stream so any output — a
// newline, a token, a full event — counts as "making progress" for the idle
// watchdog.
type progressWriter struct {
	dst      io.Writer
	progress func()
}

func newProgressWriter(dst io.Writer, progress func()) io.Writer {
	if progress == nil {
		return dst
	}
	return &progressWriter{dst: dst, progress: progress}
}

func (p *progressWriter) Write(b []byte) (int, error) {
	if len(b) > 0 {
		p.progress()
	}
	return p.dst.Write(b)
}
