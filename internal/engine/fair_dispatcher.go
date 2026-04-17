package engine

import (
	"context"
	"sync"
)

// fairDispatcher replaces the raw channel semaphore with a weighted-fair-queue
// admission controller. It enforces two invariants:
//
//  1. Total concurrent slots never exceeds cap.
//  2. Each active correlationID receives a soft fair share of cap/activeCorrelations
//     slots. A correlation already at or above its fair share must wait until either
//     more total slots open OR the fair shares rebalance (because another correlation
//     finishes all its work).
//
// Design: mutex + condition variable. The mutex is held only for brief accounting
// operations. cond.Wait() releases the mutex while sleeping, so releaseSlot can
// always proceed without waiting for any sleeping goroutine.
//
// Admission policy (admitOne):
//
//	Pass 1: find the oldest waiter whose correlation is strictly under its new
//	        fair share — admit it.
//	Pass 2 (starvation guard): if no under-share waiter exists (e.g. all waiters
//	        belong to one over-share correlation), admit the oldest waiter regardless.
//
// Ambiguity: when correlation A has 20 pending dispatches and B has 1, B is
// admitted first because B is under-share. A waits until B's slot is returned or
// the fair shares rebalance. This is intentional (Fix 4 requirement: "B must get
// a slot within the next completion window"). A cannot starve because Pass 2
// guarantees progress when all waiters are over-share.
//
// Single-correlation fast path: with one active correlation, fairShare == cap, so
// atOrUnderFairShare always returns true and every admission goes through the fast
// path (no cond.Wait), matching the old channel-semaphore latency.
//
// Deadlock freedom: the mutex is released (via cond.Wait) while waiting for a slot.
// The handler call happens entirely outside the mutex. release re-acquires the mutex
// only briefly to decrement counters, then broadcasts. No caller holds this mutex
// across a handler call. Context cancellation is handled by a goroutine that calls
// cond.Broadcast() when ctx is done, which wakes the blocked goroutine so it can
// exit the Wait loop and detect the cancellation.
type fairDispatcher struct {
	mu       sync.Mutex
	cond     *sync.Cond
	cap      int            // total slot cap, immutable after construction
	total    int            // slots currently in use
	inflight map[string]int // correlationID → slots currently held
	waiters  []*fairWaiter  // FIFO list of pending goroutines
}

// fairWaiter tracks one goroutine blocked in acquire(). Pointer-stable: never
// copied after creation, so pointer comparison is safe for identity checks.
type fairWaiter struct {
	corrID   string
	admitted bool // set true by the goroutine that selects this waiter via admit()
}

func newFairDispatcher(cap int) *fairDispatcher {
	fd := &fairDispatcher{
		cap:      cap,
		inflight: make(map[string]int),
	}
	fd.cond = sync.NewCond(&fd.mu)
	return fd
}

// acquire blocks until a fair-share slot is available for corrID, or ctx is
// cancelled. Returns false only on context cancellation (caller should abort).
//
// Invariant: the caller must call release(corrID) exactly once after a true
// return, and must NOT call release after a false return.
func (fd *fairDispatcher) acquire(ctx context.Context, corrID string) bool {
	fd.mu.Lock()

	// Fast path: immediately admit if total < cap AND this correlation is at or
	// below its current fair share. This is the hot path for uncontested slots.
	if fd.total < fd.cap && fd.atOrUnderFairShare(corrID) {
		fd.total++
		fd.inflight[corrID]++
		fd.mu.Unlock()
		return true
	}

	// Slow path: register as a waiter and block on the condition variable.
	// We spawn a goroutine to broadcast when ctx is done so that cond.Wait()
	// is guaranteed to unblock even if no release() call happens.
	w := &fairWaiter{corrID: corrID}
	fd.waiters = append(fd.waiters, w)

	// ctxDone wakes the Wait loop when the context is cancelled.
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			fd.cond.Broadcast()
		case <-stop:
		}
	}()

	for {
		// w.admitted is set by the goroutine executing admit(w) inside admitOne.
		// That goroutine has already incremented total and inflight for us, so we
		// can return immediately without touching shared state.
		if w.admitted {
			close(stop)
			fd.mu.Unlock()
			return true
		}

		// Check context cancellation. The Broadcast from the goroutine above
		// will have woken us from Wait() if ctx was cancelled.
		if ctx.Err() != nil {
			fd.removeWaiter(w)
			close(stop)
			fd.mu.Unlock()
			return false
		}

		// Release the lock and sleep until someone calls cond.Broadcast.
		fd.cond.Wait()
	}
}

