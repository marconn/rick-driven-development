package engine

import (
	"context"
	"log/slog"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// internalEvents are event types handled by the Engine, projections, or
// PersonaRunner — not by persona handlers. The sentinel ignores these.
var internalEvents = map[event.Type]bool{
	event.WorkflowRequested:     true,
	event.WorkflowStarted:       true,
	event.WorkflowCompleted:     true,
	event.WorkflowFailed:        true,
	event.WorkflowCancelled:     true,
	event.WorkflowPaused:        true,
	event.WorkflowResumed:       true,
	event.WorkflowRerouted:      true,
	event.PersonaCompleted:      true,
	event.PersonaFailed:         true,
	event.VerdictRendered:       true,
	event.FeedbackGenerated:     true,
	event.FeedbackConsumed:      true,
	event.TokenBudgetExceeded:   true,
	event.AIRequestSent:         true,
	event.AIResponseReceived:    true,
	event.AIStructuredOutput:    true,
	event.CompensationStarted:   true,
	event.CompensationCompleted: true,
	event.OperatorGuidance:      true,
	event.HintEmitted:           true,
	event.HintApproved:          true,
	event.HintRejected:          true,
	event.UnhandledEventDetected: true,
	// Context snapshots are produced, not consumed by handlers.
	event.ContextCodebase:   true,
	event.ContextSchema:     true,
	event.ContextGit:        true,
	event.ContextEnrichment: true,
	event.WorkspaceReady:    true,
	// Child workflow coordination — consumed by parent handler, not persona registry.
	event.ChildWorkflowCompleted: true,
}

// Sentinel monitors the event bus for events that no handler is subscribed to.
// When an unhandled event is detected, it emits UnhandledEventDetected so the
// operator is notified. This catches misconfigured workflows, disconnected
// gRPC handlers, and events that fall through the cracks.
type Sentinel struct {
	bus    eventbus.Bus
	reg    *handler.Registry
	logger *slog.Logger
	unsub  func()

	// onUnhandled is called when an unhandled event is detected.
	// Defaults to publishing UnhandledEventDetected on the bus.
	// Tests can override this for synchronous assertions.
	onUnhandled func(ctx context.Context, env event.Envelope)
}

// NewSentinel creates a sentinel monitor.
func NewSentinel(bus eventbus.Bus, reg *handler.Registry, logger *slog.Logger) *Sentinel {
	s := &Sentinel{
		bus:    bus,
		reg:    reg,
		logger: logger,
	}
	s.onUnhandled = s.defaultOnUnhandled
	return s
}

// Start subscribes to all events on the bus.
func (s *Sentinel) Start() {
	s.unsub = s.bus.SubscribeAll(func(ctx context.Context, env event.Envelope) error {
		// Skip internal events — handled by Engine/projections/runner.
		// WorkflowStarted is a family: workflow.started, workflow.started.<id>
		if internalEvents[env.Type] || event.IsWorkflowStarted(env.Type) {
			return nil
		}

		// Check if any handler in the registry subscribes to this event type.
		handlers := s.reg.HandlersFor(env.Type)
		if len(handlers) > 0 {
			return nil
		}

		// Unhandled — no registered handler processes this event type.
		s.logger.Warn("sentinel: unhandled event detected",
			slog.String("event_type", string(env.Type)),
			slog.String("event_id", string(env.ID)),
			slog.String("correlation_id", env.CorrelationID),
			slog.String("source", env.Source),
		)
		s.onUnhandled(ctx, env)
		return nil
	}, eventbus.WithName("sentinel"))
	s.logger.Info("sentinel: started")
}

// Stop unsubscribes from the bus.
func (s *Sentinel) Stop() {
	if s.unsub != nil {
		s.unsub()
		s.unsub = nil
	}
	s.logger.Info("sentinel: stopped")
}

func (s *Sentinel) defaultOnUnhandled(ctx context.Context, env event.Envelope) {
	payload := event.MustMarshal(event.UnhandledEventPayload{
		EventType:     string(env.Type),
		EventID:       string(env.ID),
		CorrelationID: env.CorrelationID,
		Source:        env.Source,
	})
	alertEvt := event.New(event.UnhandledEventDetected, 1, payload).
		WithCorrelation(env.CorrelationID).
		WithCausation(env.ID).
		WithSource("sentinel")

	if err := s.bus.Publish(ctx, alertEvt); err != nil {
		s.logger.Error("sentinel: failed to publish alert",
			slog.String("error", err.Error()),
		)
	}
}
