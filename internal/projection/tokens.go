package projection

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"sync"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// TokenUsageProjection tracks token consumption per workflow, phase, and backend.
type TokenUsageProjection struct {
	mu    sync.RWMutex
	usage map[string]*TokenUsage
}

// NewTokenUsageProjection creates a new token usage projection.
func NewTokenUsageProjection() *TokenUsageProjection {
	return &TokenUsageProjection{
		usage: make(map[string]*TokenUsage),
	}
}

func (p *TokenUsageProjection) Name() string { return "token-usage" }

func (p *TokenUsageProjection) Handle(_ context.Context, env event.Envelope) error {
	if env.Type != event.AIResponseReceived {
		return nil
	}

	var payload event.AIResponsePayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}
	if payload.TokensUsed == 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	tu := p.getOrCreate(env.AggregateID)
	tu.Total += payload.TokensUsed

	if payload.Persona != "" {
		tu.ByPersona[payload.Persona] += payload.TokensUsed
	}
	if payload.Backend != "" {
		tu.ByBackend[payload.Backend] += payload.TokensUsed
	}
	return nil
}

func (p *TokenUsageProjection) getOrCreate(aggregateID string) *TokenUsage {
	tu, ok := p.usage[aggregateID]
	if !ok {
		tu = &TokenUsage{
			AggregateID: aggregateID,
			ByPersona:     make(map[string]int),
			ByBackend:   make(map[string]int),
		}
		p.usage[aggregateID] = tu
	}
	return tu
}

// Get returns the token usage for a workflow.
func (p *TokenUsageProjection) Get(aggregateID string) (TokenUsage, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	tu, ok := p.usage[aggregateID]
	if !ok {
		return TokenUsage{}, false
	}
	// Return a copy with cloned maps
	result := *tu
	result.ByPersona = make(map[string]int, len(tu.ByPersona))
	maps.Copy(result.ByPersona, tu.ByPersona)
	result.ByBackend = make(map[string]int, len(tu.ByBackend))
	maps.Copy(result.ByBackend, tu.ByBackend)
	return result, true
}

// ForWorkflow aggregates token usage across all persona-scoped aggregates
// belonging to a workflow. Persona aggregates use the naming convention
// "{correlationID}:persona:{name}".
func (p *TokenUsageProjection) ForWorkflow(correlationID string) (TokenUsage, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	prefix := correlationID + ":"
	result := TokenUsage{
		AggregateID: correlationID,
		ByPersona:     make(map[string]int),
		ByBackend:   make(map[string]int),
	}
	found := false
	for aggID, tu := range p.usage {
		if aggID == correlationID || strings.HasPrefix(aggID, prefix) {
			found = true
			result.Total += tu.Total
			for k, v := range tu.ByPersona {
				result.ByPersona[k] += v
			}
			for k, v := range tu.ByBackend {
				result.ByBackend[k] += v
			}
		}
	}
	return result, found
}
