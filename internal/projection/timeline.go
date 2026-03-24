package projection

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// personaKey uniquely identifies a persona within a workflow.
type personaKey struct {
	CorrelationID string
	Persona       string
}

// PhaseTimelineProjection tracks duration and iteration count per persona.
type PhaseTimelineProjection struct {
	mu        sync.RWMutex
	timelines map[personaKey]*PhaseTimeline
}

// NewPhaseTimelineProjection creates a new phase timeline projection.
func NewPhaseTimelineProjection() *PhaseTimelineProjection {
	return &PhaseTimelineProjection{
		timelines: make(map[personaKey]*PhaseTimeline),
	}
}

func (p *PhaseTimelineProjection) Name() string { return "phase-timeline" }

func (p *PhaseTimelineProjection) Handle(_ context.Context, env event.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch env.Type {
	case event.AIRequestSent:
		// Track that a handler started running (AI backend call in progress).
		// Extract handler name from aggregate: "{corrID}:persona:{handler}".
		corrID := env.CorrelationID
		if corrID == "" {
			corrID = env.AggregateID
		}
		handlerName := personaFromAggregate(env.AggregateID)
		if handlerName == "" {
			return nil
		}
		key := personaKey{CorrelationID: corrID, Persona: handlerName}
		pt := p.getOrCreate(key, corrID)
		if pt.Status == "" {
			pt.Status = "running"
			pt.StartedAt = env.Timestamp
		}

	case event.PersonaCompleted:
		var payload event.PersonaCompletedPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return err
		}
		corrID := env.CorrelationID
		if corrID == "" {
			corrID = env.AggregateID
		}
		key := personaKey{CorrelationID: corrID, Persona: payload.Persona}
		pt := p.getOrCreate(key, corrID)
		pt.Iterations++
		pt.Status = "done"
		pt.CompletedAt = env.Timestamp
		pt.Duration = time.Duration(payload.DurationMS) * time.Millisecond
		if pt.StartedAt.IsZero() {
			// Approximate start from completion time minus duration
			pt.StartedAt = env.Timestamp.Add(-pt.Duration)
		}

	case event.PersonaFailed:
		var payload event.PersonaFailedPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return err
		}
		corrID := env.CorrelationID
		if corrID == "" {
			corrID = env.AggregateID
		}
		key := personaKey{CorrelationID: corrID, Persona: payload.Persona}
		pt := p.getOrCreate(key, corrID)
		pt.Status = "failed"
		pt.CompletedAt = env.Timestamp
		pt.Duration = time.Duration(payload.DurationMS) * time.Millisecond
		if pt.StartedAt.IsZero() {
			pt.StartedAt = env.Timestamp.Add(-pt.Duration)
		}
	}
	return nil
}

func (p *PhaseTimelineProjection) getOrCreate(key personaKey, corrID string) *PhaseTimeline {
	pt, ok := p.timelines[key]
	if !ok {
		pt = &PhaseTimeline{
			AggregateID: corrID,
			Phase:       key.Persona,
		}
		p.timelines[key] = pt
	}
	return pt
}

// Get returns the timeline for a specific persona in a workflow.
func (p *PhaseTimelineProjection) Get(aggregateID, phase string) (PhaseTimeline, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pt, ok := p.timelines[personaKey{CorrelationID: aggregateID, Persona: phase}]
	if !ok {
		return PhaseTimeline{}, false
	}
	return *pt, true
}

// ForWorkflow returns all persona timelines for a given workflow.
func (p *PhaseTimelineProjection) ForWorkflow(aggregateID string) []PhaseTimeline {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var result []PhaseTimeline
	for key, pt := range p.timelines {
		if key.CorrelationID == aggregateID {
			result = append(result, *pt)
		}
	}
	return result
}

// personaFromAggregate extracts the handler name from a persona-scoped
// aggregate ID with format "{correlationID}:persona:{handlerName}".
func personaFromAggregate(aggregateID string) string {
	const sep = ":persona:"
	idx := strings.LastIndex(aggregateID, sep)
	if idx < 0 {
		return ""
	}
	return aggregateID[idx+len(sep):]
}
