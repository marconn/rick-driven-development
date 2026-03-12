package grpchandler

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// failingStore is a mock eventstore.Store that always returns a
// concurrency conflict error on Append.
type failingStore struct {
	eventstore.Store
	appendErr       error
	appendCallCount int
}

func (f *failingStore) Load(ctx context.Context, aggregateID string) ([]event.Envelope, error) {
	return f.Store.Load(ctx, aggregateID)
}

func (f *failingStore) Append(ctx context.Context, aggregateID string, expectedVersion int, events []event.Envelope) error {
	f.appendCallCount++
	return f.appendErr
}

// failingBus is an eventbus.Bus that always returns an error on Publish.
type failingBus struct {
	eventbus.Bus
	publishErr error
}

func (b *failingBus) Publish(_ context.Context, _ event.Envelope) error {
	return b.publishErr
}

// newInjectorWithStore creates an EventInjector using a custom store and bus.
func newInjectorWithStore(t *testing.T, store eventstore.Store, bus eventbus.Bus) *EventInjector {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewEventInjector(store, bus, logger)
}

// TestInjector_RetryExhaustion verifies that when all 3 retries fail due to
// concurrency conflicts, the error message includes "persist failed after 3 retries".
func TestInjector_RetryExhaustion(t *testing.T) {
	realStore, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer realStore.Close()

	bus := eventbus.NewChannelBus()
	defer bus.Close()

	ctx := context.Background()

	// Seed a running workflow in the real store so the injector can Load it.
	requested := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "workspace-dev",
	})).
		WithAggregate("wf-exhaust", 1).
		WithCorrelation("wf-exhaust").
		WithSource("test")
	started := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "workspace-dev", Phases: []string{"researcher"},
	})).
		WithAggregate("wf-exhaust", 2).
		WithCorrelation("wf-exhaust").
		WithSource("engine:aggregate")
	if err := realStore.Append(ctx, "wf-exhaust", 0, []event.Envelope{requested, started}); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}

	// Wrap with a store that always returns concurrency conflict on Append.
	store := &failingStore{
		Store:     realStore,
		appendErr: eventstore.ErrConcurrencyConflict,
	}

	inj := newInjectorWithStore(t, store, bus)

	_, gotErr := inj.Inject(ctx, InjectRequest{
		CorrelationID: "wf-exhaust",
		EventType:     event.OperatorGuidance,
		Payload:       []byte(`{"guidance":"retry test"}`),
		Source:        "test",
	})

	if gotErr == nil {
		t.Fatal("expected error after retry exhaustion, got nil")
	}
	if !errors.Is(gotErr, eventstore.ErrConcurrencyConflict) {
		t.Errorf("expected error to wrap ErrConcurrencyConflict, got: %v", gotErr)
	}

	const wantMsg = "persist failed after 3 retries"
	if !containsSubstring(gotErr.Error(), wantMsg) {
		t.Errorf("expected error to contain %q, got: %q", wantMsg, gotErr.Error())
	}

	// Verify all 3 retries were attempted.
	if store.appendCallCount != maxInjectRetries {
		t.Errorf("expected %d Append calls (one per retry), got %d",
			maxInjectRetries, store.appendCallCount)
	}
}

// TestInjector_PublishFailure verifies that when Append succeeds but Publish
// fails, the error is wrapped with context and returned.
func TestInjector_PublishFailure(t *testing.T) {
	realStore, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer realStore.Close()

	ctx := context.Background()

	// Seed a running workflow.
	requested := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "workspace-dev",
	})).
		WithAggregate("wf-publish-fail", 1).
		WithCorrelation("wf-publish-fail").
		WithSource("test")
	started := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "workspace-dev", Phases: []string{"researcher"},
	})).
		WithAggregate("wf-publish-fail", 2).
		WithCorrelation("wf-publish-fail").
		WithSource("engine:aggregate")
	if err := realStore.Append(ctx, "wf-publish-fail", 0, []event.Envelope{requested, started}); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}

	publishErrMsg := "bus: transport unavailable"
	bus := &failingBus{
		Bus:        eventbus.NewChannelBus(),
		publishErr: errors.New(publishErrMsg),
	}
	defer bus.Bus.Close()

	inj := newInjectorWithStore(t, realStore, bus)

	_, gotErr := inj.Inject(ctx, InjectRequest{
		CorrelationID: "wf-publish-fail",
		EventType:     event.OperatorGuidance,
		Payload:       []byte(`{"guidance":"publish fail test"}`),
		Source:        "test",
	})

	if gotErr == nil {
		t.Fatal("expected error when publish fails, got nil")
	}

	// Error must mention publish.
	if !containsSubstring(gotErr.Error(), "publish") {
		t.Errorf("expected error to mention 'publish', got: %q", gotErr.Error())
	}
}

// containsSubstring is a simple helper to avoid importing strings.Contains
// in test code while keeping helper logic readable.
func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsAt(s, sub))
}

func containsAt(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
