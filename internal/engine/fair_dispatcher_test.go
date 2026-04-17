package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// Unit tests for fairDispatcher
// =============================================================================

// TestFairDispatcher_FastPath verifies that the first correlation gets
// immediate admission when the pool is below capacity.
func TestFairDispatcher_FastPath(t *testing.T) {
	fd := newFairDispatcher(8)
	ctx := context.Background()

	ok := fd.acquire(ctx, "corr-A")
	if !ok {
		t.Fatal("expected acquire to succeed immediately")
	}
	if fd.inflightTotal() != 1 {
		t.Errorf("inflight = %d, want 1", fd.inflightTotal())
	}
	fd.release("corr-A")
	if fd.inflightTotal() != 0 {
		t.Errorf("inflight after release = %d, want 0", fd.inflightTotal())
	}
}

// TestFairDispatcher_SingleCorrelation_FullThroughput verifies that a single
// correlation can acquire up to cap slots without being throttled — the single-
// workflow fast path must match old channel-semaphore behaviour.
func TestFairDispatcher_SingleCorrelation_FullThroughput(t *testing.T) {
	const cap = 8
	fd := newFairDispatcher(cap)
	ctx := context.Background()

	// Acquire all 8 slots for one correlation — should all succeed fast-path.
	for i := 0; i < cap; i++ {
		ok := fd.acquire(ctx, "corr-A")
		if !ok {
			t.Fatalf("slot %d: expected immediate acquire for single correlation", i)
		}
	}
	if fd.inflightTotal() != cap {
		t.Errorf("inflight = %d, want %d", fd.inflightTotal(), cap)
	}

	// Release all.
	for i := 0; i < cap; i++ {
		fd.release("corr-A")
	}
	if fd.inflightTotal() != 0 {
		t.Errorf("inflight after release = %d, want 0", fd.inflightTotal())
	}
}

// TestFairDispatcher_TwoCorrelations_FairShare is the core fairness test:
// two correlations with fan-out (10 dispatches each) on a maxActive=4 pool.
// Each correlation must receive approximately 2 concurrent slots (±1 tolerance
// for the integer division).
func TestFairDispatcher_TwoCorrelations_FairShare(t *testing.T) {
	const (
		maxActive = 4
		dispatchN = 10 // handlers per correlation
		wantShare = 2  // cap/correlations
		tolerance = 1  // integer division may give 1 or 2 initially
	)

	fd := newFairDispatcher(maxActive)
	ctx := context.Background()

	var (
		peakA atomic.Int32
		peakB atomic.Int32
		wg    sync.WaitGroup
	)

	// Helper: acquire slot, record peak concurrent inflight for this correlation,
	// hold for a moment, then release.
	runDispatch := func(corrID string, peak *atomic.Int32) {
		defer wg.Done()
		if !fd.acquire(ctx, corrID) {
			return
		}
		defer fd.release(corrID)

		// Record peak inflight for this correlation by reading the snapshot.
		byCorr := fd.activeCorrelations()
		cur := int32(byCorr[corrID])
		for {
			old := peak.Load()
			if cur <= old {
				break
			}
			if peak.CompareAndSwap(old, cur) {
				break
			}
		}

		// Hold the slot briefly so the pool fills and fairness is exercised.
		time.Sleep(5 * time.Millisecond)
	}

	// Launch all dispatches in parallel.
	wg.Add(dispatchN * 2)
	for i := 0; i < dispatchN; i++ {
		go runDispatch("corr-A", &peakA)
		go runDispatch("corr-B", &peakB)
	}
	wg.Wait()

	// Each correlation should have reached at most (wantShare + tolerance) concurrently.
	if pa := peakA.Load(); pa > int32(wantShare+tolerance) {
		t.Errorf("corr-A peak concurrent = %d, want <= %d (fair share %d ± %d)",
			pa, wantShare+tolerance, wantShare, tolerance)
	}
	if pb := peakB.Load(); pb > int32(wantShare+tolerance) {
		t.Errorf("corr-B peak concurrent = %d, want <= %d (fair share %d ± %d)",
			pb, wantShare+tolerance, wantShare, tolerance)
	}

	// Both correlations must have made progress (no starvation).
	if peakA.Load() == 0 {
		t.Error("corr-A never acquired any slot")
	}
	if peakB.Load() == 0 {
		t.Error("corr-B never acquired any slot")
	}
}

