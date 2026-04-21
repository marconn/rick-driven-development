package projection

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// WorkflowStatusProjection maintains the current status of all workflows.
type WorkflowStatusProjection struct {
	mu        sync.RWMutex
	workflows map[string]*WorkflowStatus
	// hintPersonas tracks which personas have emitted a hint that has not yet
	// been approved or rejected. Keyed by workflowID (== correlationID).
	// A HintEmitted event lives on a persona-scoped aggregate, so we look it
	// up via CorrelationID rather than AggregateID.
	hintPersonas map[string]map[string]struct{}
}

// NewWorkflowStatusProjection creates a new workflow status projection.
func NewWorkflowStatusProjection() *WorkflowStatusProjection {
	return &WorkflowStatusProjection{
		workflows:    make(map[string]*WorkflowStatus),
		hintPersonas: make(map[string]map[string]struct{}),
	}
}

func (p *WorkflowStatusProjection) Name() string { return "workflow-status" }

func (p *WorkflowStatusProjection) Handle(_ context.Context, env event.Envelope) error { //nolint:cyclop // single-switch dispatch is intentionally broad
	isStarted := event.IsWorkflowStarted(env.Type)
	switch {
	case env.Type == event.WorkflowRequested || isStarted ||
		env.Type == event.WorkflowCompleted || env.Type == event.WorkflowFailed ||
		env.Type == event.WorkflowCancelled ||
		env.Type == event.WorkflowPaused || env.Type == event.WorkflowResumed ||
		env.Type == event.HintEmitted ||
		env.Type == event.HintApproved ||
		env.Type == event.HintRejected:
		// handled below
	default:
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Hint events use CorrelationID as the workflow key; lifecycle events use
	// AggregateID. Both equal the workflow aggregate ID by convention.
	switch env.Type {
	case event.HintEmitted:
		return p.handleHintEmitted(env)
	case event.HintApproved:
		return p.handleHintApproved(env)
	case event.HintRejected:
		return p.handleHintRejected(env)
	}

	ws := p.getOrCreate(env.AggregateID)

	switch {
	case env.Type == event.WorkflowRequested:
		var payload event.WorkflowRequestedPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return err
		}
		ws.Status = "requested"
		ws.WorkflowID = payload.WorkflowID
		ws.Prompt = payload.Prompt
		ws.Source = payload.Source
		ws.Ticket = payload.Ticket

	case isStarted:
		var payload event.WorkflowStartedPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return err
		}
		ws.Status = "running"
		ws.Phases = payload.Phases
		ws.StartedAt = env.Timestamp

	case env.Type == event.WorkflowCompleted:
		ws.Status = "completed"
		ws.CompletedAt = env.Timestamp

	case env.Type == event.WorkflowFailed:
		var payload event.WorkflowFailedPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return err
		}
		ws.Status = "failed"
		ws.CompletedAt = env.Timestamp
		ws.FailReason = payload.Reason
		ws.FailPhase = payload.Phase
		ws.FailureKind = string(payload.FailureKind)
		ws.FailBackend = payload.Backend
		ws.FailStderr = payload.Stderr

	case env.Type == event.WorkflowCancelled:
		ws.Status = "cancelled"
		ws.CompletedAt = env.Timestamp

	case env.Type == event.WorkflowPaused:
		ws.Status = "paused"

	case env.Type == event.WorkflowResumed:
		ws.Status = "running"
	}
	return nil
}

// handleHintEmitted records a new pending hint for the persona.
// HintEmitted is published on a persona-scoped aggregate; CorrelationID is the workflow ID.
func (p *WorkflowStatusProjection) handleHintEmitted(env event.Envelope) error {
	var h event.HintEmittedPayload
	if err := json.Unmarshal(env.Payload, &h); err != nil {
		return err
	}
	workflowID := env.CorrelationID
	if workflowID == "" {
		return nil
	}
	if p.hintPersonas[workflowID] == nil {
		p.hintPersonas[workflowID] = make(map[string]struct{})
	}
	p.hintPersonas[workflowID][h.Persona] = struct{}{}
	p.syncHintCount(workflowID)
	return nil
}

// handleHintApproved removes a persona from the pending-hints set on approval.
func (p *WorkflowStatusProjection) handleHintApproved(env event.Envelope) error {
	var h event.HintApprovedPayload
	if err := json.Unmarshal(env.Payload, &h); err != nil {
		return err
	}
	p.removeHintPersona(env, h.Persona)
	return nil
}

// handleHintRejected removes a persona from the pending-hints set on rejection.
func (p *WorkflowStatusProjection) handleHintRejected(env event.Envelope) error {
	var h event.HintRejectedPayload
	if err := json.Unmarshal(env.Payload, &h); err != nil {
		return err
	}
	p.removeHintPersona(env, h.Persona)
	return nil
}

// removeHintPersona deletes a persona entry from the pending-hint set and syncs the count.
func (p *WorkflowStatusProjection) removeHintPersona(env event.Envelope, persona string) {
	workflowID := env.CorrelationID
	if workflowID == "" {
		workflowID = env.AggregateID
	}
	if set := p.hintPersonas[workflowID]; set != nil {
		delete(set, persona)
	}
	p.syncHintCount(workflowID)
}

// syncHintCount updates the WorkflowStatus.PendingHintsCount from the hint personas set.
func (p *WorkflowStatusProjection) syncHintCount(workflowID string) {
	ws := p.getOrCreate(workflowID)
	ws.PendingHintsCount = len(p.hintPersonas[workflowID])
}

func (p *WorkflowStatusProjection) getOrCreate(aggregateID string) *WorkflowStatus {
	ws, ok := p.workflows[aggregateID]
	if !ok {
		ws = &WorkflowStatus{AggregateID: aggregateID}
		p.workflows[aggregateID] = ws
	}
	return ws
}

// Get returns the current status for a workflow.
func (p *WorkflowStatusProjection) Get(aggregateID string) (WorkflowStatus, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	ws, ok := p.workflows[aggregateID]
	if !ok {
		return WorkflowStatus{}, false
	}
	return *ws, true
}

// All returns all tracked workflow statuses.
func (p *WorkflowStatusProjection) All() []WorkflowStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]WorkflowStatus, 0, len(p.workflows))
	for _, ws := range p.workflows {
		result = append(result, *ws)
	}
	return result
}
