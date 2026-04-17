package engine

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// newDurableTestEngine creates a throttled engine backed by a real SQLiteStore
// (in-memory) so WorkflowQueueStore is available.
func newDurableTestEngine(t *testing.T, maxConcurrent int) (*Engine, *eventstore.SQLiteStore, eventbus.Bus) {
	t.Helper()
	store, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	bus := eventbus.NewChannelBus()
	eng := NewEngine(store, bus, slog.Default())
	eng.SetMaxConcurrentWorkflows(maxConcurrent)
	t.Cleanup(func() {
		eng.Stop()
		_ = bus.Close()
		_ = store.Close()
	})
	return eng, store, bus
}

// TestDurableQueue_EnqueuePersistsToStore verifies that enqueuing a workflow
// when the throttle is at capacity writes the row to the DB.
func TestDurableQueue_EnqueuePersistsToStore(t *testing.T) {
	eng, store, _ := newDurableTestEngine(t, 1)
	ctx := context.Background()

	eng.RegisterWorkflow(WorkflowDef{ID: "test-wf", Required: []string{"developer"}, MaxIterations: 3})

	// Fill the only slot.
	req1 := seedWorkflowRequested(t, store, "wf-persist-1", "test-wf")
	if err := eng.processDecision(ctx, req1); err != nil {
		t.Fatalf("process wf-1: %v", err)
	}

	// Submit a second workflow — should be queued to DB.
	req2 := seedWorkflowRequested(t, store, "wf-persist-2", "test-wf")
	if err := eng.processDecision(ctx, req2); err != nil {
		t.Fatalf("process wf-2: %v", err)
	}

	// Verify DB row was written.
	queued, err := store.LoadQueuedWorkflows(ctx)
	if err != nil {
		t.Fatalf("load queued: %v", err)
	}
	if len(queued) != 1 || queued[0].AggregateID != "wf-persist-2" {
		t.Errorf("expected [wf-persist-2] in DB, got %v", queued)
	}
}

// TestDurableQueue_DequeueDeletesFromStore verifies that dequeuing (draining
// into a freed slot) removes the DB row.
func TestDurableQueue_DequeueDeletesFromStore(t *testing.T) {
	eng, store, _ := newDurableTestEngine(t, 1)
	ctx := context.Background()

	eng.RegisterWorkflow(WorkflowDef{ID: "test-wf", Required: []string{"developer"}, MaxIterations: 3})

	// Fill slot.
	req1 := seedWorkflowRequested(t, store, "wf-deq-1", "test-wf")
	if err := eng.processDecision(ctx, req1); err != nil {
		t.Fatalf("wf-1: %v", err)
	}

	// Queue wf-2.
	req2 := seedWorkflowRequested(t, store, "wf-deq-2", "test-wf")
	if err := eng.processDecision(ctx, req2); err != nil {
		t.Fatalf("wf-2: %v", err)
	}

	// Verify DB has the row.
	queued, _ := store.LoadQueuedWorkflows(ctx)
	if len(queued) != 1 {
		t.Fatalf("expected 1 in DB before drain, got %d", len(queued))
	}

	// Complete wf-1 to free the slot — triggers drain.
	devCompleted := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer",
	})).
		WithAggregate("wf-deq-1:persona:developer", 1).
		WithCorrelation("wf-deq-1")
	if err := eng.processDecision(ctx, devCompleted); err != nil {
		t.Fatalf("complete wf-1: %v", err)
	}

	// DB row must be gone after drain.
	queued, err := store.LoadQueuedWorkflows(ctx)
	if err != nil {
		t.Fatalf("load after drain: %v", err)
	}
	if len(queued) != 0 {
		t.Errorf("expected 0 in DB after drain, got %d: %v", len(queued), queued)
	}
}

