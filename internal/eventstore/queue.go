package eventstore

import (
	"context"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// WorkflowQueueStore persists the throttle queue across restarts.
//
// The table is created automatically by SQLiteStore.migrate(). Callers must
// treat the DB as authoritative: write to DB first, then mutate in-memory
// state. On startup, LoadQueuedWorkflows re-seeds the in-memory slice.
//
// This interface is only used when RICK_MAX_WORKFLOWS is set. When the throttle
// is disabled, none of these methods are called — zero cost for unthrottled
// deployments.
type WorkflowQueueStore interface {
	// EnqueueWorkflow persists a WorkflowRequested envelope. The aggregate_id
	// column is a PRIMARY KEY, so re-enqueuing the same request is idempotent.
	EnqueueWorkflow(ctx context.Context, env event.Envelope) error

	// DequeueWorkflow removes and returns the oldest entry (FIFO by enqueued_at).
	// Returns (zero, false, nil) when the queue is empty.
	DequeueWorkflow(ctx context.Context) (event.Envelope, bool, error)

	// DeleteQueuedWorkflow removes a single row by aggregate_id (used by cancel).
	// Returns nil even if the row did not exist.
	DeleteQueuedWorkflow(ctx context.Context, aggregateID string) error

	// LoadQueuedWorkflows returns all queued envelopes ordered by enqueued_at
	// (FIFO). Called once at startup before processLoop begins.
	LoadQueuedWorkflows(ctx context.Context) ([]event.Envelope, error)
}