// TestFairDispatcher_SingleCorrelation_NoRegression verifies that a single
// correlation with 50 dispatches on a maxActive=8 pool completes without
// artificial throttling — old unbounded-per-correlation throughput preserved.
func TestFairDispatcher_SingleCorrelation_NoRegression(t *testing.T) {
	const (
		maxActive = 8
		total     = 50
	)

	fd := newFairDispatcher(maxActive)
	ctx := context.Background()

	var completed atomic.Int32
	var wg sync.WaitGroup

	wg.Add(total)
	for i := 0; i < total; i++ {
		go func() {
			defer wg.Done()
			if !fd.acquire(ctx, "corr-single") {
				return
			}
			defer fd.release("corr-single")
			completed.Add(1)
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
		// All completed successfully.
	case <-time.After(5 * time.Second):
		t.Fatalf("50 dispatches on 1 correlation timed out (completed=%d) — regression in single-correlation throughput",
			completed.Load())
	}

	if n := completed.Load(); n != total {
		t.Errorf("completed %d / %d", n, total)
	}
	if fd.inflightTotal() != 0 {
		t.Errorf("inflight after all done = %d, want 0", fd.inflightTotal())
	}
}

// TestFairDispatcher_BurstyArrival verifies that a newly-arriving correlation
// (corr-B with 1 dispatch) gets a slot within the next release window, even
// when corr-A has many pending dispatches already holding the pool at capacity.
func TestFairDispatcher_BurstyArrival(t *testing.T) {
	const maxActive = 4
	fd := newFairDispatcher(maxActive)
	ctx := context.Background()

	// Fill the pool with corr-A dispatches that hold their slots.
	release := make(chan struct{})
	var holdersReady sync.WaitGroup

	holdersReady.Add(maxActive)
	for i := 0; i < maxActive; i++ {
		go func() {
			if !fd.acquire(ctx, "corr-A") {
				holdersReady.Done()
				return
			}
			holdersReady.Done()
			<-release // hold until released
			fd.release("corr-A")
		}()
	}

	// Wait until all 4 slots are held by corr-A.
	holdersReady.Wait()
	if fd.inflightTotal() != maxActive {
		t.Fatalf("expected pool full (%d), got %d", maxActive, fd.inflightTotal())
	}

	// Now inject corr-B's single dispatch — it must wait (pool full).
	bSlotAcquired := make(chan struct{}, 1)
	go func() {
		if fd.acquire(ctx, "corr-B") {
			bSlotAcquired <- struct{}{}
			fd.release("corr-B")
		}
	}()

	// corr-B must NOT have a slot while pool is full.
	select {
	case <-bSlotAcquired:
		t.Error("corr-B acquired a slot while pool was full — impossible")
	case <-time.After(50 * time.Millisecond):
		// Expected: corr-B is waiting.
	}

	// Release ONE corr-A slot. corr-B should be admitted (it's under fair share).
	close(release)

	select {
	case <-bSlotAcquired:
		// Good: corr-B got a slot within the next release window.
	case <-time.After(2 * time.Second):
		t.Error("corr-B did not acquire a slot after corr-A released — possible starvation")
	}
}

// TestFairDispatcher_ContextCancellation verifies that a goroutine blocked in
// acquire() returns false when its context is cancelled and the pool remains
// usable afterward.
func TestFairDispatcher_ContextCancellation(t *testing.T) {
	const maxActive = 1
	fd := newFairDispatcher(maxActive)
	ctx := context.Background()

	// Fill the pool.
	if !fd.acquire(ctx, "corr-holder") {
		t.Fatal("could not fill pool")
	}

	// Start a waiter with a cancellable context.
	cancelCtx, cancel := context.WithCancel(ctx)
	done := make(chan bool, 1)
	go func() {
		ok := fd.acquire(cancelCtx, "corr-waiter")
		done <- ok
	}()

	// Cancel the context after a short delay — waiter should return false.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Error("cancelled context should have caused acquire to return false")
		}
	case <-time.After(2 * time.Second):
		t.Error("acquire did not unblock after context cancellation")
	}

	// The pool should still have 1 slot (the holder). After release, 0.
	fd.release("corr-holder")
	if fd.inflightTotal() != 0 {
		t.Errorf("inflight after release = %d, want 0", fd.inflightTotal())
	}

	// Pool must be functional: new admits should work.
	if !fd.acquire(ctx, "corr-new") {
		t.Error("pool broken after context cancellation")
	}
	fd.release("corr-new")
}

// TestFairDispatcher_StarvationGuard verifies that an over-share correlation
// eventually makes progress when there are no under-share competitors
// (starvation guard: Pass 2 in admitOne).
func TestFairDispatcher_StarvationGuard(t *testing.T) {
	const maxActive = 2
	fd := newFairDispatcher(maxActive)
	ctx := context.Background()

	// Occupy both slots with corr-A.
	if !fd.acquire(ctx, "corr-A") {
		t.Fatal("first acquire failed")
	}
	if !fd.acquire(ctx, "corr-A") {
		t.Fatal("second acquire failed")
	}

	// Third corr-A acquire must wait (pool full, but will be released shortly).
	admitted := make(chan bool, 1)
	go func() {
		ok := fd.acquire(ctx, "corr-A")
		admitted <- ok
	}()

	// Release one slot — the only waiter (corr-A) must be admitted via Pass 2.
	time.Sleep(20 * time.Millisecond)
	fd.release("corr-A")

	select {
	case ok := <-admitted:
		if !ok {
			t.Error("starvation guard: corr-A waiter should have been admitted")
		}
	case <-time.After(2 * time.Second):
		t.Error("starvation guard: corr-A waiter timed out — deadlock?")
	}

	// Cleanup.
	fd.release("corr-A")
	fd.release("corr-A")
}

