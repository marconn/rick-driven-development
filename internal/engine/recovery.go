package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/projection"
)

// RecoveryResult summarises a single recovery scan.
type RecoveryResult struct {
	Recovered     int // handlers dispatched
	PausedRestored int // paused workflows whose state was restored
	Skipped       int // workflows skipped (def missing, terminal, etc.)
	Errors        int // workflows that hit errors during recovery
}

// RecoveryScanner examines workflows left in non-terminal status after a
// server restart and resumes them by dispatching eligible handlers through
// PersonaRunner.RecoverDispatch. It runs once during startup, after
// projections have caught up and PersonaRunner has subscribed to the live bus.
type RecoveryScanner struct {
	store     eventstore.Store
	workflows *projection.WorkflowStatusProjection
	runner    *PersonaRunner
	engine    *Engine
	logger    *slog.Logger
}

// NewRecoveryScanner creates a recovery scanner.
func NewRecoveryScanner(
	store eventstore.Store,
	workflows *projection.WorkflowStatusProjection,
	runner *PersonaRunner,
	engine *Engine,
	logger *slog.Logger,
) *RecoveryScanner {
	return &RecoveryScanner{
		store:     store,
		workflows: workflows,
		runner:    runner,
		engine:    engine,
		logger:    logger,
	}
}

// Recover scans all non-terminal workflows and resumes eligible handlers.
func (s *RecoveryScanner) Recover(ctx context.Context) RecoveryResult {
	var result RecoveryResult
	var runningIDs []string

	for _, ws := range s.workflows.All() {
		switch ws.Status {
		case "running", "paused":
			// candidate for recovery — also counts toward throttle
			runningIDs = append(runningIDs, ws.AggregateID)
		default:
			continue // terminal or requested — skip
		}

		recovered, paused, err := s.recoverWorkflow(ctx, ws)
		if err != nil {
			s.logger.Error("recovery: failed to recover workflow",
				slog.String("correlation", ws.AggregateID),
				slog.String("workflow", ws.WorkflowID),
				slog.String("error", err.Error()),
			)
			result.Errors++
			continue
		}
		result.Recovered += recovered
		if paused {
			result.PausedRestored++
		}
	}

	// Warm the throttle with workflows that survived the restart.
	s.engine.WarmThrottle(runningIDs)

	return result
}

// recoverWorkflow attempts to resume a single workflow.
// Returns the number of handlers dispatched and whether pause state was restored.
func (s *RecoveryScanner) recoverWorkflow(ctx context.Context, ws projection.WorkflowStatus) (int, bool, error) {
	// Replay the aggregate from the event store — this is authoritative.
	agg, err := s.replayAggregate(ctx, ws.AggregateID)
	if err != nil {
		return 0, false, err
	}

	// Look up the workflow definition.
	def, ok := s.engine.GetWorkflowDef(agg.WorkflowID)
	if !ok {
		s.logger.Warn("recovery: workflow def not found, skipping",
			slog.String("correlation", ws.AggregateID),
			slog.String("workflow_id", agg.WorkflowID),
		)
		return 0, false, nil
	}

	// Always warm the correlation cache so resolveWorkflowID works.
	s.runner.WarmCorrelationCache(ws.AggregateID, agg.WorkflowID)

	// Handle paused workflows: restore state, no dispatch.
	if agg.Status == StatusPaused {
		s.runner.WarmPauseState(ws.AggregateID)
		s.logger.Info("recovery: restored paused workflow",
			slog.String("correlation", ws.AggregateID),
			slog.String("workflow", agg.WorkflowID),
		)
		return 0, true, nil
	}

	// Only recover running workflows from here.
	if agg.Status != StatusRunning {
		return 0, false, nil
	}

	// Load all events by correlation — needed for trigger event discovery
	// and hint-pending detection.
	events, err := s.store.LoadByCorrelation(ctx, ws.AggregateID)
	if err != nil {
		return 0, false, err
	}

	// Detect hint-pending handlers: HintEmitted without HintApproved/HintRejected.
	hintPending := s.detectHintPending(events)
	if len(hintPending) > 0 {
		s.runner.WarmPauseState(ws.AggregateID)
		s.logger.Info("recovery: workflow has pending hints, restoring paused state",
			slog.String("correlation", ws.AggregateID),
			slog.String("workflow", agg.WorkflowID),
			slog.Any("hint_pending", hintPending),
		)
		return 0, true, nil
	}

	// Edge case: all Required personas have completed but no WorkflowCompleted.
	// Re-publish the last PersonaCompleted to trigger Engine.Decide().
	if s.allRequiredDone(agg, def) {
		trigger := s.findLastPersonaCompleted(events)
		if trigger != nil {
			if pubErr := s.runner.bus.Publish(ctx, *trigger); pubErr != nil {
				s.logger.Error("recovery: failed to re-publish terminal trigger",
					slog.String("correlation", ws.AggregateID),
					slog.String("error", pubErr.Error()),
				)
			}
		}
		return 0, false, nil
	}

	// Walk the DAG to find eligible handlers.
	eligible := s.findEligibleHandlers(agg, def, events)

	dispatched := 0
	for _, eh := range eligible {
		if err := s.runner.RecoverDispatch(eh.handler, eh.trigger); err != nil {
			s.logger.Warn("recovery: dispatch failed",
				slog.String("handler", eh.handler),
				slog.String("correlation", ws.AggregateID),
				slog.String("error", err.Error()),
			)
			continue
		}
		dispatched++
	}

	return dispatched, false, nil
}

