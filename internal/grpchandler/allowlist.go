package grpchandler

import "github.com/marconn/rick-event-driven-development/internal/event"

// injectableTypes lists event types that external systems may inject via gRPC.
// Everything else is system-only and must be produced by internal components.
var injectableTypes = map[event.Type]bool{
	// Workflow lifecycle (operator-initiated)
	event.WorkflowRequested: true,
	event.WorkflowCancelled: true,
	event.WorkflowPaused:    true,
	event.WorkflowResumed:   true,

	// Operator intervention
	event.OperatorGuidance: true,

	// Context injection
	event.ContextEnrichment: true,
	event.ContextCodebase:   true,
	event.ContextSchema:     true,
	event.ContextGit:        true,

	// Child workflow coordination
	event.ChildWorkflowCompleted: true,
}

// IsInjectable returns true if the event type may be injected by external
// systems. System-only types (PersonaCompleted, WorkflowStarted, etc.) are
// blocked to preserve internal invariants.
func IsInjectable(t event.Type) bool {
	return injectableTypes[t]
}
