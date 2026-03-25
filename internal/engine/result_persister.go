package engine

import (
	"context"
	"log/slog"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

const maxPersistRetries = 3

// resultPersister encapsulates the shared persist-and-publish pattern used by
// executeDispatch, executeHint, and executeHintApprovedDispatch.
type resultPersister struct {
	store  eventstore.Store
	bus    eventbus.Bus
	logger *slog.Logger
}

// persistAndPublish persists events to a persona-scoped aggregate with retry,
// then publishes them to the bus.
func (rp *resultPersister) persistAndPublish(ctx context.Context, aggregateID string, events []event.Envelope) {
	if len(events) == 0 {
		return
	}

	for attempt := range maxPersistRetries {
		currentVersion := 0
		if existing, loadErr := rp.store.Load(ctx, aggregateID); loadErr == nil && len(existing) > 0 {
			currentVersion = existing[len(existing)-1].Version
		}
		for i := range events {
			events[i] = events[i].WithAggregate(aggregateID, currentVersion+i+1)
		}
		persistErr := rp.store.Append(ctx, aggregateID, currentVersion, events)
		if persistErr == nil {
			break
		}
		if attempt == maxPersistRetries-1 {
			rp.logger.Error("persona runner: persist failed after retries",
				slog.String("aggregate", aggregateID),
				slog.String("error", persistErr.Error()),
			)
		}
	}

	for _, ne := range events {
		if pubErr := rp.bus.Publish(ctx, ne); pubErr != nil {
			rp.logger.Error("persona runner: publish failed",
				slog.String("event_type", string(ne.Type)),
				slog.String("error", pubErr.Error()),
			)
		}
	}
}