// eligibleHandler pairs a handler name with the event that should trigger it.
type eligibleHandler struct {
	handler string
	trigger event.Envelope
}

// findEligibleHandlers walks the workflow DAG and returns handlers whose
// predecessors are all satisfied but that have not yet completed.
func (s *RecoveryScanner) findEligibleHandlers(
	agg *WorkflowAggregate,
	def WorkflowDef,
	events []event.Envelope,
) []eligibleHandler {
	var eligible []eligibleHandler

	for handlerName, predecessors := range def.Graph {
		// Skip already-completed handlers.
		if agg.CompletedPersonas[handlerName] {
			continue
		}

		// Check if this handler is in a feedback retrigger cycle.
		if s.isFeedbackRetrigger(handlerName, def, events) {
			trigger := s.findLastEventOfType(events, event.FeedbackGenerated)
			if trigger != nil {
				eligible = append(eligible, eligibleHandler{
					handler: handlerName,
					trigger: *trigger,
				})
			}
			continue
		}

		// Root handlers (empty predecessors): fire on WorkflowStarted.
		if len(predecessors) == 0 {
			trigger := s.findWorkflowStartedEvent(events)
			if trigger != nil {
				eligible = append(eligible, eligibleHandler{
					handler: handlerName,
					trigger: *trigger,
				})
			}
			continue
		}

		// Non-root: check if all predecessors have completed.
		allDone := true
		for _, pred := range predecessors {
			if !agg.CompletedPersonas[pred] {
				allDone = false
				break
			}
		}
		if !allDone {
			continue
		}

		// Find a trigger event: the most recent PersonaCompleted from a predecessor.
		trigger := s.findPredecessorCompleted(events, predecessors)
		if trigger != nil {
			eligible = append(eligible, eligibleHandler{
				handler: handlerName,
				trigger: *trigger,
			})
		}
	}

	return eligible
}

