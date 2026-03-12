package grpchandler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

func newInjectorTestEnv(t *testing.T) (*EventInjector, eventstore.Store, *eventbus.ChannelBus) {
	t.Helper()
	store, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	bus := eventbus.NewChannelBus()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	inj := NewEventInjector(store, bus, logger)
	t.Cleanup(func() {
		_ = bus.Close()
		_ = store.Close()
	})
	return inj, store, bus
}

// seedWorkflow creates a running workflow aggregate for testing.
func seedWorkflow(t *testing.T, ctx context.Context, store eventstore.Store, bus *eventbus.ChannelBus, wfID string) {
	t.Helper()
	requested := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "workspace-dev",
	})).
		WithAggregate(wfID, 1).
		WithCorrelation(wfID).
		WithSource("test")

	started := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "workspace-dev", Phases: []string{"researcher"},
	})).
		WithAggregate(wfID, 2).
		WithCorrelation(wfID).
		WithSource("engine:aggregate")

	if err := store.Append(ctx, wfID, 0, []event.Envelope{requested, started}); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
}

func TestInjector_HappyPath_OperatorGuidance(t *testing.T) {
	inj, store, bus := newInjectorTestEnv(t)
	ctx := context.Background()

	seedWorkflow(t, ctx, store, bus, "wf-1")

	// Subscribe to verify publish.
	publishCh := make(chan event.Envelope, 1)
	unsub := bus.Subscribe(event.OperatorGuidance, func(_ context.Context, env event.Envelope) error {
		publishCh <- env
		return nil
	}, eventbus.WithName("test:guidance"))
	defer unsub()

	payload, _ := json.Marshal(map[string]string{"guidance": "use React"})
	id, err := inj.Inject(ctx, InjectRequest{
		CorrelationID: "wf-1",
		EventType:     event.OperatorGuidance,
		Payload:       payload,
		Source:        "grpc:test-handler",
	})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty event ID")
	}

	// Verify persisted.
	events, _ := store.Load(ctx, "wf-1")
	if events[len(events)-1].Type != event.OperatorGuidance {
		t.Errorf("expected last event to be OperatorGuidance, got %s", events[len(events)-1].Type)
	}
	if events[len(events)-1].Source != "grpc:test-handler" {
		t.Errorf("expected source grpc:test-handler, got %s", events[len(events)-1].Source)
	}

	// Verify published (wait for async delivery).
	select {
	case published := <-publishCh:
		if published.Type != event.OperatorGuidance {
			t.Errorf("expected published OperatorGuidance, got %s", published.Type)
		}
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for published event")
	}
}

func TestInjector_AllowlistRejection(t *testing.T) {
	inj, _, _ := newInjectorTestEnv(t)
	ctx := context.Background()

	for _, blocked := range []event.Type{event.PersonaCompleted, event.WorkflowStarted, event.FeedbackGenerated} {
		_, err := inj.Inject(ctx, InjectRequest{
			CorrelationID: "wf-1",
			EventType:     blocked,
			Payload:       []byte(`{}`),
			Source:        "test",
		})
		if err == nil {
			t.Errorf("expected error for blocked type %q", blocked)
		}
	}
}

func TestInjector_WorkflowNotFound(t *testing.T) {
	inj, _, _ := newInjectorTestEnv(t)
	ctx := context.Background()

	_, err := inj.Inject(ctx, InjectRequest{
		CorrelationID: "nonexistent",
		EventType:     event.OperatorGuidance,
		Payload:       []byte(`{}`),
		Source:        "test",
	})
	if err == nil {
		t.Fatal("expected error for non-existent workflow")
	}
}

func TestInjector_StatusRejection(t *testing.T) {
	inj, store, bus := newInjectorTestEnv(t)
	ctx := context.Background()

	seedWorkflow(t, ctx, store, bus, "wf-done")

	// Complete the workflow.
	completed := event.New(event.WorkflowCompleted, 1, event.MustMarshal(event.WorkflowCompletedPayload{
		Result: "done",
	})).
		WithAggregate("wf-done", 3).
		WithCorrelation("wf-done").
		WithSource("engine:aggregate")

	if err := store.Append(ctx, "wf-done", 2, []event.Envelope{completed}); err != nil {
		t.Fatalf("append completed: %v", err)
	}

	_, err := inj.Inject(ctx, InjectRequest{
		CorrelationID: "wf-done",
		EventType:     event.OperatorGuidance,
		Payload:       []byte(`{}`),
		Source:        "test",
	})
	if err == nil {
		t.Fatal("expected error for completed workflow")
	}
}

