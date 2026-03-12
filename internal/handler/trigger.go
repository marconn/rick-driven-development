package handler

import "github.com/marconn/rick-event-driven-development/internal/event"

// Trigger declares when a persona handler should fire.
// Deprecated: With DAG-based dispatch, execution topology is defined in
// WorkflowDef.Graph instead of on individual handlers. Trigger is retained
// only for gRPC proxy handlers that register via the bidirectional stream —
// PersonaRunner falls back to Trigger() for handlers not in any workflow Graph.
type Trigger struct {
	Events        []event.Type // event types to subscribe to
	AfterPersonas []string     // required completed personas (join condition)
}

// TriggeredHandler extends Handler with a declarative Trigger.
// Deprecated: Used only by gRPC proxy handlers for backward compatibility.
// Local handlers no longer implement this — their subscriptions are computed
// from workflow Graph definitions by PersonaRunner.resolveEventsFromDAG().
type TriggeredHandler interface {
	Handler
	Trigger() Trigger
}