// TestDurableQueue_CancelDeletesFromStore verifies that cancelling a queued
// workflow removes the DB row.
func TestDurableQueue_CancelDeletesFromStore(t *testing.T) {
	eng, store, _ := newDurableTestEngine(t, 1)
	ctx := context.Background()

	eng.RegisterWorkflow(WorkflowDef{ID: "test-wf", Required: []string{"developer"}, MaxIterations: 3})

	// Fill slot.
	req1 := seedWorkflowRequested(t, store, "wf-cancel-1", "test-wf")
	if err := eng.processDecision(ctx, req1); err != nil {
		t.Fatalf("wf-1: %v", err)
	}

	// Queue wf-2.
	req2 := seedWorkflowRequested(t, store, "wf-cancel-2", "test-wf")
	if err := eng.processDecision(ctx, req2); err != nil {
		t.Fatalf("wf-2: %v", err)
	}

	// Cancel wf-2 while queued.
	cancelEvt := event.New(event.WorkflowCancelled, 1,
		event.MustMarshal(event.WorkflowCancelledPayload{Reason: "operator cancelled"})).
		WithAggregate("wf-cancel-2", 2).WithCorrelation("wf-cancel-2")
	if err := store.Append(ctx, "wf-cancel-2", 1, []event.Envelope{cancelEvt}); err != nil {
		t.Fatalf("store cancel: %v", err)
	}
	if err := eng.processDecision(ctx, cancelEvt); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// DB row must be gone.
	queued, err := store.LoadQueuedWorkflows(ctx)
	if err != nil {
		t.Fatalf("load after cancel: %v", err)
	}
	if len(queued) != 0 {
		t.Errorf("expected 0 in DB after cancel, got %d: %v", len(queued), queued)
	}
}

// TestDurableQueue_RestartReloadsQueue simulates a crash-restart cycle:
// 1. Start an engine at capacity, enqueue 5 workflows.
// 2. Tear down the engine (simulate crash).
// 3. Open a new engine against the same store, call LoadQueuedWorkflows.
// 4. Verify the 5 workflows are rehydrated in FIFO order.
func TestDurableQueue_RestartReloadsQueue(t *testing.T) {
	ctx := context.Background()

	// Phase 1: fill queue in a store that persists between "restarts".
	store, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	bus1 := eventbus.NewChannelBus()
	eng1 := NewEngine(store, bus1, slog.Default())
	eng1.SetMaxConcurrentWorkflows(1)
	eng1.RegisterWorkflow(WorkflowDef{ID: "test-wf", Required: []string{"developer"}, MaxIterations: 3})

	// Fill the single slot.
	req0 := seedWorkflowRequested(t, store, "wf-slot", "test-wf")
	if err := eng1.processDecision(ctx, req0); err != nil {
		t.Fatalf("slot wf: %v", err)
	}

	// Enqueue 5 more at increasing timestamps.
	for i := 1; i <= 5; i++ {
		aggID := "wf-restart-" + string(rune('0'+i))
		env := event.New(event.WorkflowRequested, 1,
			event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "build", WorkflowID: "test-wf"})).
			WithAggregate(aggID, 1).WithCorrelation(aggID)
		env.Timestamp = time.Now().Add(time.Duration(i) * time.Millisecond)
		if err := store.Append(ctx, aggID, 0, []event.Envelope{env}); err != nil {
			t.Fatalf("append %s: %v", aggID, err)
		}
		if err := eng1.processDecision(ctx, env); err != nil {
			t.Fatalf("queue %s: %v", aggID, err)
		}
	}

	// Verify 5 rows in DB before "crash".
	dbQueued, _ := store.LoadQueuedWorkflows(ctx)
	if len(dbQueued) != 5 {
		t.Fatalf("expected 5 in DB before restart, got %d", len(dbQueued))
	}

	// Phase 2: "crash" — stop the engine without calling LoadQueuedWorkflows.
	eng1.Stop()
	_ = bus1.Close()

	// Phase 3: restart — new engine, same store.
	bus2 := eventbus.NewChannelBus()
	eng2 := NewEngine(store, bus2, slog.Default())
	eng2.SetMaxConcurrentWorkflows(1)
	eng2.RegisterWorkflow(WorkflowDef{ID: "test-wf", Required: []string{"developer"}, MaxIterations: 3})

	// Simulate recovery: warm running first, then load queue.
	eng2.WarmThrottle([]string{"wf-slot"}) // wf-slot is still "running"
	eng2.LoadQueuedWorkflows(ctx)

	// The 5 queued workflows must be back in memory in FIFO order.
	if eng2.throttle.queuedCount() != 5 {
		t.Fatalf("expected 5 queued after restart, got %d", eng2.throttle.queuedCount())
	}

	expectedOrder := []string{
		"wf-restart-1", "wf-restart-2", "wf-restart-3", "wf-restart-4", "wf-restart-5",
	}
	for i, expected := range expectedOrder {
		if eng2.throttle.queued[i].AggregateID != expected {
			t.Errorf("position %d: want %s, got %s", i, expected, eng2.throttle.queued[i].AggregateID)
		}
	}

	eng2.Stop()
	_ = bus2.Close()
	_ = store.Close()
}

