package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// === Unit Tests for workflowThrottle ===

func TestThrottleShouldQueueDisabled(t *testing.T) {
	th := newWorkflowThrottle(0, nil, slog.Default())
	if th.shouldQueue() {
		t.Error("disabled throttle should never queue")
	}
}

func TestThrottleShouldQueueUnderCapacity(t *testing.T) {
	th := newWorkflowThrottle(3, nil, slog.Default())
	th.addRunning("wf-1")
	if th.shouldQueue() {
		t.Error("should not queue when under capacity (1/3)")
	}
}

func TestThrottleShouldQueueAtCapacity(t *testing.T) {
	th := newWorkflowThrottle(2, nil, slog.Default())
	th.addRunning("wf-1")
	th.addRunning("wf-2")
	if !th.shouldQueue() {
		t.Error("should queue when at capacity (2/2)")
	}
}

func TestThrottleEnqueueDequeue(t *testing.T) {
	ctx := context.Background()
	th := newWorkflowThrottle(1, nil, slog.Default())
	env1 := event.Envelope{AggregateID: "wf-1"}
	env2 := event.Envelope{AggregateID: "wf-2"}

	th.enqueue(ctx, env1)
	th.enqueue(ctx, env2)

	if th.queuedCount() != 2 {
		t.Fatalf("expected 2 queued, got %d", th.queuedCount())
	}

	// FIFO order
	got, ok := th.dequeue(ctx)
	if !ok || got.AggregateID != "wf-1" {
		t.Errorf("first dequeue: want wf-1, got %s (ok=%v)", got.AggregateID, ok)
	}
	got, ok = th.dequeue(ctx)
	if !ok || got.AggregateID != "wf-2" {
		t.Errorf("second dequeue: want wf-2, got %s (ok=%v)", got.AggregateID, ok)
	}
	_, ok = th.dequeue(ctx)
	if ok {
		t.Error("dequeue from empty should return false")
	}
}

func TestThrottleRemoveRunning(t *testing.T) {
	th := newWorkflowThrottle(2, nil, slog.Default())
	th.addRunning("wf-1")
	th.addRunning("wf-2")

	if !th.removeRunning("wf-1") {
		t.Error("removeRunning should return true for present ID")
	}
	if th.removeRunning("wf-1") {
		t.Error("removeRunning should return false for absent ID")
	}
	if th.runningCount() != 1 {
		t.Errorf("expected 1 running, got %d", th.runningCount())
	}
}

func TestThrottleRemoveQueued(t *testing.T) {
	ctx := context.Background()
	th := newWorkflowThrottle(1, nil, slog.Default())
	th.enqueue(ctx, event.Envelope{AggregateID: "wf-1"})
	th.enqueue(ctx, event.Envelope{AggregateID: "wf-2"})
	th.enqueue(ctx, event.Envelope{AggregateID: "wf-3"})

	if !th.removeQueued(ctx, "wf-2") {
		t.Error("removeQueued should return true for present ID")
	}
	if th.queuedCount() != 2 {
		t.Errorf("expected 2 queued, got %d", th.queuedCount())
	}

	// Verify ordering preserved after middle removal
	got, _ := th.dequeue(ctx)
	if got.AggregateID != "wf-1" {
		t.Errorf("expected wf-1 first, got %s", got.AggregateID)
	}
	got, _ = th.dequeue(ctx)
	if got.AggregateID != "wf-3" {
		t.Errorf("expected wf-3 second, got %s", got.AggregateID)
	}
}

func TestThrottleRemoveQueuedNotFound(t *testing.T) {
	ctx := context.Background()
	th := newWorkflowThrottle(1, nil, slog.Default())
	if th.removeQueued(ctx, "nope") {
		t.Error("removeQueued should return false for absent ID")
	}
}

func TestThrottleWarmRunning(t *testing.T) {
	th := newWorkflowThrottle(5, nil, slog.Default())
	th.warmRunning([]string{"wf-1", "wf-2", "wf-3"})

	if th.runningCount() != 3 {
		t.Errorf("expected 3 running after warm, got %d", th.runningCount())
	}
}

func TestThrottleEnabled(t *testing.T) {
	if newWorkflowThrottle(0, nil, slog.Default()).enabled() {
		t.Error("max=0 should not be enabled")
	}
	if !newWorkflowThrottle(1, nil, slog.Default()).enabled() {
		t.Error("max=1 should be enabled")
	}
}

