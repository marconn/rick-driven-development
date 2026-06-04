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
	// AIRequestStarted is emitted at the exact moment backend.Run is invoked
	// (subprocess exec). Paired with AIRequestSent (emitted BEFORE prompt
	// assembly and backend setup) it lets operators tell handler pre-flight
	// stalls apart from subprocess-side hangs: if AIRequestSent exists but
	// AIRequestStarted does not, the handler died between event persist and
	// backend.Run — prompt build, context load, system-prompt load, or
	// persistRequestEvent itself. If both exist but AIResponseReceived does
	// not, the subprocess was invoked and then stalled.
	AIRequestStarted   Type = "ai.request.started"
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
	// PersonaFailedTracked is the storage-only mirror of PersonaFailed on the
	// workflow aggregate. It gives operators a workflow-scoped breadcrumb of
	// which persona failed (and why) alongside PersonaTracked successes —
	// `rick events <workflow_agg>` previously showed a silent gap between the
	// last tracked completion and WorkflowRetried / WorkflowFailed. Never
	// published to the bus; carries the same payload shape as PersonaFailed.
	PersonaFailedTracked Type = "persona.failed.tracked"
	// VerdictTracked is the storage-only mirror of VerdictRendered on the
	// workflow aggregate. The original VerdictRendered lives on the
	// persona-scoped aggregate (<corr>:persona:<handler>) where the source
	// handler emitted it; that aggregate is never replayed when
	// loadAggregate(<workflow_agg>) runs, so the workflow aggregate's
	// LastVerdictFingerprint state can never be rebuilt from the
	// persona-scoped event stream. Mirroring as VerdictTracked on the
	// workflow aggregate gives Apply something to fold so the
	// identical-fingerprint dedup guard in decideVerdictRendered actually
	// fires on the second byte-identical failure (instead of staying
	// dead code, as it did before this mirror existed). Mirror is appended
	// AFTER Decide returns so the *current* verdict's fingerprint is not
	// visible to the *current* Decide call (which would falsely self-match).
	// Never published on the bus — diagnostic / state-rehydration only.
	VerdictTracked Type = "verdict.tracked"

	// Compensation
	CompensationStarted   Type = "compensation.started"
	CompensationCompleted Type = "compensation.completed"

	// Workspace
	WorkspaceReady Type = "workspace.ready"

	// Operator intervention
	WorkflowPaused  Type = "workflow.paused"
	WorkflowResumed Type = "workflow.resumed"
	// WorkflowRetried reboots a Failed/Cancelled workflow from a specific phase,
	// invalidating that phase plus its DAG-downstream while leaving upstream
	// PersonaCompleted state intact. Emitted by the MCP rick_retry_workflow tool
	// when from_phase is set.
	WorkflowRetried  Type = "workflow.retried"
	OperatorGuidance Type = "operator.guidance"

	// Hint lifecycle
	HintEmitted   Type = "hint.emitted"
	HintApproved  Type = "hint.approved"
	HintRejected  Type = "hint.rejected"

	// Workflow rerouting
	WorkflowRerouted Type = "workflow.rerouted"

	// Sentinel (unhandled event detection)
	UnhandledEventDetected Type = "sentinel.unhandled"

	// DispatchDropped records a PersonaRunner admission-gate drop: an event
	// that would have dispatched a handler but was filtered out by dedup,
	// join condition, context cancellation, or a store error. Written to a
	// dedicated diagnostic aggregate {correlationID}:drops so operators can
	// count/query drops per workflow without contending with the main
	// workflow write path. Never published on the bus — diagnostic only.
	DispatchDropped Type = "dispatch.dropped"

	// DispatchStarted records the moment PersonaRunner begins executing a
	// handler. AI handlers already emit AIRequestStarted at subprocess spawn,
	// but deterministic (non-AI) handlers — workspace, context-snapshot,
	// quality-gate, pr-* non-AI steps — had no "started" signal, so dwell /
	// execution-duration telemetry could not measure them. Written to the
	// dedicated diagnostic aggregate {correlationID}:dispatch, best-effort and
	// asynchronously so it never adds I/O latency to the dispatch hot path.
	// Never published on the bus — diagnostic only, no handler subscribers.
	DispatchStarted Type = "dispatch.started"

	// KnowledgeUnavailable records that a persona declared one or more OPTIONAL
	// knowledge packs but ran on a backend that cannot deliver them (no MCP tool
	// retrieval, and eager inlining is deferred). It is the signal that
	// quantifies how often the deferred eager-delivery policy actually matters —
	// the input that decides whether to build eager inlining / RAG. Required
	// packs never produce this: they pin to a capable backend or fail dispatch.
	// Emitted by the AI handler as part of its result; diagnostic only.
	KnowledgeUnavailable Type = "knowledge.unavailable"

	// VerdictGroundingSummary records how a pr-category-review handler's LLM
	// findings fared against the diff-grounding filter — how many were parsed,
	// how many survived, and why each rejection happened. Emitted once per
	// pr-category-review invocation by ReviewHandler. Stored alongside the
	// matching VerdictRendered event on the correlation aggregate. Never
	// published on the bus — forensics only, no handler subscribers.
	VerdictGroundingSummary Type = "verdict.grounding.summary"

	// Context snapshots (ground truth from codebase)
	ContextCodebase    Type = "context.codebase"
	ContextSchema      Type = "context.schema"
	ContextGit         Type = "context.git"
	ContextEnrichment  Type = "context.enrichment"

	// Child workflow lifecycle (injected by parent workflow plugins)
	ChildWorkflowCompleted Type = "child.workflow.completed"

	// Side-effect observability: emitted after a PR comment is posted to GitHub
	// by a Rick-owned handler. Lets operators trace comment posts in the event
	// stream and lets downstream handlers dedupe on body hash without re-calling
	// the REST API.
	PRCommentPosted Type = "pr.comment.posted"
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
