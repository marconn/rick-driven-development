package engine

import (
	"log/slog"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// workflowThrottle limits the number of concurrently running workflows.
// It is owned exclusively by Engine's single FIFO goroutine — no mutex needed.
//
// When a WorkflowRequested event arrives and the running count is at capacity,
// the event is parked in a FIFO queue. When a workflow reaches a terminal state
// (completed, failed, cancelled), the throttle releases the slot and processes
// the next queued request.
//
// A maxConcurrent of 0 means unlimited — the throttle is a no-op.
type workflowThrottle struct {
	maxConcurrent int
	running       map[string]struct{} // aggregate IDs of running workflows
	queued        []event.Envelope    // parked WorkflowRequested events
	logger        *slog.Logger
}

func newWorkflowThrottle(maxConcurrent int, logger *slog.Logger) *workflowThrottle {
	return &workflowThrottle{
		maxConcurrent: maxConcurrent,
		running:       make(map[string]struct{}),
		queued:        nil,
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
func (t *workflowThrottle) enqueue(env event.Envelope) {
	t.queued = append(t.queued, env)
	t.logger.Info("engine: workflow queued (at capacity)",
		slog.String("aggregate_id", env.AggregateID),
		slog.Int("running", len(t.running)),
		slog.Int("max", t.maxConcurrent),
		slog.Int("queued", len(t.queued)),
	)
}

// dequeue removes and returns the oldest queued event, or false if empty.
func (t *workflowThrottle) dequeue() (event.Envelope, bool) {
	if len(t.queued) == 0 {
		return event.Envelope{}, false
	}
	env := t.queued[0]
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
func (t *workflowThrottle) removeQueued(aggregateID string) bool {
	for i, env := range t.queued {
		if env.AggregateID == aggregateID {
			t.queued = append(t.queued[:i], t.queued[i+1:]...)
			return true
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
