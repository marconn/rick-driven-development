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

// WorkflowRegistry is the narrow dependency EventInjector needs to validate
// that a WorkflowRequested references a registered DAG before persisting it.
// *engine.Engine implements this. Provided as an interface so callers (and
// tests) can plug in their own registry without depending on the full
// engine type.
//
// May be nil; when nil, WorkflowID validation is skipped. Production wiring
// should always pass a real registry — passing nil only exists so legacy
// tests that don't exercise the WorkflowRequested path don't have to
// construct an engine.
type WorkflowRegistry interface {
	GetWorkflowDef(id string) (engine.WorkflowDef, bool)
}

// EventInjector persists and publishes externally-supplied events into the
// event store. It validates the event type against the allowlist, checks
// workflow status, validates the WorkflowID for new workflows, and handles
// optimistic concurrency conflicts with retry.
//
// Reusable by the gRPC stream handler, a future unary RPC, or HTTP endpoint.
type EventInjector struct {
	store    eventstore.Store
	bus      eventbus.Bus
	registry WorkflowRegistry
	logger   *slog.Logger
}

// NewEventInjector creates an EventInjector. registry may be nil in tests
// that don't inject WorkflowRequested events; production callers must pass
// a real registry so injected WorkflowRequested events are validated
// against the engine's known DAGs.
func NewEventInjector(store eventstore.Store, bus eventbus.Bus, registry WorkflowRegistry, logger *slog.Logger) *EventInjector {
	return &EventInjector{
		store:    store,
		bus:      bus,
		registry: registry,
		logger:   logger,
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
		// Validate WorkflowID points at a registered DAG before we persist
		// the event. Without this check, a typo or env-var drift produces
		// a WorkflowRequested the engine can't act on; the engine's
		// fail-fast guard will turn it into WorkflowFailed, but rejecting
		// at the injector boundary gives the gRPC caller a synchronous
		// error and avoids a polluting failed-workflow row in the store.
		// docs/bugs/jira-dev-stuck-in-requested.md.
		if inj.registry != nil {
			var p event.WorkflowRequestedPayload
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return "", fmt.Errorf("injector: decode WorkflowRequestedPayload: %w", err)
			}
			if p.WorkflowID == "" {
				return "", errors.New("injector: WorkflowRequested missing workflow_id")
			}
			if _, ok := inj.registry.GetWorkflowDef(p.WorkflowID); !ok {
				return "", fmt.Errorf("injector: workflow_id %q is not a registered DAG (typo, plugin not loaded, or env-var drift)", p.WorkflowID)
			}
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
