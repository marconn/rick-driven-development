package projection

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// VerdictProjection accumulates review verdicts per workflow correlation.
type VerdictProjection struct {
	mu       sync.RWMutex
	verdicts map[string][]VerdictRecord // correlationID → verdicts
}

// NewVerdictProjection creates a new verdict projection.
func NewVerdictProjection() *VerdictProjection {
	return &VerdictProjection{
		verdicts: make(map[string][]VerdictRecord),
	}
}

func (p *VerdictProjection) Name() string { return "verdict" }

func (p *VerdictProjection) Handle(_ context.Context, env event.Envelope) error {
	if env.Type != event.VerdictRendered {
		return nil
	}

	var payload event.VerdictPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}

	issues := make([]VerdictIssue, len(payload.Issues))
	for i, iss := range payload.Issues {
		issues[i] = VerdictIssue{
			Severity:    iss.Severity,
			Category:    iss.Category,
			Description: iss.Description,
			File:        iss.File,
			Line:        iss.Line,
		}
	}

	record := VerdictRecord{
		Phase:       payload.Phase,
		SourcePhase: payload.SourcePhase,
		Outcome:     string(payload.Outcome),
		Summary:     payload.Summary,
		Issues:      issues,
	}

	p.mu.Lock()
	p.verdicts[env.CorrelationID] = append(p.verdicts[env.CorrelationID], record)
	p.mu.Unlock()

	return nil
}

// ForWorkflow returns all verdict records for a workflow. Returns a deep copy.
func (p *VerdictProjection) ForWorkflow(correlationID string) []VerdictRecord {
	p.mu.RLock()
	defer p.mu.RUnlock()

	src := p.verdicts[correlationID]
	if len(src) == 0 {
		return nil
	}

	result := make([]VerdictRecord, len(src))
	for i, v := range src {
		result[i] = VerdictRecord{
			Phase:       v.Phase,
			SourcePhase: v.SourcePhase,
			Outcome:     v.Outcome,
			Summary:     v.Summary,
		}
		if len(v.Issues) > 0 {
			result[i].Issues = make([]VerdictIssue, len(v.Issues))
			copy(result[i].Issues, v.Issues)
		}
	}
	return result
}