// === Integration Tests: Engine + Throttle ===

func newThrottledTestEngine(t *testing.T, maxConcurrent int) (*Engine, eventstore.Store, eventbus.Bus) {
	t.Helper()
	store, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	bus := eventbus.NewChannelBus()
	logger := slog.Default()
	eng := NewEngine(store, bus, logger)
	eng.SetMaxConcurrentWorkflows(maxConcurrent)
	t.Cleanup(func() {
		eng.Stop()
		_ = bus.Close()
		_ = store.Close()
	})
	return eng, store, bus
}

func seedWorkflowRequested(t *testing.T, store eventstore.Store, aggID, workflowID string) event.Envelope {
	t.Helper()
	ctx := context.Background()
	reqEvt := event.New(event.WorkflowRequested, 1,
		event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "build", WorkflowID: workflowID})).
		WithAggregate(aggID, 1).WithCorrelation(aggID)
	if err := store.Append(ctx, aggID, 0, []event.Envelope{reqEvt}); err != nil {
		t.Fatalf("seed %s: %v", aggID, err)
	}
	return reqEvt
}

func TestEngineThrottleQueuesAtCapacity(t *testing.T) {
	eng, store, bus := newThrottledTestEngine(t, 1)
	ctx := context.Background()

	eng.RegisterWorkflow(WorkflowDef{ID: "test-wf", Required: []string{"developer"}, MaxIterations: 3})

	// Subscribe to published events
	published := make(chan event.Envelope, 20)
	bus.SubscribeAll(func(_ context.Context, env event.Envelope) error {
		published <- env
		return nil
	})

	// Seed and process first workflow — should start
	req1 := seedWorkflowRequested(t, store, "wf-1", "test-wf")
	if err := eng.processDecision(ctx, req1); err != nil {
		t.Fatalf("process wf-1: %v", err)
	}

	// Verify WorkflowStarted published for wf-1
	time.Sleep(50 * time.Millisecond)
	var startedCount int
	drain(published, func(env event.Envelope) {
		if event.IsWorkflowStarted(env.Type) {
			startedCount++
		}
	})
	if startedCount != 1 {
		t.Fatalf("expected 1 WorkflowStarted, got %d", startedCount)
	}

	// Seed and process second workflow — should be queued
	req2 := seedWorkflowRequested(t, store, "wf-2", "test-wf")
	if err := eng.processDecision(ctx, req2); err != nil {
		t.Fatalf("process wf-2: %v", err)
	}

	// No WorkflowStarted should be published for wf-2
	time.Sleep(50 * time.Millisecond)
	startedCount = 0
	drain(published, func(env event.Envelope) {
		if event.IsWorkflowStarted(env.Type) {
			startedCount++
		}
	})
	if startedCount != 0 {
		t.Fatalf("expected 0 WorkflowStarted (wf-2 should be queued), got %d", startedCount)
	}

	if eng.throttle.queuedCount() != 1 {
		t.Fatalf("expected 1 queued, got %d", eng.throttle.queuedCount())
	}
}

func TestEngineThrottleDrainsOnTerminal(t *testing.T) {
	eng, store, bus := newThrottledTestEngine(t, 1)
	ctx := context.Background()

	def := WorkflowDef{ID: "test-wf", Required: []string{"developer"}, MaxIterations: 3}
	eng.RegisterWorkflow(def)

	published := make(chan event.Envelope, 20)
	bus.SubscribeAll(func(_ context.Context, env event.Envelope) error {
		published <- env
		return nil
	})

	// Start wf-1 (takes the only slot)
	req1 := seedWorkflowRequested(t, store, "wf-1", "test-wf")
	if err := eng.processDecision(ctx, req1); err != nil {
		t.Fatalf("process wf-1: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	drain(published, nil) // clear

	// Queue wf-2
	req2 := seedWorkflowRequested(t, store, "wf-2", "test-wf")
	if err := eng.processDecision(ctx, req2); err != nil {
		t.Fatalf("process wf-2: %v", err)
	}

	// processDecision(req1) already stored WorkflowStarted on wf-1 at version 2.
	// We also need PersonaTracked to be stored on the workflow aggregate so
	// CompletedPersonas is populated for the WorkflowCompleted decision.

	// Complete wf-1 via PersonaCompleted
	devCompleted := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer",
	})).
		WithAggregate("wf-1:persona:developer", 1).
		WithCorrelation("wf-1")
	if err := eng.processDecision(ctx, devCompleted); err != nil {
		t.Fatalf("complete wf-1: %v", err)
	}

	// Engine should have: emitted WorkflowCompleted for wf-1, decremented running,
	// then dequeued wf-2 and emitted WorkflowStarted for wf-2.
	time.Sleep(50 * time.Millisecond)
	var completedWF1, startedWF2 bool
	drain(published, func(env event.Envelope) {
		if env.Type == event.WorkflowCompleted && env.CorrelationID == "wf-1" {
			completedWF1 = true
		}
		if event.IsWorkflowStarted(env.Type) && env.CorrelationID == "wf-2" {
			startedWF2 = true
		}
	})

	if !completedWF1 {
		t.Error("expected WorkflowCompleted for wf-1")
	}
	if !startedWF2 {
		t.Error("expected WorkflowStarted for wf-2 (dequeued after wf-1 completed)")
	}
	if eng.throttle.runningCount() != 1 {
		t.Errorf("expected 1 running (wf-2), got %d", eng.throttle.runningCount())
	}
	if eng.throttle.queuedCount() != 0 {
		t.Errorf("expected 0 queued, got %d", eng.throttle.queuedCount())
	}
}

