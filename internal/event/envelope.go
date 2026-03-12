package event

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ID is a unique event identifier.
type ID string

// NewID generates a new unique event ID.
func NewID() ID {
	return ID(uuid.New().String())
}

// Type represents an event type string like "workflow.started" or "phase.completed".
type Type string

// Core event types used throughout the system.
const (
	// Workflow lifecycle
	WorkflowRequested Type = "workflow.requested"
	// WorkflowStarted is the base type for the workflow-started family.
	// The engine emits WorkflowStartedFor(workflowID) — never this generic
	// constant. Do NOT subscribe handlers to this; use WorkflowStartedFor().
	// This constant exists for IsWorkflowStarted() checks and sentinel/projection internals.
	WorkflowStarted   Type = "workflow.started"
	WorkflowCompleted Type = "workflow.completed"
	WorkflowFailed    Type = "workflow.failed"
	WorkflowCancelled Type = "workflow.cancelled"

	// AI operations
	AIRequestSent      Type = "ai.request.sent"
	AIResponseReceived Type = "ai.response.received"
	AIStructuredOutput Type = "ai.structured_output"

	// Feedback cycle
	VerdictRendered   Type = "verdict.rendered"
	FeedbackGenerated Type = "feedback.generated"
	FeedbackConsumed  Type = "feedback.consumed"

	// Budget
	TokenBudgetExceeded Type = "token.budget.exceeded"

	// Persona lifecycle
	PersonaCompleted Type = "persona.completed"
	PersonaFailed    Type = "persona.failed"
	// PersonaTracked is an internal engine event stored on the workflow aggregate
	// to record that a persona has completed, so CompletedPersonas survives
	// aggregate replay without re-querying persona-scoped aggregates.
	// Never published to the bus — use PersonaCompleted for bus dispatch.
	PersonaTracked Type = "persona.tracked"

	// Compensation
	CompensationStarted   Type = "compensation.started"
	CompensationCompleted Type = "compensation.completed"

	// Workspace
	WorkspaceReady Type = "workspace.ready"

	// Operator intervention
	WorkflowPaused  Type = "workflow.paused"
	WorkflowResumed Type = "workflow.resumed"
	OperatorGuidance Type = "operator.guidance"

	// Hint lifecycle
	HintEmitted   Type = "hint.emitted"
	HintApproved  Type = "hint.approved"
	HintRejected  Type = "hint.rejected"

	// Workflow rerouting
	WorkflowRerouted Type = "workflow.rerouted"

	// Sentinel (unhandled event detection)
	UnhandledEventDetected Type = "sentinel.unhandled"

	// Context snapshots (ground truth from codebase)
	ContextCodebase    Type = "context.codebase"
	ContextSchema      Type = "context.schema"
	ContextGit         Type = "context.git"
	ContextEnrichment  Type = "context.enrichment"

	// Child workflow lifecycle (injected by parent workflow plugins)
	ChildWorkflowCompleted Type = "child.workflow.completed"
)

// WorkflowStartedPrefix is the prefix for workflow-scoped start events.
// Entry-point handlers subscribe to workflow-specific types (e.g.,
// "workflow.started.default") so they only fire on their target workflow.
const WorkflowStartedPrefix = "workflow.started."

// WorkflowStartedFor returns a workflow-scoped start event type.
// E.g., WorkflowStartedFor("workspace-dev") → "workflow.started.workspace-dev".
func WorkflowStartedFor(workflowID string) Type {
	return Type(WorkflowStartedPrefix + workflowID)
}

// IsWorkflowStarted returns true for both the generic WorkflowStarted
// and any workflow-scoped variant (workflow.started.<id>).
func IsWorkflowStarted(t Type) bool {
	return t == WorkflowStarted || strings.HasPrefix(string(t), WorkflowStartedPrefix)
}

// Envelope is the core event wrapper. Every state change in the system
// is represented as an immutable Envelope stored in the event store.
type Envelope struct {
	ID            ID              `json:"id"`
	Type          Type            `json:"type"`
	AggregateID   string          `json:"aggregate_id"`
	Version       int             `json:"version"`
	SchemaVersion int             `json:"schema_version"`
	Timestamp     time.Time       `json:"timestamp"`
	CausationID   ID              `json:"causation_id,omitempty"`
	CorrelationID string          `json:"correlation_id"`
	Source        string          `json:"source"`
	Payload       json.RawMessage `json:"payload"`
}

// New creates a new Envelope with a generated ID and current timestamp.
// schemaVersion specifies the payload schema version for upcasting support.
// The caller must set AggregateID, Version, CorrelationID, and Source.
func New(eventType Type, schemaVersion int, payload json.RawMessage) Envelope {
	return Envelope{
		ID:            NewID(),
		Type:          eventType,
		SchemaVersion: schemaVersion,
		Timestamp:     time.Now(),
		Payload:       payload,
	}
}

// WithAggregate sets the aggregate ID and version.
func (e Envelope) WithAggregate(aggregateID string, version int) Envelope {
	e.AggregateID = aggregateID
	e.Version = version
	return e
}

// WithCausation sets the causation ID (the event that caused this one).
func (e Envelope) WithCausation(causationID ID) Envelope {
	e.CausationID = causationID
	return e
}

// WithCorrelation sets the correlation ID (ties entire workflow run together).
func (e Envelope) WithCorrelation(correlationID string) Envelope {
	e.CorrelationID = correlationID
	return e
}

// WithSource sets the source (e.g., "handler:reviewer", "engine:scheduler").
func (e Envelope) WithSource(source string) Envelope {
	e.Source = source
	return e
}