// release frees the slot held by corrID and wakes up the best candidate waiter.
func (fd *fairDispatcher) release(corrID string) {
	fd.mu.Lock()
	fd.total--
	fd.inflight[corrID]--
	if fd.inflight[corrID] == 0 {
		delete(fd.inflight, corrID)
	}
	fd.admitOne()
	fd.mu.Unlock()
}

// admitOne selects and admits the best pending waiter. Must be called with mu held.
//
// Two-pass algorithm:
//
//	Pass 1: first waiter whose correlation is strictly under its new fair share.
//	Pass 2: oldest non-admitted waiter unconditionally (starvation guard).
func (fd *fairDispatcher) admitOne() {
	if fd.total >= fd.cap || len(fd.waiters) == 0 {
		return
	}

	// Pass 1: prefer an under-fair-share correlation.
	for _, w := range fd.waiters {
		if w.admitted {
			continue
		}
		if fd.atOrUnderFairShare(w.corrID) {
			fd.admit(w)
			return
		}
	}

	// Pass 2: starvation guard — admit the oldest non-admitted waiter.
	for _, w := range fd.waiters {
		if !w.admitted {
			fd.admit(w)
			return
		}
	}
}

// admit marks w as admitted, increments counters, removes w from the waiters
// slice, and broadcasts to wake all sleeping goroutines. Must be called with
// mu held. The selected goroutine will see w.admitted == true and return.
func (fd *fairDispatcher) admit(w *fairWaiter) {
	fd.total++
	fd.inflight[w.corrID]++
	w.admitted = true
	fd.removeWaiter(w)
	// Broadcast wakes all waiters. The selected goroutine exits; others
	// re-check their own w.admitted (false) and go back to sleep.
	fd.cond.Broadcast()
}

// removeWaiter removes w from the waiters slice by pointer identity.
// Must be called with mu held.
func (fd *fairDispatcher) removeWaiter(w *fairWaiter) {
	for i, existing := range fd.waiters {
		if existing == w {
			fd.waiters = append(fd.waiters[:i], fd.waiters[i+1:]...)
			return
		}
	}
}

// atOrUnderFairShare returns true if corrID's current inflight count is strictly
// less than its fair share of the cap. Must be called with mu held.
//
// Fair share = cap / max(1, activeCorrelations). A correlation not yet in inflight
// is counted as +1 active so new arrivals get a proportional budget. Integer
// arithmetic truncates, so with cap=4 and 3 correlations each gets 1 slot.
// "Strictly less than" means a correlation holding exactly fairShare is treated
// as at-share — it won't preempt a truly under-share correlation in Pass 1.
//
// Minimum fair share is 1: every correlation can always make progress, even when
// cap < activeCorrelations (e.g., cap=2, 5 correlations → fairShare=1 each).
func (fd *fairDispatcher) atOrUnderFairShare(corrID string) bool {
	active := len(fd.inflight)
	if _, exists := fd.inflight[corrID]; !exists {
		// corrID not yet holding any slots — count it as a new participant.
		active++
	}
	if active < 1 {
		active = 1
	}
	fairShare := fd.cap / active
	if fairShare < 1 {
		fairShare = 1
	}
	return fd.inflight[corrID] < fairShare
}

// activeCorrelations returns a snapshot of correlationID → inflight counts.
// Used by PersonaRunner.Snapshot() for observability.
func (fd *fairDispatcher) activeCorrelations() map[string]int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	out := make(map[string]int, len(fd.inflight))
	for k, v := range fd.inflight {
		out[k] = v
	}
	return out
}

// inflightTotal returns the current total slots in use.
func (fd *fairDispatcher) inflightTotal() int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.total
}