func TestEngineThrottleCancelQueued(t *testing.T) {
	eng, store, bus := newThrottledTestEngine(t, 1)
	ctx := context.Background()

	eng.RegisterWorkflow(WorkflowDef{ID: "test-wf", Required: []string{"developer"}, MaxIterations: 3})

	published := make(chan event.Envelope, 20)
	bus.SubscribeAll(func(_ context.Context, env event.Envelope) error {
		published <- env
		return nil
	})

	// Start wf-1
	req1 := seedWorkflowRequested(t, store, "wf-1", "test-wf")
	if err := eng.processDecision(ctx, req1); err != nil {
		t.Fatalf("process wf-1: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	drain(published, nil)

	// Queue wf-2
	req2 := seedWorkflowRequested(t, store, "wf-2", "test-wf")
	if err := eng.processDecision(ctx, req2); err != nil {
		t.Fatalf("process wf-2: %v", err)
	}

	if eng.throttle.queuedCount() != 1 {
		t.Fatalf("expected 1 queued, got %d", eng.throttle.queuedCount())
	}

	// Cancel wf-2 while it's queued — store needs the aggregate for replay
	cancelEvt := event.New(event.WorkflowCancelled, 1,
		event.MustMarshal(event.WorkflowCancelledPayload{Reason: "no longer needed"})).
		WithAggregate("wf-2", 2).WithCorrelation("wf-2")
	if err := store.Append(ctx, "wf-2", 1, []event.Envelope{cancelEvt}); err != nil {
		t.Fatalf("store cancel: %v", err)
	}
	if err := eng.processDecision(ctx, cancelEvt); err != nil {
		t.Fatalf("cancel wf-2: %v", err)
	}

	if eng.throttle.queuedCount() != 0 {
		t.Errorf("expected 0 queued after cancel, got %d", eng.throttle.queuedCount())
	}
	// wf-1 should still be running
	if eng.throttle.runningCount() != 1 {
		t.Errorf("expected 1 running, got %d", eng.throttle.runningCount())
	}
}

func TestEngineThrottleCancelRunningDrainsQueue(t *testing.T) {
	eng, store, bus := newThrottledTestEngine(t, 1)
	ctx := context.Background()

	eng.RegisterWorkflow(WorkflowDef{ID: "test-wf", Required: []string{"developer"}, MaxIterations: 3})

	published := make(chan event.Envelope, 20)
	bus.SubscribeAll(func(_ context.Context, env event.Envelope) error {
		published <- env
		return nil
	})

	// Start wf-1
	req1 := seedWorkflowRequested(t, store, "wf-1", "test-wf")
	if err := eng.processDecision(ctx, req1); err != nil {
		t.Fatalf("process wf-1: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	drain(published, nil)

	// Queue wf-2
	req2 := seedWorkflowRequested(t, store, "wf-2", "test-wf")
	if err := eng.processDecision(ctx, req2); err != nil {
		t.Fatalf("process wf-2: %v", err)
	}

	// Cancel running wf-1 — should free slot and dequeue wf-2.
	// processDecision(req1) stored WorkflowStarted at version 2, so cancel goes at 3.
	cancelEvt := event.New(event.WorkflowCancelled, 1,
		event.MustMarshal(event.WorkflowCancelledPayload{Reason: "abort"})).
		WithAggregate("wf-1", 3).WithCorrelation("wf-1")
	if err := store.Append(ctx, "wf-1", 2, []event.Envelope{cancelEvt}); err != nil {
		t.Fatalf("store cancel: %v", err)
	}

	if err := eng.processDecision(ctx, cancelEvt); err != nil {
		t.Fatalf("cancel wf-1: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// wf-2 should now be started
	var startedWF2 bool
	drain(published, func(env event.Envelope) {
		if event.IsWorkflowStarted(env.Type) && env.CorrelationID == "wf-2" {
			startedWF2 = true
		}
	})
	if !startedWF2 {
		t.Error("expected WorkflowStarted for wf-2 after wf-1 cancelled")
	}
	if eng.throttle.runningCount() != 1 {
		t.Errorf("expected 1 running (wf-2), got %d", eng.throttle.runningCount())
	}
}

func TestEngineNoThrottleByDefault(t *testing.T) {
	eng, _, _ := newTestEngine(t)
	if eng.throttle != nil {
		t.Error("throttle should be nil by default (RICK_MAX_WORKFLOWS not set)")
	}
}

func TestEngineSetMaxConcurrentWorkflowsZeroDisables(t *testing.T) {
	eng, _, _ := newTestEngine(t)
	eng.SetMaxConcurrentWorkflows(5)
	if eng.throttle == nil {
		t.Fatal("throttle should be set")
	}
	eng.SetMaxConcurrentWorkflows(0)
	if eng.throttle != nil {
		t.Error("SetMaxConcurrentWorkflows(0) should disable throttle")
	}
}

func TestEngineWarmThrottleNoOp(t *testing.T) {
	eng, _, _ := newTestEngine(t)
	// Should not panic when throttle is nil
	eng.WarmThrottle([]string{"wf-1", "wf-2"})
}

func TestEngineThrottleSnapshot_Disabled(t *testing.T) {
	eng, _, _ := newTestEngine(t)
	snap := eng.ThrottleSnapshot()
	if snap.Enabled || snap.MaxConcurrent != 0 || snap.Running != 0 || snap.Queued != 0 {
		t.Errorf("unset throttle must snapshot as zero-enabled, got %+v", snap)
	}
}

func TestEngineThrottleSnapshot_ReflectsState(t *testing.T) {
	ctx := context.Background()
	eng, _, _ := newTestEngine(t)
	eng.SetMaxConcurrentWorkflows(3)
	eng.WarmThrottle([]string{"wf-a", "wf-b"})
	// Simulate a queued request bypassing processLoop for the test — the
	// throttle is owned by that goroutine but we can poke it directly here
	// because the test engine is not Started.
	eng.throttle.enqueue(ctx, event.Envelope{AggregateID: "wf-queued"})

	snap := eng.ThrottleSnapshot()
	if !snap.Enabled {
		t.Fatal("snapshot should report Enabled after SetMaxConcurrentWorkflows")
	}
	if snap.MaxConcurrent != 3 {
		t.Errorf("MaxConcurrent = %d, want 3", snap.MaxConcurrent)
	}
	if snap.Running != 2 {
		t.Errorf("Running = %d, want 2", snap.Running)
	}
	if snap.Queued != 1 {
		t.Errorf("Queued = %d, want 1", snap.Queued)
	}
}

// drain reads all pending events from the channel and optionally calls fn for each.
func drain(ch <-chan event.Envelope, fn func(event.Envelope)) {
	for {
		select {
		case env := <-ch:
			if fn != nil {
				fn(env)
			}
		default:
			return
		}
	}
}

// === Throttle Watchdog Tests ===

func TestThrottleStalledEntriesEmptyWhenAllFresh(t *testing.T) {
	th := newWorkflowThrottle(5, nil, slog.Default())
	th.addRunning("wf-1")
	th.addRunning("wf-2")
	got := th.stalledEntries(time.Hour, time.Now())
	if len(got) != 0 {
		t.Errorf("expected 0 stalled when all activity fresh, got %d (%v)", len(got), got)
	}
}

func TestThrottleStalledEntriesDetectsAged(t *testing.T) {
	th := newWorkflowThrottle(5, nil, slog.Default())
	th.addRunning("fresh")
	th.addRunning("stale")
	// Backdate "stale" by hand to simulate a long-silent workflow.
	th.lastActivity["stale"] = time.Now().Add(-2 * time.Hour)
	got := th.stalledEntries(time.Hour, time.Now())
	if len(got) != 1 || got[0] != "stale" {
		t.Errorf("expected ['stale'] stalled, got %v", got)
	}
}

func TestThrottleStalledEntriesZeroThresholdSkips(t *testing.T) {
	th := newWorkflowThrottle(5, nil, slog.Default())
	th.addRunning("wf-1")
	th.lastActivity["wf-1"] = time.Now().Add(-time.Hour)
	if got := th.stalledEntries(0, time.Now()); len(got) != 0 {
		t.Errorf("threshold=0 should disable detection, got %v", got)
	}
}

func TestThrottleTouchActivityRefreshes(t *testing.T) {
	th := newWorkflowThrottle(5, nil, slog.Default())
	th.addRunning("wf-1")
	th.lastActivity["wf-1"] = time.Now().Add(-time.Hour)
	th.touchActivity("wf-1")
	if got := th.stalledEntries(30*time.Minute, time.Now()); len(got) != 0 {
		t.Errorf("touchActivity should clear staleness, got %v", got)
	}
}

func TestThrottleTouchActivityIgnoresUnknown(t *testing.T) {
	th := newWorkflowThrottle(5, nil, slog.Default())
	// Touching an ID that isn't running must not insert it.
	th.touchActivity("ghost")
	if _, ok := th.lastActivity["ghost"]; ok {
		t.Error("touchActivity must not insert entries for absent IDs")
	}
}

func TestThrottleMarkStalledFreesAndCounts(t *testing.T) {
	th := newWorkflowThrottle(5, nil, slog.Default())
	th.addRunning("wf-1")
	stamp := time.Now().Add(-90 * time.Minute)
	th.lastActivity["wf-1"] = stamp

	silence := th.markStalled("wf-1", time.Now())
	if silence < 89*time.Minute || silence > 91*time.Minute {
		t.Errorf("silence duration off: %v", silence)
	}
	if th.runningCount() != 0 {
		t.Errorf("markStalled must remove the running entry, got %d", th.runningCount())
	}
	if th.stalledCount() != 1 {
		t.Errorf("stalledCount = %d, want 1", th.stalledCount())
	}
}

// TestEngineWatchdogReclaimsStalledRunning runs runWatchdogSweep against a
// running aggregate whose lastActivity has aged past the threshold and
// asserts that a WorkflowFailed{stalled} is emitted, the slot is freed, and
// the counter increments.
func TestEngineWatchdogReclaimsStalledRunning(t *testing.T) {
	eng, store, bus := newThrottledTestEngine(t, 1)
	ctx := context.Background()
	eng.RegisterWorkflow(WorkflowDef{ID: "test-wf", Required: []string{"developer"}, MaxIterations: 3})

	published := make(chan event.Envelope, 32)
	bus.SubscribeAll(func(_ context.Context, env event.Envelope) error {
		published <- env
		return nil
	})

	// Start a workflow so it sits in throttle.running and the aggregate
	// reaches StatusRunning. Drain the dispatch artefacts.
	req := seedWorkflowRequested(t, store, "wf-1", "test-wf")
	if err := eng.processDecision(ctx, req); err != nil {
		t.Fatalf("process wf-1: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	drain(published, nil)

	if eng.throttle.runningCount() != 1 {
		t.Fatalf("expected 1 running before sweep, got %d", eng.throttle.runningCount())
	}

	// Backdate activity so the watchdog will pick it up.
	eng.throttle.lastActivity["wf-1"] = time.Now().Add(-2 * time.Hour)

	eng.stallTimeout = time.Hour
	eng.runWatchdogSweep(ctx, time.Now())

	time.Sleep(50 * time.Millisecond)
	var failedKind event.FailureKind
	var sawFailed bool
	drain(published, func(env event.Envelope) {
		if env.Type != event.WorkflowFailed {
			return
		}
		var p event.WorkflowFailedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			t.Fatalf("unmarshal WorkflowFailed: %v", err)
		}
		failedKind = p.FailureKind
		sawFailed = true
	})
	if !sawFailed {
		t.Fatal("expected WorkflowFailed published by watchdog")
	}
	if failedKind != event.FailureKindStalled {
		t.Errorf("FailureKind = %q, want %q", failedKind, event.FailureKindStalled)
	}
	if eng.throttle.runningCount() != 0 {
		t.Errorf("watchdog should free the slot, running = %d", eng.throttle.runningCount())
	}
	if got := eng.ThrottleSnapshot().Stalled; got != 1 {
		t.Errorf("ThrottleSnapshot.Stalled = %d, want 1", got)
	}
}

// TestEngineWatchdogSkipsPaused asserts that a Paused aggregate is left
// alone (no WorkflowFailed, slot retained) — paused-by-hint is intentional
// and may exceed any timeout while waiting for an operator.
func TestEngineWatchdogSkipsPaused(t *testing.T) {
	eng, store, bus := newThrottledTestEngine(t, 2)
	ctx := context.Background()
	eng.RegisterWorkflow(WorkflowDef{ID: "test-wf", Required: []string{"developer"}, MaxIterations: 3})

	published := make(chan event.Envelope, 32)
	bus.SubscribeAll(func(_ context.Context, env event.Envelope) error {
		published <- env
		return nil
	})

	req := seedWorkflowRequested(t, store, "wf-paused", "test-wf")
	if err := eng.processDecision(ctx, req); err != nil {
		t.Fatalf("process: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	drain(published, nil)

	// Append a WorkflowPaused so loadAggregate reports StatusPaused. We
	// fetch the current version first to satisfy optimistic concurrency.
	loaded, err := eng.loadAggregate(ctx, "wf-paused")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	pauseEvt := event.New(event.WorkflowPaused, 1, event.MustMarshal(event.WorkflowPausedPayload{Reason: "hint"})).
		WithAggregate("wf-paused", loaded.Version+1).
		WithCorrelation("wf-paused").
		WithSource("test")
	if err := store.Append(ctx, "wf-paused", loaded.Version, []event.Envelope{pauseEvt}); err != nil {
		t.Fatalf("append pause: %v", err)
	}

	// Backdate activity past the threshold.
	eng.throttle.lastActivity["wf-paused"] = time.Now().Add(-2 * time.Hour)
	eng.stallTimeout = time.Hour
	drain(published, nil)
	eng.runWatchdogSweep(ctx, time.Now())

	time.Sleep(50 * time.Millisecond)
	drain(published, func(env event.Envelope) {
		if env.Type == event.WorkflowFailed {
			t.Errorf("watchdog must not WorkflowFailed a paused aggregate, got source=%s", env.Source)
		}
	})
	if eng.throttle.runningCount() != 1 {
		t.Errorf("paused entry should retain its slot, running = %d", eng.throttle.runningCount())
	}
	if eng.ThrottleSnapshot().Stalled != 0 {
		t.Errorf("Stalled counter must not increment for paused aggregates")
	}
}

// TestEngineProcessDecisionRefreshesActivity asserts that any event the
// engine routes through processDecision touches the watchdog clock for its
// correlation, so a busy workflow stays out of the stalled set.
func TestEngineProcessDecisionRefreshesActivity(t *testing.T) {
	eng, store, _ := newThrottledTestEngine(t, 1)
	ctx := context.Background()
	eng.RegisterWorkflow(WorkflowDef{ID: "test-wf", Required: []string{"developer"}, MaxIterations: 3})

	req := seedWorkflowRequested(t, store, "wf-active", "test-wf")
	if err := eng.processDecision(ctx, req); err != nil {
		t.Fatalf("process: %v", err)
	}
	// Backdate activity to simulate a long-silent workflow…
	eng.throttle.lastActivity["wf-active"] = time.Now().Add(-2 * time.Hour)

	// …then route an unrelated lifecycle event through processDecision.
	// HintRejected is a cheap pick — engine subscribes to it and Decide()
	// is a no-op for an aggregate without prior HintEmitted state.
	hintEvt := event.New(event.HintRejected, 1, event.MustMarshal(event.HintRejectedPayload{Persona: "developer"})).
		WithAggregate("wf-active:persona:developer", 1).
		WithCorrelation("wf-active").
		WithSource("test")
	_ = eng.processDecision(ctx, hintEvt)

	if got := eng.throttle.stalledEntries(time.Hour, time.Now()); len(got) != 0 {
		t.Errorf("activity touch should drop wf-active out of stalled, got %v", got)
	}
}
