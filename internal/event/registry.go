package event

import (
	"encoding/json"
	"fmt"
	"maps"
	"sync"
)

// Upcaster transforms an event payload from one schema version to the next.
type Upcaster func(payload json.RawMessage) (json.RawMessage, error)

// Registration holds metadata about a registered event type.
type Registration struct {
	Type           Type
	Description    string
	CurrentVersion int
	Upcasters      map[int]Upcaster // version N → version N+1
}

// Registry tracks all known event types and their schema versions.
// It provides upcasting to migrate old events to current schemas.
type Registry struct {
	mu            sync.RWMutex
	registrations map[Type]*Registration
}

// NewRegistry creates an empty event type registry.
func NewRegistry() *Registry {
	return &Registry{
		registrations: make(map[Type]*Registration),
	}
}

// Register adds an event type to the registry.
func (r *Registry) Register(eventType Type, description string, currentVersion int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registrations[eventType] = &Registration{
		Type:           eventType,
		Description:    description,
		CurrentVersion: currentVersion,
		Upcasters:      make(map[int]Upcaster),
	}
}

// RegisterUpcaster adds a schema migration from version to version+1.
func (r *Registry) RegisterUpcaster(eventType Type, fromVersion int, upcaster Upcaster) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.registrations[eventType]
	if !ok {
		return fmt.Errorf("event type %q not registered", eventType)
	}
	reg.Upcasters[fromVersion] = upcaster
	return nil
}

// Upcast migrates a payload from its schema version to the current version.
// Returns the original payload if already current or no upcasters needed.
func (r *Registry) Upcast(eventType Type, schemaVersion int, payload json.RawMessage) (json.RawMessage, int, error) {
	r.mu.RLock()
	reg, ok := r.registrations[eventType]
	if !ok {
		r.mu.RUnlock()
		// Unknown types pass through without upcasting
		return payload, schemaVersion, nil
	}
	currentVersion := reg.CurrentVersion
	upcasters := make(map[int]Upcaster, len(reg.Upcasters))
	maps.Copy(upcasters, reg.Upcasters)
	r.mu.RUnlock()

	current := payload
	for v := schemaVersion; v < currentVersion; v++ {
		upcaster, ok := upcasters[v]
		if !ok {
			return nil, v, fmt.Errorf("no upcaster for %q from version %d to %d", eventType, v, v+1)
		}
		var err error
		current, err = upcaster(current)
		if err != nil {
			return nil, v, fmt.Errorf("upcast %q v%d→v%d: %w", eventType, v, v+1, err)
		}
	}
	return current, currentVersion, nil
}

// IsRegistered checks if an event type is known to the registry.
func (r *Registry) IsRegistered(eventType Type) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.registrations[eventType]
	return ok
}

// Types returns all registered event types.
func (r *Registry) Types() []Type {
	r.mu.RLock()
	defer r.mu.RUnlock()
	types := make([]Type, 0, len(r.registrations))
	for t := range r.registrations {
		types = append(types, t)
	}
	return types
}

// DefaultRegistry returns a registry pre-populated with all core event types.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(WorkflowRequested, "User requests a workflow run", 1)
	r.Register(WorkflowStarted, "Engine begins executing a workflow", 1)
	r.Register(WorkflowCompleted, "Workflow finishes successfully", 1)
	r.Register(WorkflowFailed, "Workflow fails", 1)
	r.Register(WorkflowCancelled, "Workflow is cancelled", 1)
	r.Register(AIRequestSent, "AI backend call is made", 1)
	r.Register(AIResponseReceived, "AI backend returns", 1)
	r.Register(AIStructuredOutput, "Structured output extracted from AI response", 1)
	r.Register(VerdictRendered, "Review/QA verdict is rendered", 1)
	r.Register(FeedbackGenerated, "Feedback is prepared for a retry", 1)
	r.Register(FeedbackConsumed, "Handler acknowledges feedback", 1)
	r.Register(TokenBudgetExceeded, "Cumulative token usage exceeds budget", 1)
	r.Register(CompensationStarted, "Rollback begins", 1)
	r.Register(CompensationCompleted, "Rollback finishes", 1)
	return r
}