// TestDurableQueue_OrphanRowCleaned verifies that LoadQueuedWorkflows deletes
// rows whose aggregate_id has no events in the events table (orphan from
// crash between queue write and events Append).
func TestDurableQueue_OrphanRowCleaned(t *testing.T) {
	ctx := context.Background()

	store, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Manually insert an orphan queue row (no corresponding events row).
	orphanEnv := event.Envelope{
		ID:            event.NewID(),
		Type:          event.WorkflowRequested,
		AggregateID:   "wf-orphan",
		CorrelationID: "wf-orphan",
		SchemaVersion: 1,
		Timestamp:     time.Now(),
		Payload:       event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "build"}),
	}
	if err := store.EnqueueWorkflow(ctx, orphanEnv); err != nil {
		t.Fatalf("enqueue orphan: %v", err)
	}

	// Also insert a valid queued workflow with a real aggregate.
	validEnv := event.New(event.WorkflowRequested, 1,
		event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "build", WorkflowID: "test-wf"})).
		WithAggregate("wf-valid", 1).WithCorrelation("wf-valid")
	if err := store.Append(ctx, "wf-valid", 0, []event.Envelope{validEnv}); err != nil {
		t.Fatalf("append valid: %v", err)
	}
	if err := store.EnqueueWorkflow(ctx, validEnv); err != nil {
		t.Fatalf("enqueue valid: %v", err)
	}

	bus := eventbus.NewChannelBus()
	eng := NewEngine(store, bus, slog.Default())
	eng.SetMaxConcurrentWorkflows(1)
	eng.WarmThrottle(nil)
	eng.LoadQueuedWorkflows(ctx)
	t.Cleanup(func() {
		eng.Stop()
		_ = bus.Close()
	})

	// Only the valid workflow should be in memory.
	if eng.throttle.queuedCount() != 1 {
		t.Errorf("expected 1 queued (orphan removed), got %d", eng.throttle.queuedCount())
	}
	if eng.throttle.queued[0].AggregateID != "wf-valid" {
		t.Errorf("expected wf-valid, got %s", eng.throttle.queued[0].AggregateID)
	}

	// DB row for orphan must be gone.
	dbQueued, err := store.LoadQueuedWorkflows(ctx)
	if err != nil {
		t.Fatalf("load after orphan cleanup: %v", err)
	}
	if len(dbQueued) != 1 || dbQueued[0].AggregateID != "wf-valid" {
		t.Errorf("expected [wf-valid] in DB, got %v", dbQueued)
	}
}

// TestDurableQueue_NoopWhenThrottleDisabled verifies that LoadQueuedWorkflows
// is a no-op when no throttle is configured.
func TestDurableQueue_NoopWhenThrottleDisabled(t *testing.T) {
	ctx := context.Background()
	eng, _, _ := newTestEngine(t) // no throttle

	// Must not panic.
	eng.LoadQueuedWorkflows(ctx)

	if eng.throttle != nil {
		t.Error("throttle should be nil")
	}
}
