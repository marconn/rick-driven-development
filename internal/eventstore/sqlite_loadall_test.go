package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

func TestLoadAllEmpty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	result, err := store.LoadAll(ctx, 0, 0)
	if err != nil {
		t.Fatalf("load all empty: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected 0 events, got %d", len(result))
	}
}

func TestLoadAllReturnsEventsInOrder(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Insert events across multiple aggregates
	for _, aggID := range []string{"agg-1", "agg-2"} {
		events := []event.Envelope{
			makeEvent(event.WorkflowRequested, `{"prompt":"test"}`),
			makeEvent(event.WorkflowStarted, `{"workflow_id":"w1","phases":["a"]}`),
		}
		if err := store.Append(ctx, aggID, 0, events); err != nil {
			t.Fatalf("append %s: %v", aggID, err)
		}
	}

	result, err := store.LoadAll(ctx, 0, 0)
	if err != nil {
		t.Fatalf("load all: %v", err)
	}
	if len(result) != 4 {
		t.Fatalf("expected 4 events, got %d", len(result))
	}

	// Positions must be monotonically increasing
	for i := 1; i < len(result); i++ {
		if result[i].Position <= result[i-1].Position {
			t.Errorf("position %d (%d) should be > position %d (%d)",
				i, result[i].Position, i-1, result[i-1].Position)
		}
	}

	// First two events belong to agg-1, next two to agg-2
	if result[0].Event.AggregateID != "agg-1" {
		t.Errorf("expected agg-1, got %s", result[0].Event.AggregateID)
	}
	if result[2].Event.AggregateID != "agg-2" {
		t.Errorf("expected agg-2, got %s", result[2].Event.AggregateID)
	}
}

func TestLoadAllAfterPosition(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	events := []event.Envelope{
		makeEvent("test.1", `{}`),
		makeEvent("test.2", `{}`),
		makeEvent("test.3", `{}`),
	}
	if err := store.Append(ctx, "agg-1", 0, events); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Load all to get positions
	all, err := store.LoadAll(ctx, 0, 0)
	if err != nil {
		t.Fatalf("load all: %v", err)
	}

	// Load after position of first event
	after, err := store.LoadAll(ctx, all[0].Position, 0)
	if err != nil {
		t.Fatalf("load after: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("expected 2 events after position %d, got %d", all[0].Position, len(after))
	}
	if string(after[0].Event.Type) != "test.2" {
		t.Errorf("expected test.2, got %s", after[0].Event.Type)
	}
}

func TestLoadAllWithLimit(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	events := []event.Envelope{
		makeEvent("test.1", `{}`),
		makeEvent("test.2", `{}`),
		makeEvent("test.3", `{}`),
	}
	if err := store.Append(ctx, "agg-1", 0, events); err != nil {
		t.Fatalf("append: %v", err)
	}

	result, err := store.LoadAll(ctx, 0, 2)
	if err != nil {
		t.Fatalf("load with limit: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 events, got %d", len(result))
	}
}

func TestLoadEvent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	evt := makeEvent(event.WorkflowRequested, `{"prompt":"hello"}`)
	if err := store.Append(ctx, "agg-1", 0, []event.Envelope{evt}); err != nil {
		t.Fatalf("append: %v", err)
	}

	loaded, err := store.LoadEvent(ctx, string(evt.ID))
	if err != nil {
		t.Fatalf("load event: %v", err)
	}
	if loaded.Type != event.WorkflowRequested {
		t.Errorf("expected type %s, got %s", event.WorkflowRequested, loaded.Type)
	}
	if string(loaded.Payload) != `{"prompt":"hello"}` {
		t.Errorf("unexpected payload: %s", loaded.Payload)
	}
}

func TestLoadEventNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, err := store.LoadEvent(ctx, "nonexistent-id")
	if !errors.Is(err, ErrEventNotFound) {
		t.Errorf("expected ErrEventNotFound, got: %v", err)
	}
}

func TestDeleteDeadLetter(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	dl := DeadLetter{
		ID:       "dl-1",
		EventID:  "evt-1",
		Handler:  "test-handler",
		Error:    "boom",
		Attempts: 1,
		FailedAt: time.Now().Format(time.RFC3339),
	}
	if err := store.RecordDeadLetter(ctx, dl); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := store.DeleteDeadLetter(ctx, "dl-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Verify it's gone
	dls, err := store.LoadDeadLetters(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(dls) != 0 {
		t.Errorf("expected 0 dead letters, got %d", len(dls))
	}
}

func TestDeleteDeadLetterNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.DeleteDeadLetter(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent dead letter")
	}
}

func TestLoadAllPreservesPayload(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	payload := `{"prompt":"test payload","workflow_id":"w1","source":"raw"}`
	evt := makeEvent(event.WorkflowRequested, payload)
	if err := store.Append(ctx, "agg-1", 0, []event.Envelope{evt}); err != nil {
		t.Fatalf("append: %v", err)
	}

	result, err := store.LoadAll(ctx, 0, 0)
	if err != nil {
		t.Fatalf("load all: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(result[0].Event.Payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["prompt"] != "test payload" {
		t.Errorf("expected prompt 'test payload', got %v", got["prompt"])
	}
}
