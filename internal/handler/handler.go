package handler

import (
	"context"
	"errors"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// ErrIncomplete is returned by handlers that have successfully processed an
// event but have more work to do. PersonaRunner persists result events but
// does NOT emit PersonaCompleted or PersonaFailed. The handler will re-trigger
// on future subscribed events (e.g., ChildWorkflowCompleted).
var ErrIncomplete = errors.New("handler: incomplete")

// Handler is the plugin interface for event-driven phases.
// Handlers subscribe to specific event types and return new events to emit.
// Returning events (instead of publishing directly) enables transactional outbox:
// the caller can persist the returned events atomically with the processing record.
type Handler interface {
	// Name returns the unique handler name (e.g., "researcher", "developer").
	Name() string

	// Subscribes returns the event types this handler wants to receive.
	Subscribes() []event.Type

	// Handle processes an event and returns zero or more events to emit.
	// The caller is responsible for publishing the returned events.
	Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error)
}

// Phased is optionally implemented by handlers that use a phase name different
// from their handler name (e.g., handler "developer" uses phase "develop").
// The engine uses this to build a phase→persona mapping for verdict resolution.
type Phased interface {
	Phase() string
}

// Hinter is optionally implemented by handlers that support pre-check hints.
// When a handler implements Hinter, PersonaRunner calls Hint() first. Full
// Handle() dispatch is gated on HintApproved for that persona+correlation.
// Handlers that don't implement Hinter execute immediately as before.
type Hinter interface {
	Hint(ctx context.Context, env event.Envelope) ([]event.Envelope, error)
}

// LifecycleHook is an optional interface handlers can implement for setup/teardown.
// Handlers that manage external resources (DB connections, HTTP clients, etc.)
// should implement this to ensure clean initialization and graceful shutdown.
type LifecycleHook interface {
	// Init is called when the handler is registered. Use for resource allocation.
	Init() error
	// Shutdown is called when the handler is unregistered or the system shuts down.
	Shutdown() error
}