// isFeedbackRetrigger checks if a handler is in a feedback retrigger cycle:
// it's listed in RetriggeredBy for FeedbackGenerated, and there's an
// unresolved FeedbackGenerated (no PersonaCompleted for this handler after it).
func (s *RecoveryScanner) isFeedbackRetrigger(
	handlerName string,
	def WorkflowDef,
	events []event.Envelope,
) bool {
	// Check if handler is retriggerable by FeedbackGenerated.
	triggers, ok := def.RetriggeredBy[handlerName]
	if !ok {
		return false
	}
	if !slices.Contains(triggers, event.FeedbackGenerated) {
		return false
	}

	// Scan events: is there a FeedbackGenerated without a subsequent
	// PersonaCompleted from this handler?
	lastFeedback := -1
	lastCompletion := -1
	for i, e := range events {
		if e.Type == event.FeedbackGenerated {
			lastFeedback = i
		}
		if e.Type == event.PersonaCompleted {
			var pc event.PersonaCompletedPayload
			if err := json.Unmarshal(e.Payload, &pc); err == nil && pc.Persona == handlerName {
				lastCompletion = i
			}
		}
	}

	return lastFeedback > lastCompletion
}

// detectHintPending scans events for HintEmitted without a corresponding
// HintApproved or HintRejected. Returns the persona names with pending hints.
func (s *RecoveryScanner) detectHintPending(events []event.Envelope) []string {
	pending := make(map[string]bool) // persona → has pending hint

	for _, e := range events {
		switch e.Type {
		case event.HintEmitted:
			var p event.HintEmittedPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				pending[p.Persona] = true
			}
		case event.HintApproved:
			var p event.HintApprovedPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				delete(pending, p.Persona)
			}
		case event.HintRejected:
			var p event.HintRejectedPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				delete(pending, p.Persona)
			}
		}
	}

	result := make([]string, 0, len(pending))
	for persona := range pending {
		result = append(result, persona)
	}
	return result
}

// allRequiredDone returns true if all Required personas have completed.
func (s *RecoveryScanner) allRequiredDone(agg *WorkflowAggregate, def WorkflowDef) bool {
	for _, req := range def.Required {
		if !agg.CompletedPersonas[req] {
			return false
		}
	}
	return len(def.Required) > 0
}

// replayAggregate loads and replays a workflow aggregate from the event store.
func (s *RecoveryScanner) replayAggregate(ctx context.Context, aggregateID string) (*WorkflowAggregate, error) {
	agg := NewWorkflowAggregate(aggregateID)

	events, err := s.store.Load(ctx, aggregateID)
	if err != nil {
		return nil, err
	}

	for _, env := range events {
		agg.Apply(env)
	}

	return agg, nil
}

// --- Trigger event finders ---

// findWorkflowStartedEvent finds the first workflow.started.* event.
func (s *RecoveryScanner) findWorkflowStartedEvent(events []event.Envelope) *event.Envelope {
	for i := range events {
		if event.IsWorkflowStarted(events[i].Type) {
			return &events[i]
		}
	}
	return nil
}

// findPredecessorCompleted finds the most recent PersonaCompleted from any
// of the given predecessors.
func (s *RecoveryScanner) findPredecessorCompleted(events []event.Envelope, predecessors []string) *event.Envelope {
	predSet := make(map[string]bool, len(predecessors))
	for _, p := range predecessors {
		predSet[p] = true
	}

	var best *event.Envelope
	for i := range events {
		if events[i].Type != event.PersonaCompleted {
			continue
		}
		var pc event.PersonaCompletedPayload
		if err := json.Unmarshal(events[i].Payload, &pc); err == nil && predSet[pc.Persona] {
			best = &events[i]
		}
	}
	return best
}

// findLastPersonaCompleted finds the most recent PersonaCompleted event.
func (s *RecoveryScanner) findLastPersonaCompleted(events []event.Envelope) *event.Envelope {
	var best *event.Envelope
	for i := range events {
		if events[i].Type == event.PersonaCompleted {
			best = &events[i]
		}
	}
	return best
}

// findLastEventOfType finds the most recent event of the given type.
func (s *RecoveryScanner) findLastEventOfType(events []event.Envelope, t event.Type) *event.Envelope {
	var best *event.Envelope
	for i := range events {
		if events[i].Type == t {
			best = &events[i]
		}
	}
	return best
}
