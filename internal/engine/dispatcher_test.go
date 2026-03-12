package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// dispatchStubHandler satisfies handler.Handler for dispatcher unit tests.
type dispatchStubHandler struct {
	name   string
	handle func(ctx context.Context, env event.Envelope) ([]event.Envelope, error)
}

func (s *dispatchStubHandler) Name() string                   { return s.name }
func (s *dispatchStubHandler) Subscribes() []event.Type       { return nil }
func (s *dispatchStubHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	return s.handle(ctx, env)
}

func newDispatcherWithHandler(name string, fn func(context.Context, event.Envelope) ([]event.Envelope, error)) *LocalDispatcher {
	reg := handler.NewRegistry()
	_ = reg.Register(&dispatchStubHandler{name: name, handle: fn})
	return NewLocalDispatcher(reg)
}

func TestLocalDispatcher_Success(t *testing.T) {
	outEvt := event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{Phase: "develop"}))
	d := newDispatcherWithHandler("worker", func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		return []event.Envelope{outEvt}, nil
	})

	result, err := d.Dispatch(context.Background(), "worker", event.Envelope{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Handler != "worker" {
		t.Errorf("result.Handler: want %q, got %q", "worker", result.Handler)
	}
	if len(result.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(result.Events))
	}
	if result.Events[0].Type != event.AIResponseReceived {
		t.Errorf("event type: want %s, got %s", event.AIResponseReceived, result.Events[0].Type)
	}
}

func TestLocalDispatcher_HandlerNotFound(t *testing.T) {
	reg := handler.NewRegistry()
	d := NewLocalDispatcher(reg)

	result, err := d.Dispatch(context.Background(), "missing", event.Envelope{})
	if err == nil {
		t.Fatal("expected error for missing handler, got nil")
	}
	if result != nil {
		t.Errorf("expected nil result on not-found, got %+v", result)
	}
	if !errors.Is(err, ErrHandlerNotFound) {
		t.Errorf("expected errors.Is(err, ErrHandlerNotFound) to be true, got: %v", err)
	}
}

func TestLocalDispatcher_HandlerError(t *testing.T) {
	handlerErr := errors.New("handler exploded")
	d := newDispatcherWithHandler("failer", func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		return nil, handlerErr
	})

	result, err := d.Dispatch(context.Background(), "failer", event.Envelope{})
	if err == nil {
		t.Fatal("expected error propagated from handler")
	}
	if !errors.Is(err, handlerErr) {
		t.Errorf("expected wrapped handlerErr, got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result on handler error, got %+v", result)
	}
}

func TestLocalDispatcher_NilEvents(t *testing.T) {
	// Handler returns nil events — result.Events should be nil (zero-length acceptable).
	d := newDispatcherWithHandler("noop", func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		return nil, nil
	})

	result, err := d.Dispatch(context.Background(), "noop", event.Envelope{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result even with nil events")
	}
	if len(result.Events) != 0 {
		t.Errorf("expected 0 events, got %d", len(result.Events))
	}
}
