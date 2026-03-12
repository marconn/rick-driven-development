package grpchandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// InjectRequest describes an event to inject into a workflow.
type InjectRequest struct {
	CorrelationID string
	EventType     event.Type
	Payload       json.RawMessage
	Source        string
}

// EventInjector persists and publishes externally-supplied events into the
// event store. It validates the event type against the allowlist, checks
// workflow status, and handles optimistic concurrency conflicts with retry.
//
// Reusable by the gRPC stream handler, a future unary RPC, or HTTP endpoint.
type EventInjector struct {
	store  eventstore.Store
	bus    eventbus.Bus
	logger *slog.Logger
}

// NewEventInjector creates an EventInjector.
func NewEventInjector(store eventstore.Store, bus eventbus.Bus, logger *slog.Logger) *EventInjector {
	return &EventInjector{
		store:  store,
		bus:    bus,
		logger: logger,
	}
}

const maxInjectRetries = 3

// Inject validates and persists a single event, then publishes it on the bus.
// Returns the server-assigned event ID.
func (inj *EventInjector) Inject(ctx context.Context, req InjectRequest) (event.ID, error) {
	if !IsInjectable(req.EventType) {
		return "", fmt.Errorf("injector: event type %q is not injectable", req.EventType)
	}

	isNewWorkflow := req.EventType == event.WorkflowRequested

	var lastErr error
	for attempt := range maxInjectRetries {
		id, err := inj.tryInject(ctx, req, isNewWorkflow)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, eventstore.ErrConcurrencyConflict) {
			return "", err
		}
		lastErr = err
		inj.logger.Warn("injector: concurrency conflict, retrying",
			slog.String("correlation_id", req.CorrelationID),
			slog.Int("attempt", attempt+1),
		)
	}
	return "", fmt.Errorf("injector: persist failed after %d retries: %w", maxInjectRetries, lastErr)
}

func (inj *EventInjector) tryInject(ctx context.Context, req InjectRequest, isNewWorkflow bool) (event.ID, error) {
	events, err := inj.store.Load(ctx, req.CorrelationID)
	if err != nil {
		return "", fmt.Errorf("injector: load aggregate: %w", err)
	}

	currentVersion := 0
	if len(events) > 0 {
		currentVersion = events[len(events)-1].Version
	}

	if isNewWorkflow {
		if currentVersion != 0 {
			return "", fmt.Errorf("injector: workflow %q already exists", req.CorrelationID)
		}
	} else {
		if currentVersion == 0 {
			return "", fmt.Errorf("injector: workflow not found: %s", req.CorrelationID)
		}
		// Validate workflow status — reject terminal states.
		agg := engine.NewWorkflowAggregate(req.CorrelationID)
		for _, env := range events {
			agg.Apply(env)
		}
		switch agg.Status {
		case engine.StatusCompleted, engine.StatusFailed, engine.StatusCancelled:
			return "", fmt.Errorf("injector: cannot inject into %s workflow", agg.Status)
		}
	}

	env := event.New(req.EventType, 1, req.Payload).
		WithAggregate(req.CorrelationID, currentVersion+1).
		WithCorrelation(req.CorrelationID).
		WithSource(req.Source)

	if err := inj.store.Append(ctx, req.CorrelationID, currentVersion, []event.Envelope{env}); err != nil {
		return "", err
	}
	if err := inj.bus.Publish(ctx, env); err != nil {
		return "", fmt.Errorf("injector: publish: %w", err)
	}

	inj.logger.Info("injector: event injected",
		slog.String("event_id", string(env.ID)),
		slog.String("type", string(req.EventType)),
		slog.String("correlation_id", req.CorrelationID),
		slog.String("source", req.Source),
	)
	return env.ID, nil
}
