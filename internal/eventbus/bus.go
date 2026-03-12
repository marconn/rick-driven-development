package eventbus

import (
	"context"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// HandlerFunc processes an event. Returning an error triggers retry/dead-letter logic.
type HandlerFunc func(ctx context.Context, env event.Envelope) error

// SubscribeOption configures a subscription.
type SubscribeOption func(*subscribeConfig)

type subscribeConfig struct {
	name string // subscriber name for logging/metrics
	sync bool   // run handler synchronously in publisher's goroutine
}

// WithName sets a name for the subscription (used in logging and dead letters).
func WithName(name string) SubscribeOption {
	return func(c *subscribeConfig) {
		c.name = name
	}
}

// WithSync makes the handler run synchronously in the publisher's goroutine.
// Use this when the subscriber needs to preserve the publication order of events
// (e.g., Engine's FIFO channel). The handler MUST be non-blocking.
func WithSync() SubscribeOption {
	return func(c *subscribeConfig) {
		c.sync = true
	}
}

// Bus defines the in-process event pub/sub interface.
type Bus interface {
	// Publish sends an event to all matching subscribers.
	Publish(ctx context.Context, env event.Envelope) error

	// Subscribe registers a handler for a specific event type.
	// Returns an unsubscribe function.
	Subscribe(eventType event.Type, handler HandlerFunc, opts ...SubscribeOption) func()

	// SubscribeAll registers a handler for all event types (projections, logging).
	// Returns an unsubscribe function.
	SubscribeAll(handler HandlerFunc, opts ...SubscribeOption) func()

	// Close shuts down the bus and waits for in-flight events to complete.
	Close() error
}
