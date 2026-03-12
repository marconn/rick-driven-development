package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)


// ErrHandlerNotFound is returned by Dispatch when no handler is registered
// under the requested name. It is a configuration/programming error, not a
// transient runtime failure, so callers should propagate it directly rather
// than converting it into a PersonaFailed event.
var ErrHandlerNotFound = errors.New("dispatcher: handler not found")

// Dispatcher invokes a named handler with an event and returns the resulting events.
// This seam enables swapping LocalDispatcher (in-process) for a RemoteDispatcher
// (NATS/gRPC) without touching Engine logic.
type Dispatcher interface {
	Dispatch(ctx context.Context, handlerName string, env event.Envelope) (*DispatchResult, error)
}

// DispatchResult wraps handler output with metadata.
type DispatchResult struct {
	Events  []event.Envelope
	Handler string
}

// LocalDispatcher dispatches to handlers registered in the local handler.Registry.
type LocalDispatcher struct {
	registry *handler.Registry
}

// NewLocalDispatcher creates a dispatcher backed by a local handler registry.
func NewLocalDispatcher(registry *handler.Registry) *LocalDispatcher {
	return &LocalDispatcher{registry: registry}
}

// Dispatch looks up the handler by name and calls Handle().
// Returns ErrHandlerNotFound (wrapped) if no handler is registered under handlerName.
// When the handler returns ErrIncomplete, the result events are preserved alongside
// the error so the caller can persist them without emitting PersonaCompleted.
func (d *LocalDispatcher) Dispatch(ctx context.Context, handlerName string, env event.Envelope) (*DispatchResult, error) {
	h, ok := d.registry.Get(handlerName)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrHandlerNotFound, handlerName)
	}
	events, err := h.Handle(ctx, env)
	if err != nil {
		if errors.Is(err, handler.ErrIncomplete) {
			return &DispatchResult{Events: events, Handler: handlerName}, err
		}
		return nil, err
	}
	return &DispatchResult{Events: events, Handler: handlerName}, nil
}
