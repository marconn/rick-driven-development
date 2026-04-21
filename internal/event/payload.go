package event

import "encoding/json"

// MustMarshal marshals v to JSON or panics. Use only for payloads
// constructed from trusted, known-good data.
func MustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic("event: marshal payload: " + err.Error())
	}
	return data
}

// WorkflowRequestedPayload is emitted when a user requests a workflow run.
type WorkflowRequestedPayload struct {
	Prompt     string `json:"prompt"`
	WorkflowID string `json:"workflow_id"` // which DAG to run
	Source     string `json:"source"`      // "jira:PROJ-123", "gh:owner/repo#1", "raw"
	// Workspace params (optional — when set, workspace persona runs first)
	Repo       string `json:"repo,omitempty"`
	Ticket     string `json:"ticket,omitempty"`
	RepoBranch string `json:"repo_branch,omitempty"` // existing branch to check out (overrides ticket-as-branch)
	BaseBranch string `json:"base_branch,omitempty"`
	Isolate    bool   `json:"isolate,omitempty"`
}

// WorkspaceReadyPayload is emitted when the workspace persona has provisioned a git workspace.
type WorkspaceReadyPayload struct {
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	Base     string `json:"base"`
	Isolated bool   `json:"isolated"`
}

// WorkflowStartedPayload is emitted when the engine begins executing a workflow.
type WorkflowStartedPayload struct {
	WorkflowID string   `json:"workflow_id"`
	Phases     []string `json:"phases"` // ordered phase names from DAG
	Source     string   `json:"source,omitempty"`
	Ticket     string   `json:"ticket,omitempty"`
	Prompt     string   `json:"prompt,omitempty"`
}

// WorkflowCompletedPayload is emitted when a workflow finishes successfully.
type WorkflowCompletedPayload struct {
	Result string `json:"result"`
}

// WorkflowFailedPayload is emitted when a workflow fails.
type WorkflowFailedPayload struct {
	Reason string `json:"reason"`
	Phase  string `json:"phase,omitempty"` // which phase caused failure
}

// WorkflowCancelledPayload is emitted when an operator cancels a workflow.
type WorkflowCancelledPayload struct {
	Reason string `json:"reason"`
	Source string `json:"source,omitempty"` // "cli", "mcp", "auto"
}

// WorkflowPausedPayload is emitted when an operator pauses a workflow.
type WorkflowPausedPayload struct {
	Reason string `json:"reason"`
	Source string `json:"source,omitempty"` // "operator", "auto:max_iterations"
}

// WorkflowResumedPayload is emitted when an operator resumes a paused workflow.
type WorkflowResumedPayload struct {
	Reason string `json:"reason,omitempty"`
}

// WorkflowRetriedPayload reboots a Failed/Cancelled workflow at a specific
// phase. The aggregate clears CompletedPersonas for every persona in
// InvalidatedPersonas (computed by the emitter from the DAG's DownstreamOf),
// flips Status back to Running, and PersonaRunner re-dispatches FromPhase
// using the most recent predecessor completion as its trigger. The list is
// stored on the event — not recomputed from the live DAG — so replay stays
// deterministic even if the workflow definition changes later.
type WorkflowRetriedPayload struct {
	FromPhase           string   `json:"from_phase"`
	InvalidatedPersonas []string `json:"invalidated_personas"`
	Reason              string   `json:"reason,omitempty"`
	// Automatic=true marks retries emitted by the engine itself (e.g.,
	// auto-retry on transient idle_timeout). The aggregate counts these
	// separately so a single persona can't trigger more than one automatic
	// retry per workflow — operator-initiated retries via rick_retry_workflow
	// leave Automatic=false and do NOT count against the cap.
	Automatic bool `json:"automatic,omitempty"`
}

// OperatorGuidancePayload is emitted when an operator injects context into a workflow.
type OperatorGuidancePayload struct {
	Content    string `json:"content"`                // operator's text input
	Target     string `json:"target,omitempty"`       // target persona (optional)
	AutoResume bool   `json:"auto_resume,omitempty"`  // resume workflow after injection
}

// VerdictOutcome represents the result of a review/QA phase.
type VerdictOutcome string

const (
	VerdictPass    VerdictOutcome = "pass"
	VerdictFail    VerdictOutcome = "fail"
	VerdictUnknown VerdictOutcome = "unknown"
)

