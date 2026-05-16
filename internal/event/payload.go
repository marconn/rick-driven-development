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

// legacyPersonaFromPhase translates a legacy phase verb to the corresponding
// handler name for back-compat with events written before the phase-verb /
// handler-name collapse (2026-05-05). Reads only — writers always emit handler
// names.
//
// The map covers every built-in phase verb that differed from its handler
// name. Phase verbs that already matched the handler name (architect, qa) and
// shared verbs that fan out to multiple handlers (pr-category-review) pass
// through unchanged — old events of the latter shape didn't carry handler-
// name information at the AI-payload level anyway, so there's nothing to
// recover.
func legacyPersonaFromPhase(phase string) string {
	switch phase {
	case "develop":
		return "developer"
	case "research":
		return "researcher"
	case "commit":
		return "committer"
	case "review":
		return "reviewer"
	case "feedback-analyze":
		return "feedback-analyzer"
	case "pr-reply":
		return "pr-replier"
	case "qa-analyze":
		return "qa-analyzer"
	}
	return phase
}

// pickPersona returns persona if non-empty, otherwise translates the legacy
// phase verb. Used by tolerant UnmarshalJSON implementations to migrate old
// stored events on read.
func pickPersona(persona, legacyPhase string) string {
	if persona != "" {
		return persona
	}
	return legacyPersonaFromPhase(legacyPhase)
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
//
// FailureKind, Backend, and Stderr are populated when the failure originated
// from a PersonaFailed event. They let `rick_workflow_status` return an
// actionable signal (idle_timeout / wall_timeout / handler_error / ...) and
// the subprocess stderr tail without forcing operators to replay the raw
// event chain. All three fields are optional for back-compat with events
// written before they existed (aggregate-level failures like
// TokenBudgetExceeded or hint rejections leave them empty).
type WorkflowFailedPayload struct {
	Reason      string      `json:"reason"`
	Persona     string      `json:"persona,omitempty"` // which handler caused failure
	FailureKind FailureKind `json:"failure_kind,omitempty"`
	Backend     string      `json:"backend,omitempty"`
	Stderr      string      `json:"stderr,omitempty"`
}

// UnmarshalJSON tolerates the legacy `phase` field with verb values.
func (p *WorkflowFailedPayload) UnmarshalJSON(data []byte) error {
	type alias WorkflowFailedPayload
	var raw struct {
		alias
		Phase string `json:"phase,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = WorkflowFailedPayload(raw.alias)
	if p.Persona == "" && raw.Phase != "" {
		p.Persona = legacyPersonaFromPhase(raw.Phase)
	}
	return nil
}

// WorkflowCancelledPayload is emitted when an operator cancels a workflow.
type WorkflowCancelledPayload struct {
	Reason string `json:"reason"`
	Source string `json:"source,omitempty"` // "cli", "mcp", "auto"
}

// WorkflowPausedPayload is emitted when an operator pauses a workflow.
//
// RetargetPersona / RetargetSource are an optional re-trigger directive for
// the resume path: when a pause was caused by an escalation that intends the
// operator to fix something and re-run a persona (byte-identical fingerprint,
// max-iter exhausted with EscalateOnMaxIter), the emitter sets these fields
// so decideWorkflowResumed can re-emit FeedbackGenerated for the named
// persona on resume. Empty means "resume just unpauses" — used by advisory
// verdicts (the operator's resume signals manual validation, not a retry)
// and hint pauses (which advance via HintApproved/HintRejected, not via
// WorkflowResumed).
type WorkflowPausedPayload struct {
	Reason          string `json:"reason"`
	Source          string `json:"source,omitempty"` // "operator", "auto:max_iterations", "auto:rate_limited"
	RetargetPersona string `json:"retarget_persona,omitempty"`
	RetargetSource  string `json:"retarget_source,omitempty"`
	// RetryFromPhase is the rate-limit-pause analogue of RetargetPersona. When
	// set, decideWorkflowResumed emits WorkflowRetried{from_phase=RetryFromPhase}
	// on resume rather than FeedbackGenerated — the rate-limited persona and
	// its barrier siblings need a clean re-dispatch through the retry
	// machinery, not a developer-loop feedback round. Mutually exclusive with
	// RetargetPersona at emit time; resume handles RetryFromPhase first when
	// both happen to be set.
	RetryFromPhase string `json:"retry_from_phase,omitempty"`
	// RateLimitResetHint is a best-effort, human-readable parse of the reset
	// time the provider surfaced in stderr (e.g. "4:50pm (America/Costa_Rica)"
	// from claude). Empty when the stderr line did not carry a reset hint.
	// Surfaced verbatim in rick_workflow_status so operators know when to
	// resume; the engine does not parse this further.
	RateLimitResetHint string `json:"rate_limit_reset_hint,omitempty"`
	// RateLimitBackend names the backend driver that hit the limit
	// ("claude", "gemini", "codex"). Set alongside RetryFromPhase for
	// rate-limit pauses; empty otherwise.
	RateLimitBackend string `json:"rate_limit_backend,omitempty"`
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
//
// Advisory=true signals that the verdict source does not trust its own
// failure signal (e.g., quality-gate's local VM failed but GitHub CI is
// green on the same SHA — almost always a local-env flake). The aggregate
// converts advisory fails into WorkflowPaused instead of FeedbackGenerated
// so the loop escalates to an operator rather than burning developer
// iterations on a non-regression. Advisory is ignored on pass/unknown
// outcomes.
type VerdictPayload struct {
	Persona       string         `json:"persona"`                  // target handler whose work is being evaluated (e.g., "developer")
	SourcePersona string         `json:"source_persona,omitempty"` // handler that rendered the verdict (e.g., "reviewer", "qa")
	Outcome       VerdictOutcome `json:"outcome"`
	Issues        []Issue        `json:"issues,omitempty"`
	Summary       string         `json:"summary"`
	Advisory      bool           `json:"advisory,omitempty"`
	// Source classifies HOW this verdict was produced (parser path), so
	// operators can distinguish a real PASS from a defaulted PASS or a PASS
	// demoted from FAIL. Forensics-only — engine ignores it. Empty zero value
	// preserves back-compat for events written before this field existed.
	Source VerdictSource `json:"verdict_source,omitempty"`
	// RawDiagnostics carries the unfiltered tail of the captured output that
	// produced this verdict — typically the last ~64 lines of stack/test
	// stdout+stderr before any human-readable filtering. Populated by
	// quality-gate so the developer's feedback prompt can act on the raw
	// diagnostics even when the human-readable Issue.Description has been
	// trimmed of Docker/multipass lifecycle noise. Empty when the verdict
	// source did not capture a diagnostic stream.
	RawDiagnostics string `json:"raw_diagnostics,omitempty"`
	// DevTriggerID is the event ID of the developer PersonaCompleted that
	// triggered this review. Populated by ReviewHandler from the envelope it
	// received. Used by the review-consolidator to pair verdicts from reviewer
	// and qa that reviewed the same developer iteration — when the consolidator
	// merges, it groups VerdictRendered events by this field so it never folds
	// a stale verdict from iteration N-1 into iteration N's feedback. Empty for
	// events written before this field existed or for non-review verdict sources
	// (committer, quality-gate) that have no developer-iteration concept.
	DevTriggerID string `json:"dev_trigger_id,omitempty"`
}

// UnmarshalJSON tolerates legacy events written with the `phase`/`source_phase`
// fields and verb values ("develop"/"review"). New events use `persona`/
// `source_persona` with handler names. See legacyPersonaFromPhase.
func (v *VerdictPayload) UnmarshalJSON(data []byte) error {
	type alias VerdictPayload
	var raw struct {
		alias
		Phase       string `json:"phase,omitempty"`
		SourcePhase string `json:"source_phase,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*v = VerdictPayload(raw.alias)
	v.Persona = pickPersona(v.Persona, raw.Phase)
	v.SourcePersona = pickPersona(v.SourcePersona, raw.SourcePhase)
	return nil
}

// VerdictSource records which parser path produced a VerdictPayload. Used to
// distinguish a legitimate PASS from a defaulted PASS (no VERDICT: line) or a
// PASS that was demoted from FAIL because every issue failed grounding.
// Operators query verdict_source to triage suspicious-fast PASS workflows.
type VerdictSource string

const (
	// VerdictSourceUnspecified is the zero value — events written before this
	// field existed deserialize to this value.
	VerdictSourceUnspecified VerdictSource = ""
	// VerdictSourceExplicitPass means a "VERDICT: PASS" line was found in the
	// LLM output.
	VerdictSourceExplicitPass VerdictSource = "explicit_pass"
	// VerdictSourceExplicitFail means a "VERDICT: FAIL" line was found.
	VerdictSourceExplicitFail VerdictSource = "explicit_fail"
	// VerdictSourceDefaultOptimistic means no "VERDICT:" line was found and
	// ParseVerdict defaulted to PASS optimistically. The single most actionable
	// signal — surfaces silent malformed-output passes.
	VerdictSourceDefaultOptimistic VerdictSource = "default_optimistic"
	// VerdictSourceDowngradedNoGrounded means the LLM returned an explicit
	// FAIL with N issues, but every issue was filtered out by the
	// pr-category-review grounding check, so the verdict was demoted to PASS.
	VerdictSourceDowngradedNoGrounded VerdictSource = "downgraded_no_grounded"
)

// FeedbackGeneratedPayload is emitted when feedback is prepared for a retry.
type FeedbackGeneratedPayload struct {
	TargetPersona string  `json:"target_persona"`           // handler to reschedule (e.g., "developer")
	SourcePersona string  `json:"source_persona,omitempty"` // handler that generated feedback (e.g., "reviewer", "qa")
	Iteration     int     `json:"iteration"`
	Issues        []Issue `json:"issues"`
	Summary       string  `json:"summary"`
	// RawDiagnostics is forwarded from the source VerdictPayload so the
	// developer's iteration prompt has the unfiltered tail of the failure
	// stream, even when Issue.Description has been trimmed for human
	// readability. Empty when the upstream verdict carried none.
	RawDiagnostics string `json:"raw_diagnostics,omitempty"`
}

// UnmarshalJSON tolerates legacy events. Note: the pre-collapse aggregate
// already wrote the resolved handler name into target_phase/source_phase, so
// no value translation is needed here — it's a pure key rename.
func (p *FeedbackGeneratedPayload) UnmarshalJSON(data []byte) error {
	type alias FeedbackGeneratedPayload
	var raw struct {
		alias
		TargetPhase string `json:"target_phase,omitempty"`
		SourcePhase string `json:"source_phase,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = FeedbackGeneratedPayload(raw.alias)
	if p.TargetPersona == "" {
		p.TargetPersona = raw.TargetPhase
	}
	if p.SourcePersona == "" {
		p.SourcePersona = raw.SourcePhase
	}
	return nil
}

// FeedbackConsumedPayload is emitted when a handler acknowledges feedback.
type FeedbackConsumedPayload struct {
	Persona   string `json:"persona"`
	Iteration int    `json:"iteration"`
}

// UnmarshalJSON tolerates the legacy `phase` field with verb values.
func (p *FeedbackConsumedPayload) UnmarshalJSON(data []byte) error {
	type alias FeedbackConsumedPayload
	var raw struct {
		alias
		Phase string `json:"phase,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = FeedbackConsumedPayload(raw.alias)
	p.Persona = pickPersona(p.Persona, raw.Phase)
	return nil
}

// AIRequestPayload is emitted when an AI backend call is made.
type AIRequestPayload struct {
	Persona    string `json:"persona"`
	Backend    string `json:"backend"` // "claude", "gemini"
	PromptHash string `json:"prompt_hash"` // for dedup, not the full prompt
}

// UnmarshalJSON tolerates the legacy `phase` field with verb values. The
// pre-collapse struct also carried a separate `persona` key set to the system-
// prompt key (often the same string as the handler name), so we prefer the
// new persona, fall back to the legacy phase verb, and translate.
func (p *AIRequestPayload) UnmarshalJSON(data []byte) error {
	type alias AIRequestPayload
	var raw struct {
		alias
		Phase string `json:"phase,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = AIRequestPayload(raw.alias)
	if p.Persona == "" && raw.Phase != "" {
		p.Persona = legacyPersonaFromPhase(raw.Phase)
	}
	return nil
}

// AIRequestStartedPayload is emitted at the moment backend.Run is invoked —
// i.e., the subprocess is about to be exec'd. The gap between AIRequestSent
// and AIRequestStarted is handler pre-flight (prompt build, context load,
// request-event persist). The gap between AIRequestStarted and
// AIResponseReceived is the subprocess itself (stream read, extractor,
// watchdog). Operators diagnose stalls by counting which gap dominates.
type AIRequestStartedPayload struct {
	Persona       string `json:"persona"`
	Backend       string `json:"backend"`
	PromptHash    string `json:"prompt_hash"`
	SpawnUnixNano int64  `json:"spawn_unix_nano"` // time.Now().UnixNano() at spawn call
}

// UnmarshalJSON tolerates the legacy `phase` field with verb values.
func (p *AIRequestStartedPayload) UnmarshalJSON(data []byte) error {
	type alias AIRequestStartedPayload
	var raw struct {
		alias
		Phase string `json:"phase,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = AIRequestStartedPayload(raw.alias)
	if p.Persona == "" && raw.Phase != "" {
		p.Persona = legacyPersonaFromPhase(raw.Phase)
	}
	return nil
}

// AIResponsePayload is emitted when an AI backend returns.
//
// Output is the canonical text every consumer (consolidator, projections,
// MCP tools, agent UI) reads. For pr-category-review handlers it is the
// post-grounding-rewrite text — possibly the canned "No grounded issues
// found…" string. OutputRaw is forensics-only: the original LLM bytes
// captured before rewrite, only populated when grounding mutated the output.
// Future consumers MUST treat OutputRaw as optional.
type AIResponsePayload struct {
	Persona    string          `json:"persona"`
	Backend    string          `json:"backend"`
	TokensUsed int             `json:"tokens_used,omitempty"`
	DurationMS int64           `json:"duration_ms"`
	Structured bool            `json:"structured"`              // was structured output extracted?
	Output     json.RawMessage `json:"output,omitempty"`        // canonical text (post-grounding for pr-category-review)
	OutputRaw  json.RawMessage `json:"output_raw,omitempty"`    // forensics: original LLM text when Output was rewritten
	OutputRef  string          `json:"output_ref,omitempty"`    // blob storage ref for large outputs
}

// UnmarshalJSON tolerates the legacy `phase` field with verb values.
func (p *AIResponsePayload) UnmarshalJSON(data []byte) error {
	type alias AIResponsePayload
	var raw struct {
		alias
		Phase string `json:"phase,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = AIResponsePayload(raw.alias)
	if p.Persona == "" && raw.Phase != "" {
		p.Persona = legacyPersonaFromPhase(raw.Phase)
	}
	return nil
}

// TokenBudgetExceededPayload is emitted when cumulative token usage exceeds the budget.
type TokenBudgetExceededPayload struct {
	TotalUsed int    `json:"total_used"`
	Budget    int    `json:"budget"`
	Persona   string `json:"persona"` // handler that triggered the breach
}

// UnmarshalJSON tolerates the legacy `phase` field with verb values.
func (p *TokenBudgetExceededPayload) UnmarshalJSON(data []byte) error {
	type alias TokenBudgetExceededPayload
	var raw struct {
		alias
		Phase string `json:"phase,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = TokenBudgetExceededPayload(raw.alias)
	if p.Persona == "" && raw.Phase != "" {
		p.Persona = legacyPersonaFromPhase(raw.Phase)
	}
	return nil
}

// PersonaCompletedPayload is emitted when a persona handler finishes successfully.
type PersonaCompletedPayload struct {
	Persona      string `json:"persona"`               // handler name: "developer", "documenter"
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
	// FailureKindOutputTruncated means the persona-runner's developer guard
	// rail tripped: the handler produced suspiciously short output (< 64
	// bytes) while the workspace had uncommitted changes — meaning the
	// model wrote files correctly but Rick lost the descriptive output
	// somewhere upstream (the 2026-04-29 `["sub"]` incident). Surfacing
	// this as a failure pauses the workflow so the operator can inspect
	// the workspace directly instead of running review/qa against garbage.
	FailureKindOutputTruncated FailureKind = "output_truncated"
	// FailureKindStalled means the throttle watchdog observed no engine
	// activity for the workflow within RICK_THROTTLE_STALL_TIMEOUT and
	// auto-failed it to free its concurrency slot. Paused workflows are
	// excluded — only Running aggregates are eligible. Distinct from
	// idle/wall timeouts (which target a single backend call); this is a
	// workflow-level liveness fault.
	FailureKindStalled FailureKind = "stalled"
	// FailureKindRateLimited means the upstream LLM provider rate-limited the
	// request — backend exited non-zero with a recognizable "limit reached"
	// signature (e.g. claude's "You've hit your limit · resets <time>"). The
	// aggregate converts this into WorkflowPaused (not WorkflowFailed) so the
	// parallel branch under a sync-feedback barrier survives the failure and
	// the operator/scheduled resume re-dispatches the persona post-reset.
	FailureKindRateLimited FailureKind = "rate_limited"
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
	TriggerEvent   string      `json:"trigger_event"`
	TriggerID      string      `json:"trigger_id"`
	Reactive       bool        `json:"reactive"`
	Error          string      `json:"error"`
	FailureKind    FailureKind `json:"failure_kind,omitempty"`
	// Backend names the driver that produced the failure when the error came
	// from a BackendError (claude, gemini, codex, or a round-robin composite
	// name like "round-robin(claude,gemini,codex)"). Empty when the failure
	// originated outside a backend call (handler-local error, context cancel).
	Backend        string      `json:"backend,omitempty"`
	Stderr         string      `json:"stderr,omitempty"`
	DurationMS     int64       `json:"duration_ms"`
	ChainDepth     int         `json:"chain_depth"`
	HandlerVersion string      `json:"handler_version,omitempty"`
}

// CompensationPayload is emitted during rollback.
type CompensationPayload struct {
	Persona string `json:"persona"`
	Action  string `json:"action"` // what compensation was performed
}

// UnmarshalJSON tolerates the legacy `phase` field with verb values.
func (p *CompensationPayload) UnmarshalJSON(data []byte) error {
	type alias CompensationPayload
	var raw struct {
		alias
		Phase string `json:"phase,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = CompensationPayload(raw.alias)
	if p.Persona == "" && raw.Phase != "" {
		p.Persona = legacyPersonaFromPhase(raw.Phase)
	}
	return nil
}

// --- Hint payloads ---

// HintEmittedPayload is emitted when a persona runs a lightweight pre-check
// before full execution. The Engine auto-approves or pauses based on confidence.
type HintEmittedPayload struct {
	Persona       string          `json:"persona"`
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

// GroundingDropReason classifies why a pr-category-review issue was rejected
// by the diff-grounding filter (handler/review.go:groundIssue). Aggregated in
// VerdictGroundingSummaryPayload.DropReasons so operators can see whether
// reviewers are systematically producing findings the filter rejects.
type GroundingDropReason string

const (
	// GroundingDropUnspecified is the zero value — used when a drop reason
	// cannot be classified or for back-compat with older events.
	GroundingDropUnspecified GroundingDropReason = ""
	// GroundingDropFileNotInScope means the issue cited a file not in the
	// PR's changed-files set (after basename resolution).
	GroundingDropFileNotInScope GroundingDropReason = "file_not_in_scope"
	// GroundingDropLineNotInChanged means the cited file was in scope but the
	// cited line was not among the lines added in the PR diff.
	GroundingDropLineNotInChanged GroundingDropReason = "line_not_in_changed"
	// GroundingDropTokenNotNearLine means the cited file:line was a changed
	// line, but the backticked code-token in the issue description did not
	// appear in the ±1-line window of changed text.
	GroundingDropTokenNotNearLine GroundingDropReason = "token_not_near_line"
	// GroundingRescuedFileScope means the cited line was NOT in the changed
	// set, but the backtick token from the description appears somewhere in
	// the file's changed lines. The issue survives with Line=0 so the
	// consolidator demotes it to an unanchored body bullet rather than
	// emitting an inline comment at a hallucinated line.
	GroundingRescuedFileScope GroundingDropReason = "rescued_file_scope"
)

// VerdictGroundingSummaryPayload is emitted once per pr-category-review
// invocation (unconditionally, even when the LLM returned PASS with no
// issues). Records how many issues the LLM produced, how many survived the
// diff-grounding filter, and the drop-reason breakdown for the rest.
//
// Operators query this to detect reviewers whose findings are systematically
// being dropped — a sign that either the LLM is hallucinating ungrounded
// claims OR the grounding filter is too strict for legitimate findings.
// The empty-summary case (OriginalCount=0, GroundedCount=0) is itself useful
// signal because its absence would indicate a code-path bug.
//
// Stored on the standard correlation aggregate so it lands alongside the
// VerdictRendered event for the same reviewer. Never published on the bus —
// observability only, no handler subscribers.
type VerdictGroundingSummaryPayload struct {
	Reviewer        string                      `json:"reviewer"`         // handler name, e.g. "pr-data"
	OriginalCount   int                         `json:"original_count"`   // issues parsed from raw LLM output
	GroundedCount   int                         `json:"grounded_count"`   // issues that survived the filter
	DropReasons     map[GroundingDropReason]int `json:"drop_reasons,omitempty"`
	RescuedCount    int                         `json:"rescued_count,omitempty"`
	OriginalOutcome VerdictOutcome              `json:"original_outcome"` // pre-grounding verdict
	FinalOutcome    VerdictOutcome              `json:"final_outcome"`    // post-grounding verdict (after FAIL→PASS demotion if any)
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
// from GitHub. Kind distinguishes the posting intent so projections and tests
// can filter. Skipped is true when the poster short-circuited because an
// identical body already existed on the PR — observability only, not an error.
//
// Kind values:
//   - "summary":      top-level PR comment summarising the round (optional).
//   - "inline-reply": reply on an existing inline review-comment thread;
//                     InReplyToID points at the thread's root comment.
//   - "reply":        legacy top-level reply comment (pre-inline-reply
//                     contract). Retained so historical events still parse.
type PRCommentPostedPayload struct {
	Repo        string `json:"repo"` // "owner/repo"
	PRNumber    int    `json:"pr_number"`
	Kind        string `json:"kind"`                     // "summary", "inline-reply", "reply"
	CommentID   int    `json:"comment_id,omitempty"`     // GitHub comment ID (0 when skipped)
	InReplyToID int    `json:"in_reply_to_id,omitempty"` // for kind="inline-reply": ID of the thread root
	BodyHash    string `json:"body_hash"`                // sha256 hex of the posted body
	Skipped     bool   `json:"skipped,omitempty"`        // true when an identical comment already existed
}