func TestInjector_NewWorkflow_WorkflowRequested(t *testing.T) {
	inj, store, _ := newInjectorTestEnv(t)
	ctx := context.Background()

	payload := event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "build something",
		WorkflowID: "workspace-dev",
	})
	id, err := inj.Inject(ctx, InjectRequest{
		CorrelationID: "wf-new",
		EventType:     event.WorkflowRequested,
		Payload:       payload,
		Source:        "grpc:ci-system",
	})
	if err != nil {
		t.Fatalf("inject WorkflowRequested: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty event ID")
	}

	events, _ := store.Load(ctx, "wf-new")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != event.WorkflowRequested {
		t.Errorf("expected WorkflowRequested, got %s", events[0].Type)
	}
	if events[0].Version != 1 {
		t.Errorf("expected version 1, got %d", events[0].Version)
	}
}

func TestInjector_WorkflowRequested_AlreadyExists(t *testing.T) {
	inj, store, bus := newInjectorTestEnv(t)
	ctx := context.Background()

	seedWorkflow(t, ctx, store, bus, "wf-exists")

	_, err := inj.Inject(ctx, InjectRequest{
		CorrelationID: "wf-exists",
		EventType:     event.WorkflowRequested,
		Payload:       event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "dup"}),
		Source:        "test",
	})
	if err == nil {
		t.Fatal("expected error for duplicate WorkflowRequested")
	}
}

func TestInjector_ConcurrentInjects(t *testing.T) {
	inj, store, bus := newInjectorTestEnv(t)
	ctx := context.Background()

	seedWorkflow(t, ctx, store, bus, "wf-concurrent")

	var wg sync.WaitGroup
	errs := make(chan error, 2)

	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			payload, _ := json.Marshal(map[string]int{"idx": idx})
			_, err := inj.Inject(ctx, InjectRequest{
				CorrelationID: "wf-concurrent",
				EventType:     event.OperatorGuidance,
				Payload:       payload,
				Source:        "grpc:concurrent",
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent inject failed: %v", err)
	}

	// Both should have persisted.
	events, _ := store.Load(ctx, "wf-concurrent")
	guidanceCount := 0
	for _, e := range events {
		if e.Type == event.OperatorGuidance {
			guidanceCount++
		}
	}
	if guidanceCount != 2 {
		t.Errorf("expected 2 OperatorGuidance events, got %d", guidanceCount)
	}
}

// Verify WorkflowAggregate is used for status validation.
func TestInjector_CancelledWorkflow(t *testing.T) {
	inj, store, bus := newInjectorTestEnv(t)
	ctx := context.Background()

	seedWorkflow(t, ctx, store, bus, "wf-cancel")

	cancelled := event.New(event.WorkflowCancelled, 1, event.MustMarshal(event.WorkflowCancelledPayload{
		Reason: "test", Source: "test",
	})).
		WithAggregate("wf-cancel", 3).
		WithCorrelation("wf-cancel").
		WithSource("test")

	if err := store.Append(ctx, "wf-cancel", 2, []event.Envelope{cancelled}); err != nil {
		t.Fatalf("append cancelled: %v", err)
	}

	_, err := inj.Inject(ctx, InjectRequest{
		CorrelationID: "wf-cancel",
		EventType:     event.OperatorGuidance,
		Payload:       []byte(`{}`),
		Source:        "test",
	})
	if err == nil {
		t.Fatal("expected error for cancelled workflow")
	}
}

// Verify context injection into paused workflow works.
func TestInjector_PausedWorkflow_Allowed(t *testing.T) {
	inj, store, bus := newInjectorTestEnv(t)
	ctx := context.Background()

	seedWorkflow(t, ctx, store, bus, "wf-paused")

	paused := event.New(event.WorkflowPaused, 1, event.MustMarshal(event.WorkflowPausedPayload{
		Reason: "test", Source: "test",
	})).
		WithAggregate("wf-paused", 3).
		WithCorrelation("wf-paused").
		WithSource("test")

	if err := store.Append(ctx, "wf-paused", 2, []event.Envelope{paused}); err != nil {
		t.Fatalf("append paused: %v", err)
	}

	_, err := inj.Inject(ctx, InjectRequest{
		CorrelationID: "wf-paused",
		EventType:     event.OperatorGuidance,
		Payload:       []byte(`{"guidance":"continue"}`),
		Source:        "test",
	})
	if err != nil {
		t.Fatalf("expected inject into paused workflow to succeed: %v", err)
	}
}