// Issue represents a typed, categorized issue found during review.
type Issue struct {
	Severity    string `json:"severity"`    // "critical", "major", "minor"
	Category    string `json:"category"`    // security, concurrency, error_handling, observability, api_contract, idempotency, testing, integration, performance, data, good_hygiene, correctness
	Description string `json:"description"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
}

// VerdictPayload replaces heuristic text parsing with structured verdicts.
type VerdictPayload struct {
	Phase       string         `json:"phase"`        // phase being evaluated (e.g., "develop")
	SourcePhase string         `json:"source_phase"` // phase that rendered the verdict (e.g., "review")
	Outcome     VerdictOutcome `json:"outcome"`
	Issues      []Issue        `json:"issues,omitempty"`
	Summary     string         `json:"summary"`
}

// FeedbackGeneratedPayload is emitted when feedback is prepared for a retry.
type FeedbackGeneratedPayload struct {
	TargetPhase string  `json:"target_phase"`          // phase to reschedule (e.g., "develop")
	SourcePhase string  `json:"source_phase,omitempty"` // phase that generated feedback (e.g., "review")
	Iteration   int     `json:"iteration"`
	Issues      []Issue `json:"issues"`
	Summary     string  `json:"summary"`
}

// FeedbackConsumedPayload is emitted when a handler acknowledges feedback.
type FeedbackConsumedPayload struct {
	Phase     string `json:"phase"`
	Iteration int    `json:"iteration"`
}

// AIRequestPayload is emitted when an AI backend call is made.
type AIRequestPayload struct {
	Phase      string `json:"phase"`
	Backend    string `json:"backend"` // "claude", "gemini"
	Persona    string `json:"persona"`
	PromptHash string `json:"prompt_hash"` // for dedup, not the full prompt
}

// AIRequestStartedPayload is emitted at the moment backend.Run is invoked —
// i.e., the subprocess is about to be exec'd. The gap between AIRequestSent
// and AIRequestStarted is handler pre-flight (prompt build, context load,
// request-event persist). The gap between AIRequestStarted and
// AIResponseReceived is the subprocess itself (stream read, extractor,
// watchdog). Operators diagnose stalls by counting which gap dominates.
type AIRequestStartedPayload struct {
	Phase         string `json:"phase"`
	Backend       string `json:"backend"`
	Persona       string `json:"persona"`
	PromptHash    string `json:"prompt_hash"`
	SpawnUnixNano int64  `json:"spawn_unix_nano"` // time.Now().UnixNano() at spawn call
}

// AIResponsePayload is emitted when an AI backend returns.
type AIResponsePayload struct {
	Phase      string          `json:"phase"`
	Backend    string          `json:"backend"`
	TokensUsed int             `json:"tokens_used,omitempty"`
	DurationMS int64           `json:"duration_ms"`
	Structured bool            `json:"structured"`              // was structured output extracted?
	Output     json.RawMessage `json:"output,omitempty"`        // raw LLM response for replay
	OutputRef  string          `json:"output_ref,omitempty"`    // blob storage ref for large outputs
}

// TokenBudgetExceededPayload is emitted when cumulative token usage exceeds the budget.
type TokenBudgetExceededPayload struct {
	TotalUsed int `json:"total_used"`
	Budget    int `json:"budget"`
	Phase     string `json:"phase"` // phase that triggered the breach
}

// PersonaCompletedPayload is emitted when a persona handler finishes successfully.
type PersonaCompletedPayload struct {
	Persona      string `json:"persona"`              // handler name: "developer", "documenter"
	Phase        string `json:"phase,omitempty"`       // DAG phase, empty for reactive
	TriggerEvent string `json:"trigger_event"`         // event type that triggered this
	TriggerID    string `json:"trigger_id"`            // ID of triggering event
	Reactive     bool   `json:"reactive"`              // true=bus-triggered, false=DAG-triggered
	OutputRef    string `json:"output_ref,omitempty"`  // event ID of AIResponseReceived (avoids duplicating large payloads)
	DurationMS   int64  `json:"duration_ms"`
	ChainDepth   int    `json:"chain_depth"`           // reactive chain depth (storm protection)
}

// FailureKind classifies a persona handler failure so operators can tell a
// transient-shaped failure (wedged subprocess killed by the idle watchdog)
// apart from a deterministic one (handler code error, permanent backend
// error). Emitted on PersonaFailedPayload; safe default is
// FailureKindUnspecified for back-compat with events written before this
// field existed.
type FailureKind string

const (
	// FailureKindUnspecified is the zero value — used for events written
	// before FailureKind was added, or when no classification is available.
	FailureKindUnspecified FailureKind = ""
	// FailureKindIdleTimeout means the backend subprocess was silent for
	// longer than RICK_BACKEND_STALL_TIMEOUT and was killed by the idle
	// watchdog. Transient — a retry has a real chance of succeeding.
	FailureKindIdleTimeout FailureKind = "idle_timeout"
	// FailureKindWallTimeout means the backend ran past its wall-clock
	// budget (RICK_BACKEND_TIMEOUT / RICK_REVIEW_BACKEND_TIMEOUT) and the
	// context deadline fired. Transient-ish — prompt may genuinely need
	// more time.
	FailureKindWallTimeout FailureKind = "wall_timeout"
	// FailureKindCancelled means the per-correlation context was cancelled
	// before backend.Run returned (operator cancel, workflow shutdown).
	FailureKindCancelled FailureKind = "cancelled"
	// FailureKindBackendError means backend.Run returned a non-timeout
	// error — subprocess crashed, invalid args, auth failure, etc. See
	// the Error and Stderr fields for specifics.
	FailureKindBackendError FailureKind = "backend_error"
	// FailureKindHandlerError means the handler itself returned an error
	// before or after the backend call — prompt build failure, context
	// load failure, etc. Deterministic; retry rarely helps.
	FailureKindHandlerError FailureKind = "handler_error"
)

// PersonaFailedPayload is emitted when a persona handler fails.
//
// FailureKind classifies the failure so operators and auto-retry policies
// can distinguish transient shapes (idle/wall timeout) from deterministic
// ones (handler error). Stderr captures the last bytes of subprocess output
// when available (backend-driven failures); it is bounded and may be
// truncated. Both fields are optional for backward compatibility.
//
// HandlerVersion carries the VCS revision of the binary that emitted this
// failure — populated from internal/buildinfo.Version(). It is omitted from
// PersonaCompletedPayload because fleet drift questions only arise on failures.
type PersonaFailedPayload struct {
	Persona        string      `json:"persona"`
	Phase          string      `json:"phase,omitempty"`
	TriggerEvent   string      `json:"trigger_event"`
	TriggerID      string      `json:"trigger_id"`
	Reactive       bool        `json:"reactive"`
	Error          string      `json:"error"`
	FailureKind    FailureKind `json:"failure_kind,omitempty"`
	Stderr         string      `json:"stderr,omitempty"`
	DurationMS     int64       `json:"duration_ms"`
	ChainDepth     int         `json:"chain_depth"`
	HandlerVersion string      `json:"handler_version,omitempty"`
}

// CompensationPayload is emitted during rollback.
type CompensationPayload struct {
	Phase  string `json:"phase"`
	Action string `json:"action"` // what compensation was performed
}

// --- Hint payloads ---

// HintEmittedPayload is emitted when a persona runs a lightweight pre-check
// before full execution. The Engine auto-approves or pauses based on confidence.
type HintEmittedPayload struct {
	Persona       string          `json:"persona"`
	Phase         string          `json:"phase"`
	TriggerEvent  string          `json:"trigger_event"`          // original event type
	TriggerID     string          `json:"trigger_id"`             // original event ID for replay
	Confidence    float64         `json:"confidence"`             // 0.0-1.0
	Plan          string          `json:"plan"`                   // what the persona intends to do
	Blockers      []string        `json:"blockers,omitempty"`     // issues that may prevent success
	TokenEstimate int             `json:"token_estimate"`         // estimated token usage
	Metadata      json.RawMessage `json:"metadata,omitempty"`     // handler-specific data
}

// HintApprovedPayload is emitted when a hint is accepted (auto or operator).
// Triggers full execution for the hinted persona.
type HintApprovedPayload struct {
	Persona   string `json:"persona"`
	TriggerID string `json:"trigger_id"` // original event to replay
	Guidance  string `json:"guidance"`   // optional operator guidance injected into context
}

// HintRejectedPayload is emitted when a hint is rejected.
type HintRejectedPayload struct {
	Persona string `json:"persona"`
	Reason  string `json:"reason"`
	Action  string `json:"action"` // "skip" = mark persona complete, "fail" = fail workflow
}

// WorkflowReroutedPayload is emitted when the workflow's Required list is modified.
type WorkflowReroutedPayload struct {
	Persona        string   `json:"persona"`                   // hint source that triggered reroute
	AddRequired    []string `json:"add_required,omitempty"`    // personas to add
	RemoveRequired []string `json:"remove_required,omitempty"` // personas to skip
	Reason         string   `json:"reason"`
}

// --- Sentinel payloads ---

// UnhandledEventPayload is emitted when the sentinel detects an event that no
// handler is subscribed to process.
type UnhandledEventPayload struct {
	EventType     string `json:"event_type"`
	EventID       string `json:"event_id"`
	CorrelationID string `json:"correlation_id"`
	Source        string `json:"source"`
}

// DispatchDroppedPayload records a PersonaRunner admission-gate drop. Written
// to the dedicated diagnostic aggregate {correlationID}:drops so operators
// can post-hoc analyze why a dispatch didn't happen without having to replay
// the full event chain or grep log output. Fields:
//
//   - Handler: target handler name that was filtered out
//   - DroppedEventID: the triggering event that would have dispatched
//   - DroppedEventType: that event's type (e.g., "persona.completed")
//   - DropReason: one of the engine's drop_reason constants (event_dedup,
//     join_unsatisfied, join_gate_dedup, ctx_cancelled, store_error)
//   - MissingPredecessors: for join_unsatisfied, the personas absent from
//     latestByPersona at check time. Empty for other reasons.
//   - Fingerprint: for join_gate_dedup, the sorted|joined predecessor event
//     IDs that matched a previously-dispatched join. Empty for other reasons.
//   - Detail: free-form text for store_error (wrapped error) or other reasons.
type DispatchDroppedPayload struct {
	Handler             string   `json:"handler"`
	DroppedEventID      string   `json:"dropped_event_id"`
	DroppedEventType    string   `json:"dropped_event_type"`
	DropReason          string   `json:"drop_reason"`
	MissingPredecessors []string `json:"missing_predecessors,omitempty"`
	Fingerprint         string   `json:"fingerprint,omitempty"`
	Detail              string   `json:"detail,omitempty"`
}

// --- Child workflow payloads ---

// ChildWorkflowCompletedPayload is injected by external systems when a child
// workflow reaches a terminal state. Enables parent workflows to re-trigger
// handlers that coordinate multi-workflow execution.
type ChildWorkflowCompletedPayload struct {
	ParentCorrelation string `json:"parent_correlation"`
	ChildCorrelation  string `json:"child_correlation"`
	ChildTicket       string `json:"child_ticket,omitempty"`
	Status            string `json:"status"`                 // "completed", "failed", "cancelled"
	Result            string `json:"result,omitempty"`
	FailedPhase       string `json:"failed_phase,omitempty"`
	DurationMS        int64  `json:"duration_ms,omitempty"`
}

// --- Context snapshot payloads (ground truth from codebase) ---

// FileEntry is a single entry in the codebase file tree.
type FileEntry struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Language string `json:"lang,omitempty"`
}

// FileSnap is a file's content captured at snapshot time.
type FileSnap struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// ContextCodebasePayload captures the codebase structure and key file contents.
type ContextCodebasePayload struct {
	Tree      []FileEntry `json:"tree"`
	Files     []FileSnap  `json:"files"`
	Framework string      `json:"framework,omitempty"` // "go-grpc", "vue-webpack", etc.
	Language  string      `json:"language,omitempty"`  // "go", "typescript", etc.
}

// ContextSchemaPayload captures schema definitions (proto, SQL, GraphQL).
type ContextSchemaPayload struct {
	Proto   []FileSnap `json:"proto,omitempty"`
	SQL     []FileSnap `json:"sql,omitempty"`
	GraphQL []FileSnap `json:"graphql,omitempty"`
}

// ContextGitPayload captures git state at snapshot time.
type ContextGitPayload struct {
	HEAD          string   `json:"head"`
	Branch        string   `json:"branch"`
	RecentLog     []string `json:"recent_log"`
	DiffStat      string   `json:"diff_stat,omitempty"`
	ModifiedFiles []string `json:"modified_files,omitempty"`
}

// ContextEnrichmentPayload carries supplementary context injected by
// before-hook systems (e.g., library suggestions, component catalogs).
// Downstream personas read this from the correlation chain.
type ContextEnrichmentPayload struct {
	Source  string              `json:"source"`            // enricher identity: "frontend-enricher"
	Kind    string              `json:"kind"`              // "libraries", "components", "patterns"
	Items   []EnrichmentItem    `json:"items"`
	Summary string              `json:"summary,omitempty"` // human-readable summary
}

// EnrichmentItem is a single suggestion from an enrichment system.
type EnrichmentItem struct {
	Name        string `json:"name"`                  // "shadcn/ui", "tanstack-query"
	Version     string `json:"version,omitempty"`     // "^4.0.0"
	Reason      string `json:"reason"`                // why this is recommended
	DocURL      string `json:"doc_url,omitempty"`     // reference link
	ImportPath  string `json:"import_path,omitempty"` // "@tanstack/react-query"
}

// PRCommentPostedPayload records a PR comment Rick posted via its own client.
// BodyHash lets downstream handlers dedupe without re-fetching the comment list
// from GitHub. Kind distinguishes reply (immediate, addresses reviewer feedback)
// from summary (workflow completion overview) so projections and tests can
// filter by intent. Skipped is true when the poster short-circuited because an
// identical body already existed on the PR — observability only, not an error.
type PRCommentPostedPayload struct {
	Repo      string `json:"repo"`                 // "owner/repo"
	PRNumber  int    `json:"pr_number"`
	Kind      string `json:"kind"`                 // "reply", "summary"
	CommentID int    `json:"comment_id,omitempty"` // GitHub comment ID (0 when skipped)
	BodyHash  string `json:"body_hash"`            // sha256 hex of the posted body
	Skipped   bool   `json:"skipped,omitempty"`    // true when an identical comment already existed
}
