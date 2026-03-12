package handler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// mockHandler implements Handler for testing.
type mockHandler struct {
	name       string
	subscribes []event.Type
	handleFn   func(ctx context.Context, env event.Envelope) ([]event.Envelope, error)
}

func (m *mockHandler) Name() string            { return m.name }
func (m *mockHandler) Subscribes() []event.Type { return m.subscribes }
func (m *mockHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	if m.handleFn != nil {
		return m.handleFn(ctx, env)
	}
	return nil, nil
}

// mockLifecycleHandler implements Handler + LifecycleHook.
type mockLifecycleHandler struct {
	mockHandler
	initCalled     bool
	shutdownCalled bool
	initErr        error
	shutdownErr    error
}

func (m *mockLifecycleHandler) Init() error {
	m.initCalled = true
	return m.initErr
}

func (m *mockLifecycleHandler) Shutdown() error {
	m.shutdownCalled = true
	return m.shutdownErr
}

func TestRegistryRegister(t *testing.T) {
	reg := NewRegistry()

	h := &mockHandler{
		name:       "test-handler",
		subscribes: []event.Type{"test.event"},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, ok := reg.Get("test-handler")
	if !ok {
		t.Fatal("handler not found")
	}
	if got.Name() != "test-handler" {
		t.Errorf("expected test-handler, got %s", got.Name())
	}
}

func TestRegistryDuplicateRegister(t *testing.T) {
	reg := NewRegistry()

	h := &mockHandler{name: "dup"}
	if err := reg.Register(h); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := reg.Register(h); err == nil {
		t.Error("expected error for duplicate registration")
	}
}

func TestRegistryUnregister(t *testing.T) {
	reg := NewRegistry()

	h := &mockHandler{
		name:       "removable",
		subscribes: []event.Type{"test.event"},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := reg.Unregister("removable"); err != nil {
		t.Fatalf("unregister: %v", err)
	}

	_, ok := reg.Get("removable")
	if ok {
		t.Error("handler should not exist after unregister")
	}

	handlers := reg.HandlersFor("test.event")
	if len(handlers) != 0 {
		t.Error("event type index should be cleared after unregister")
	}
}

func TestRegistryUnregisterNotFound(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Unregister("nonexistent"); err == nil {
		t.Error("expected error for unregistering nonexistent handler")
	}
}

func TestRegistryHandlersFor(t *testing.T) {
	reg := NewRegistry()

	h1 := &mockHandler{name: "h1", subscribes: []event.Type{"shared.event", "h1.only"}}
	h2 := &mockHandler{name: "h2", subscribes: []event.Type{"shared.event"}}
	if err := reg.Register(h1); err != nil {
		t.Fatalf("register h1: %v", err)
	}
	if err := reg.Register(h2); err != nil {
		t.Fatalf("register h2: %v", err)
	}

	shared := reg.HandlersFor("shared.event")
	if len(shared) != 2 {
		t.Errorf("expected 2 handlers for shared.event, got %d", len(shared))
	}

	h1Only := reg.HandlersFor("h1.only")
	if len(h1Only) != 1 {
		t.Errorf("expected 1 handler for h1.only, got %d", len(h1Only))
	}

	none := reg.HandlersFor("unknown.event")
	if len(none) != 0 {
		t.Errorf("expected 0 handlers for unknown, got %d", len(none))
	}
}

func TestRegistryLifecycleHooks(t *testing.T) {
	reg := NewRegistry()

	h := &mockLifecycleHandler{
		mockHandler: mockHandler{name: "lifecycle"},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !h.initCalled {
		t.Error("Init should have been called on register")
	}

	if err := reg.Unregister("lifecycle"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if !h.shutdownCalled {
		t.Error("Shutdown should have been called on unregister")
	}
}

func TestRegistryLifecycleInitError(t *testing.T) {
	reg := NewRegistry()

	h := &mockLifecycleHandler{
		mockHandler: mockHandler{name: "fail-init"},
		initErr:     errors.New("init failed"),
	}
	if err := reg.Register(h); err == nil {
		t.Error("expected error when Init fails")
	}
	_, ok := reg.Get("fail-init")
	if ok {
		t.Error("handler should not be registered when Init fails")
	}
}

func TestRegistryAll(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(&mockHandler{name: "a"}); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if err := reg.Register(&mockHandler{name: "b"}); err != nil {
		t.Fatalf("register b: %v", err)
	}
	if err := reg.Register(&mockHandler{name: "c"}); err != nil {
		t.Fatalf("register c: %v", err)
	}

	all := reg.All()
	if len(all) != 3 {
		t.Errorf("expected 3 handlers, got %d", len(all))
	}
}

func TestRegistryNames(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(&mockHandler{name: "alpha"}); err != nil {
		t.Fatalf("register alpha: %v", err)
	}
	if err := reg.Register(&mockHandler{name: "beta"}); err != nil {
		t.Fatalf("register beta: %v", err)
	}

	names := reg.Names()
	if len(names) != 2 {
		t.Errorf("expected 2 names, got %d", len(names))
	}
}

func TestRegistryShutdownAll(t *testing.T) {
	reg := NewRegistry()

	h1 := &mockLifecycleHandler{mockHandler: mockHandler{name: "h1"}}
	h2 := &mockLifecycleHandler{mockHandler: mockHandler{name: "h2"}}
	h3 := &mockHandler{name: "h3"} // no lifecycle hooks

	if err := reg.Register(h1); err != nil {
		t.Fatalf("register h1: %v", err)
	}
	if err := reg.Register(h2); err != nil {
		t.Fatalf("register h2: %v", err)
	}
	if err := reg.Register(h3); err != nil {
		t.Fatalf("register h3: %v", err)
	}

	if err := reg.ShutdownAll(); err != nil {
		t.Fatalf("shutdown all: %v", err)
	}
	if !h1.shutdownCalled {
		t.Error("h1 Shutdown not called")
	}
	if !h2.shutdownCalled {
		t.Error("h2 Shutdown not called")
	}
}

func TestRegistryShutdownAllReturnsFirstError(t *testing.T) {
	reg := NewRegistry()

	h1 := &mockLifecycleHandler{
		mockHandler: mockHandler{name: "h1"},
		shutdownErr: errors.New("h1 shutdown failed"),
	}
	h2 := &mockLifecycleHandler{
		mockHandler: mockHandler{name: "h2"},
		shutdownErr: errors.New("h2 shutdown failed"),
	}

	if err := reg.Register(h1); err != nil {
		t.Fatalf("register h1: %v", err)
	}
	if err := reg.Register(h2); err != nil {
		t.Fatalf("register h2: %v", err)
	}

	err := reg.ShutdownAll()
	if err == nil {
		t.Error("expected error from ShutdownAll when handlers fail")
	}
	// Both handlers should have had Shutdown called despite the first error.
	if !h1.shutdownCalled {
		t.Error("h1 Shutdown not called")
	}
	if !h2.shutdownCalled {
		t.Error("h2 Shutdown not called")
	}
}

func TestHandlerHandle(t *testing.T) {
	h := &mockHandler{
		name:       "echo",
		subscribes: []event.Type{"input.event"},
		handleFn: func(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
			return []event.Envelope{
				{
					ID:            event.ID("output-1"),
					Type:          "output.event",
					AggregateID:   env.AggregateID,
					CorrelationID: env.CorrelationID,
					CausationID:   env.ID,
					Source:        "handler:echo",
					Payload:       json.RawMessage(`{"echoed":true}`),
				},
			}, nil
		},
	}

	env := event.Envelope{
		ID:            "input-1",
		Type:          "input.event",
		AggregateID:   "agg-1",
		CorrelationID: "corr-1",
		Payload:       json.RawMessage(`{}`),
	}

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result event, got %d", len(results))
	}
	if results[0].CausationID != "input-1" {
		t.Errorf("causation should reference input event, got %q", results[0].CausationID)
	}
	if results[0].Source != "handler:echo" {
		t.Errorf("expected source handler:echo, got %s", results[0].Source)
	}
	if results[0].AggregateID != "agg-1" {
		t.Errorf("expected aggregate agg-1, got %s", results[0].AggregateID)
	}
	if results[0].CorrelationID != "corr-1" {
		t.Errorf("expected correlation corr-1, got %s", results[0].CorrelationID)
	}
}

func TestHandlerHandleReturnsNilOnNoOp(t *testing.T) {
	h := &mockHandler{
		name:       "noop",
		subscribes: []event.Type{"noop.event"},
	}

	env := event.Envelope{
		ID:   "noop-1",
		Type: "noop.event",
	}

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results from noop handler, got %v", results)
	}
}

func TestRegistryGetMissingHandler(t *testing.T) {
	reg := NewRegistry()
	_, ok := reg.Get("missing")
	if ok {
		t.Error("expected Get to return false for unregistered handler")
	}
}

func TestRegistryHandlersForIsolation(t *testing.T) {
	// Verify that modifying the returned slice doesn't corrupt the registry index.
	reg := NewRegistry()
	h := &mockHandler{name: "isolated", subscribes: []event.Type{"iso.event"}}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	first := reg.HandlersFor("iso.event")
	first[0] = nil // mutate the returned copy

	second := reg.HandlersFor("iso.event")
	if second[0] == nil {
		t.Error("registry internal slice was corrupted by external mutation")
	}
}

func TestRegistryReplace(t *testing.T) {
	reg := NewRegistry()
	old := &mockHandler{name: "swappable", subscribes: []event.Type{"test.event"}}
	if err := reg.Register(old); err != nil {
		t.Fatalf("register: %v", err)
	}

	newH := &mockHandler{name: "swappable", subscribes: []event.Type{"test.event", "new.event"}}
	if err := reg.Replace("swappable", newH); err != nil {
		t.Fatalf("replace: %v", err)
	}

	got, ok := reg.Get("swappable")
	if !ok {
		t.Fatal("handler not found after replace")
	}
	if got != newH {
		t.Error("registry should return the new handler")
	}

	// byEvent should include the new subscription.
	handlers := reg.HandlersFor("new.event")
	if len(handlers) != 1 {
		t.Errorf("expected 1 handler for new.event, got %d", len(handlers))
	}
}

func TestRegistryReplaceLifecycleHooks(t *testing.T) {
	reg := NewRegistry()
	old := &mockLifecycleHandler{mockHandler: mockHandler{name: "lc-swap", subscribes: []event.Type{"test.event"}}}
	if err := reg.Register(old); err != nil {
		t.Fatalf("register: %v", err)
	}

	newH := &mockLifecycleHandler{mockHandler: mockHandler{name: "lc-swap", subscribes: []event.Type{"test.event"}}}
	if err := reg.Replace("lc-swap", newH); err != nil {
		t.Fatalf("replace: %v", err)
	}

	if !old.shutdownCalled {
		t.Error("old handler Shutdown should have been called")
	}
	if !newH.initCalled {
		t.Error("new handler Init should have been called")
	}
}

func TestRegistryReplaceNotFound(t *testing.T) {
	reg := NewRegistry()
	h := &mockHandler{name: "ghost"}
	if err := reg.Replace("ghost", h); err == nil {
		t.Error("expected error when replacing non-existent handler")
	}
}

func TestRegistryReplaceInitFailure(t *testing.T) {
	reg := NewRegistry()
	old := &mockHandler{name: "stable", subscribes: []event.Type{"test.event"}}
	if err := reg.Register(old); err != nil {
		t.Fatalf("register: %v", err)
	}

	bad := &mockLifecycleHandler{
		mockHandler: mockHandler{name: "stable", subscribes: []event.Type{"test.event"}},
		initErr:     errors.New("init failed"),
	}
	if err := reg.Replace("stable", bad); err == nil {
		t.Error("expected error when replacement Init fails")
	}

	// Old handler must still be registered after the failed replace.
	got, ok := reg.Get("stable")
	if !ok {
		t.Fatal("old handler should still be registered after failed replace")
	}
	if got != old {
		t.Error("old handler should be preserved after failed replace")
	}
}

func TestRegistryReplaceUpdatesEventIndex(t *testing.T) {
	reg := NewRegistry()
	old := &mockHandler{name: "idx-test", subscribes: []event.Type{"old.event", "shared.event"}}
	if err := reg.Register(old); err != nil {
		t.Fatalf("register: %v", err)
	}

	newH := &mockHandler{name: "idx-test", subscribes: []event.Type{"new.event", "shared.event"}}
	if err := reg.Replace("idx-test", newH); err != nil {
		t.Fatalf("replace: %v", err)
	}

	// old.event should have no handlers.
	if len(reg.HandlersFor("old.event")) != 0 {
		t.Error("old.event should have no handlers after replace")
	}
	// new.event should have the new handler.
	if len(reg.HandlersFor("new.event")) != 1 {
		t.Error("new.event should have 1 handler after replace")
	}
	// shared.event should still have exactly 1 handler (the new one).
	if len(reg.HandlersFor("shared.event")) != 1 {
		t.Error("shared.event should have exactly 1 handler after replace")
	}
}

func TestRegistryReplaceNameMismatch(t *testing.T) {
	reg := NewRegistry()
	old := &mockHandler{name: "original"}
	if err := reg.Register(old); err != nil {
		t.Fatalf("register: %v", err)
	}

	wrong := &mockHandler{name: "different-name"}
	if err := reg.Replace("original", wrong); err == nil {
		t.Error("expected error when new handler name doesn't match")
	}
}
