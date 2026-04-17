package eventstore

import (
	"context"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// TestWorkflowQueueStoreImplemented verifies compile-time interface compliance.
func TestWorkflowQueueStoreImplemented(t *testing.T) {
	var _ WorkflowQueueStore = (*SQLiteStore)(nil)
}

// makeQueueEnvelope creates a minimal WorkflowRequested envelope for queue tests.
func makeQueueEnvelope(aggID string) event.Envelope {
	return event.Envelope{
		ID:            event.NewID(),
		Type:          event.WorkflowRequested,
		AggregateID:   aggID,
		CorrelationID: aggID,
		SchemaVersion: 1,
		Timestamp:     time.Now(),
		Payload:       event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "build", WorkflowID: "test-wf"}),
	}
}

func TestEnqueueWorkflow_RoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	env := makeQueueEnvelope("wf-queue-1")
	if err := store.EnqueueWorkflow(ctx, env); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	loaded, err := store.LoadQueuedWorkflows(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 queued, got %d", len(loaded))
	}
	if loaded[0].AggregateID != "wf-queue-1" {
		t.Errorf("expected aggregate_id wf-queue-1, got %s", loaded[0].AggregateID)
	}
	if loaded[0].Type != event.WorkflowRequested {
		t.Errorf("expected WorkflowRequested, got %s", loaded[0].Type)
	}
}

func TestEnqueueWorkflow_FIFOOrdering(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Insert three envelopes with predictable ordering via different timestamps.
	ids := []string{"wf-first", "wf-second", "wf-third"}
	for i, id := range ids {
		env := makeQueueEnvelope(id)
		// Spread timestamps to guarantee ordering.
		env.Timestamp = time.Now().Add(time.Duration(i) * time.Millisecond)
		if err := store.EnqueueWorkflow(ctx, env); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}

	loaded, err := store.LoadQueuedWorkflows(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 3 {
		t.Fatalf("expected 3, got %d", len(loaded))
	}
	for i, expected := range ids {
		if loaded[i].AggregateID != expected {
			t.Errorf("position %d: want %s, got %s", i, expected, loaded[i].AggregateID)
		}
	}
}

func TestEnqueueWorkflow_Idempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	env := makeQueueEnvelope("wf-idem")
	// Enqueue twice — second call must be silently ignored.
	if err := store.EnqueueWorkflow(ctx, env); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if err := store.EnqueueWorkflow(ctx, env); err != nil {
		t.Fatalf("second enqueue (should be no-op): %v", err)
	}

	loaded, err := store.LoadQueuedWorkflows(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 {
		t.Errorf("expected 1 row (idempotent insert), got %d", len(loaded))
	}
}

func TestDeleteQueuedWorkflow(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	env1 := makeQueueEnvelope("wf-del-1")
	env2 := makeQueueEnvelope("wf-del-2")
	if err := store.EnqueueWorkflow(ctx, env1); err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	if err := store.EnqueueWorkflow(ctx, env2); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}

	// Delete first entry.
	if err := store.DeleteQueuedWorkflow(ctx, "wf-del-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	loaded, err := store.LoadQueuedWorkflows(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 || loaded[0].AggregateID != "wf-del-2" {
		t.Errorf("expected [wf-del-2] after delete, got %v", loaded)
	}
}

func TestDeleteQueuedWorkflow_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Deleting a non-existent row must return nil (idempotent).
	if err := store.DeleteQueuedWorkflow(ctx, "nonexistent"); err != nil {
		t.Errorf("delete non-existent should return nil, got: %v", err)
	}
}

func TestDequeueWorkflow_FIFO(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	env1 := makeQueueEnvelope("wf-dq-1")
	env1.Timestamp = time.Now()
	env2 := makeQueueEnvelope("wf-dq-2")
	env2.Timestamp = time.Now().Add(time.Millisecond)

	if err := store.EnqueueWorkflow(ctx, env1); err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	if err := store.EnqueueWorkflow(ctx, env2); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}

	// First dequeue returns oldest.
	got, ok, err := store.DequeueWorkflow(ctx)
	if err != nil {
		t.Fatalf("dequeue 1: %v", err)
	}
	if !ok || got.AggregateID != "wf-dq-1" {
		t.Errorf("expected wf-dq-1, got %s (ok=%v)", got.AggregateID, ok)
	}

	// Row must be gone from DB.
	loaded, _ := store.LoadQueuedWorkflows(ctx)
	if len(loaded) != 1 || loaded[0].AggregateID != "wf-dq-2" {
		t.Errorf("expected [wf-dq-2] after first dequeue, got %v", loaded)
	}

	// Second dequeue returns the remaining one.
	got, ok, err = store.DequeueWorkflow(ctx)
	if err != nil {
		t.Fatalf("dequeue 2: %v", err)
	}
	if !ok || got.AggregateID != "wf-dq-2" {
		t.Errorf("expected wf-dq-2, got %s (ok=%v)", got.AggregateID, ok)
	}

	// Empty queue returns false.
	_, ok, err = store.DequeueWorkflow(ctx)
	if err != nil {
		t.Fatalf("dequeue empty: %v", err)
	}
	if ok {
		t.Error("dequeue from empty should return ok=false")
	}
}

func TestLoadQueuedWorkflows_Empty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	loaded, err := store.LoadQueuedWorkflows(ctx)
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(loaded) != 0 {
		t.Errorf("expected 0, got %d", len(loaded))
	}
}

// TestMigration_WorkflowQueueTableCreated verifies that NewSQLiteStore creates
// the workflow_queue table on a fresh DB. This also serves as the migration
// test: existing DBs without the table get it on next open because the schema
// uses CREATE TABLE IF NOT EXISTS.
func TestMigration_WorkflowQueueTableCreated(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// If the table doesn't exist, EnqueueWorkflow would return an error.
	env := makeQueueEnvelope("migration-test")
	if err := store.EnqueueWorkflow(ctx, env); err != nil {
		t.Fatalf("workflow_queue table not created by migration: %v", err)
	}

	loaded, err := store.LoadQueuedWorkflows(ctx)
	if err != nil {
		t.Fatalf("load after migration: %v", err)
	}
	if len(loaded) != 1 {
		t.Errorf("expected 1, got %d", len(loaded))
	}
}
