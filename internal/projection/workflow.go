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
}

// NewWorkflowStatusProjection creates a new workflow status projection.
func NewWorkflowStatusProjection() *WorkflowStatusProjection {
	return &WorkflowStatusProjection{
		workflows: make(map[string]*WorkflowStatus),
	}
}

func (p *WorkflowStatusProjection) Name() string { return "workflow-status" }

func (p *WorkflowStatusProjection) Handle(_ context.Context, env event.Envelope) error {
	// WorkflowStarted is now a family: workflow.started.<id>
	isStarted := event.IsWorkflowStarted(env.Type)
	switch {
	case env.Type == event.WorkflowRequested || isStarted ||
		env.Type == event.WorkflowCompleted || env.Type == event.WorkflowFailed ||
		env.Type == event.WorkflowCancelled ||
		env.Type == event.WorkflowPaused || env.Type == event.WorkflowResumed:
		// handled below
	default:
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

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
