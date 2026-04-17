package engine

import (
	"context"
	"log/slog"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// workflowThrottle limits the number of concurrently running workflows.
// It is owned exclusively by Engine's single FIFO goroutine — no mutex needed.
//
// When a WorkflowRequested event arrives and the running count is at capacity,
// the event is parked in a FIFO queue. When a workflow reaches a terminal state
// (completed, failed, cancelled), the throttle releases the slot and processes
// the next queued request.
//
// When a WorkflowQueueStore is provided (non-nil), every enqueue/dequeue/remove
// is persisted to SQLite before the in-memory slice is mutated. A crash after
// the DB write but before the slice update is safe: on restart LoadQueuedWorkflows
// re-seeds the slice from DB. A crash before the DB write is also safe: the
// operator's request is never acknowledged as "queued".
//
// A maxConcurrent of 0 means unlimited — the throttle is a no-op.
type workflowThrottle struct {
	maxConcurrent int
	running       map[string]struct{} // aggregate IDs of running workflows
	queued        []event.Envelope    // parked WorkflowRequested events (in-memory FIFO)
	store         eventstore.WorkflowQueueStore
	logger        *slog.Logger
}

func newWorkflowThrottle(maxConcurrent int, store eventstore.WorkflowQueueStore, logger *slog.Logger) *workflowThrottle {
	return &workflowThrottle{
		maxConcurrent: maxConcurrent,
		running:       make(map[string]struct{}),
		queued:        nil,
		store:         store,
		logger:        logger,
	}
}

// shouldQueue returns true if the throttle is active and at capacity.
func (t *workflowThrottle) shouldQueue() bool {
	if t.maxConcurrent <= 0 {
		return false
	}
	return len(t.running) >= t.maxConcurrent
}

// enqueue parks a WorkflowRequested event for later processing.
// If a store is configured, the row is written to DB before appending to the
// in-memory slice — crash safety guarantee.
func (t *workflowThrottle) enqueue(ctx context.Context, env event.Envelope) {
	if t.store != nil {
		if err := t.store.EnqueueWorkflow(ctx, env); err != nil {
			t.logger.Error("engine: failed to persist queued workflow",
				slog.String("aggregate_id", env.AggregateID),
				slog.String("error", err.Error()),
			)
			// Fall through: we still enqueue in memory so the runtime session
			// works. The entry will be lost on crash, but we log loudly.
		}
	}
	t.queued = append(t.queued, env)
	t.logger.Info("engine: workflow queued (at capacity)",
		slog.String("aggregate_id", env.AggregateID),
		slog.Int("running", len(t.running)),
		slog.Int("max", t.maxConcurrent),
		slog.Int("queued", len(t.queued)),
	)
}

// dequeue removes and returns the oldest queued event, or (zero, false) if empty.
// If a store is configured, the DB row is deleted first — crash safety guarantee.
func (t *workflowThrottle) dequeue(ctx context.Context) (event.Envelope, bool) {
	if len(t.queued) == 0 {
		return event.Envelope{}, false
	}
	env := t.queued[0]

	if t.store != nil {
		if err := t.store.DeleteQueuedWorkflow(ctx, env.AggregateID); err != nil {
			t.logger.Error("engine: failed to delete dequeued workflow from DB",
				slog.String("aggregate_id", env.AggregateID),
				slog.String("error", err.Error()),
			)
			// Fall through: the in-memory dequeue still proceeds. The orphaned
			// DB row will be harmlessly re-loaded on the next restart and
			// re-enqueued into memory (duplicate-safe because the aggregate will
			// already be running/complete).
		}
	}

	t.queued = t.queued[1:]
	return env, true
}

// addRunning marks a workflow as running.
func (t *workflowThrottle) addRunning(aggregateID string) {
	t.running[aggregateID] = struct{}{}
}

// removeRunning removes a workflow from the running set.
// Returns true if it was present (i.e., a slot was freed).
func (t *workflowThrottle) removeRunning(aggregateID string) bool {
	_, ok := t.running[aggregateID]
	if ok {
		delete(t.running, aggregateID)
	}
	return ok
}

// removeQueued removes a workflow from the queue (e.g., cancelled before starting).
// Returns true if it was found and removed.
// If a store is configured, the DB row is deleted first — crash safety guarantee.
func (t *workflowThrottle) removeQueued(ctx context.Context, aggregateID string) bool {
	for i, env := range t.queued {
		if env.AggregateID == aggregateID {
			if t.store != nil {
				if err := t.store.DeleteQueuedWorkflow(ctx, aggregateID); err != nil {
					t.logger.Error("engine: failed to remove cancelled workflow from DB queue",
						slog.String("aggregate_id", aggregateID),
						slog.String("error", err.Error()),
					)
				}
			}
			t.queued = append(t.queued[:i], t.queued[i+1:]...)
			return true
		}
	}

	// Not in memory — may still be in DB if we recovered from a crash between
	// the DB write and the memory append. Delete it defensively.
	if t.store != nil {
		if err := t.store.DeleteQueuedWorkflow(ctx, aggregateID); err != nil {
			t.logger.Error("engine: failed to delete absent queued workflow from DB",
				slog.String("aggregate_id", aggregateID),
				slog.String("error", err.Error()),
			)
		}
	}
	return false
}

// warmRunning seeds the running set during recovery. Called once at startup
// after projections have caught up.
func (t *workflowThrottle) warmRunning(aggregateIDs []string) {
	for _, id := range aggregateIDs {
		t.running[id] = struct{}{}
	}
	if len(aggregateIDs) > 0 {
		t.logger.Info("engine: throttle warmed from recovery",
			slog.Int("running", len(t.running)),
			slog.Int("max", t.maxConcurrent),
		)
	}
}

// warmQueued seeds the in-memory queue from DB rows loaded at startup.
// Called once after warmRunning, before processLoop begins.
func (t *workflowThrottle) warmQueued(envs []event.Envelope) {
	t.queued = envs
	if len(envs) > 0 {
		t.logger.Info("engine: throttle queue rehydrated from DB",
			slog.Int("queued", len(envs)),
		)
	}
}

// enabled returns true if the throttle is active (maxConcurrent > 0).
func (t *workflowThrottle) enabled() bool {
	return t.maxConcurrent > 0
}

// runningCount returns the number of currently running workflows.
func (t *workflowThrottle) runningCount() int {
	return len(t.running)
}

// queuedCount returns the number of queued workflows.
func (t *workflowThrottle) queuedCount() int {
	return len(t.queued)
}