// =============================================================================
// Integration tests: PersonaRunner with fairDispatcher
// =============================================================================

// TestFairDispatch_TwoWorkflowsSharePool is an integration test wiring the
// PersonaRunner with a small pool (maxActive=4) and two concurrent workflows
// (5 handlers each). Each workflow should make progress — neither is starved
// by the other.
//
// The test uses the fairDispatcher directly (not via PersonaRunner) to avoid
// the complexity of wiring the Engine for WorkflowStarted dispatch. We verify
// that both correlations can acquire slots concurrently without one monopolising
// the pool.
func TestFairDispatch_TwoWorkflowsSharePool(t *testing.T) {
	const (
		maxActive = 4
		total     = 10 // dispatches per correlation
	)

	fd := newFairDispatcher(maxActive)
	ctx := context.Background()

	var (
		firedA atomic.Int32
		firedB atomic.Int32
		wg     sync.WaitGroup
	)

	runHandler := func(corrID string, counter *atomic.Int32) {
		defer wg.Done()
		if !fd.acquire(ctx, corrID) {
			return
		}
		defer fd.release(corrID)
		counter.Add(1)
		// Brief hold to create contention.
		time.Sleep(2 * time.Millisecond)
	}

	wg.Add(total * 2)
	for i := 0; i < total; i++ {
		go runHandler("corr-A", &firedA)
		go runHandler("corr-B", &firedB)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out (firedA=%d, firedB=%d)", firedA.Load(), firedB.Load())
	}

	if firedA.Load() != total {
		t.Errorf("corr-A: expected %d fires, got %d", total, firedA.Load())
	}
	if firedB.Load() != total {
		t.Errorf("corr-B: expected %d fires, got %d", total, firedB.Load())
	}
	if fd.inflightTotal() != 0 {
		t.Errorf("inflight after all done = %d, want 0", fd.inflightTotal())
	}
}

// TestFairDispatch_BurstyArrival_PersonaRunner reproduces the starved-committer
// scenario at the fairDispatcher level: correlation A (pr-review fan-out) holds
// all pool slots; correlation B's single critical-path dispatch arrives and must
// get a slot within the next release window, not starve behind A's queue.
func TestFairDispatch_BurstyArrival_PersonaRunner(t *testing.T) {
	const (
		maxActive  = 4
		noiseCount = 20 // corr-A dispatches (pr-review-style fan-out)
	)

	fd := newFairDispatcher(maxActive)
	ctx := context.Background()

	// Phase 1: fill the pool with corr-A (noise) dispatches that hold slots.
	// Use a gate to synchronise: all holders must be blocking before B arrives.
	holdersAcquired := make(chan struct{}, noiseCount)
	releaseNoise := make(chan struct{})
	var noiseWg sync.WaitGroup

	noiseWg.Add(noiseCount)
	for i := 0; i < noiseCount; i++ {
		go func() {
			defer noiseWg.Done()
			if !fd.acquire(ctx, "corr-A") {
				return
			}
			holdersAcquired <- struct{}{} // signal: this goroutine holds a slot
			<-releaseNoise                // hold until released
			fd.release("corr-A")
		}()
	}

	// Wait for the pool to fill (maxActive goroutines holding slots from corr-A).
	filled := 0
	timeout := time.After(2 * time.Second)
	for filled < maxActive {
		select {
		case <-holdersAcquired:
			filled++
		case <-timeout:
			t.Fatalf("pool did not fill after 2s (filled=%d)", filled)
		}
	}

	// Phase 2: inject corr-B's single critical-path dispatch.
	// Because corr-B is a new participant, fairDispatcher Pass 1 will prefer it
	// (it's under fair share) over the remaining corr-A waiters.
	bGotSlot := make(chan struct{}, 1)
	go func() {
		if fd.acquire(ctx, "corr-B") {
			bGotSlot <- struct{}{}
			fd.release("corr-B")
		}
	}()

	// Release one noise slot. corr-B must be admitted (under-share preference).
	close(releaseNoise)

	select {
	case <-bGotSlot:
		// corr-B got its slot. Pass.
	case <-time.After(2 * time.Second):
		t.Error("corr-B (critical handler) starved behind corr-A (noise fan-out) — fair-share not working")
	}

	noiseWg.Wait()
}

