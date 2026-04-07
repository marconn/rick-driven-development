# package event

Defines the canonical immutable `Envelope`, all event `Type` constants, and the payload structs that every other package in Rick produces or consumes.

## Files
- `envelope.go` — `ID`, `Type`, `Envelope` struct, `New`/`With*` builders, all event `Type` constants, `WorkflowStartedFor`/`IsWorkflowStarted` helpers
- `payload.go` — typed payload structs for each event type, `MustMarshal` helper, shared enums (`VerdictOutcome`, `Issue`, `FileEntry`, `EnrichmentItem`)
- `registry.go` — `Registry` for schema versioning + `Upcaster` migration chain, `DefaultRegistry()` pre-populated with core types
- `envelope_test.go`, `registry_test.go` — unit tests (skip unless touching invariants)

## Key types
- `Envelope` — immutable wrapper: `ID`, `Type`, `AggregateID`, `Version`, `SchemaVersion`, `Timestamp`, `CausationID`, `CorrelationID`, `Source`, `Payload (json.RawMessage)`. Built via `New(type, schemaVersion, payload)` then chained with `WithAggregate`, `WithCausation`, `WithCorrelation`, `WithSource`
- `ID` — string alias, `NewID()` returns a UUIDv4
- `Type` — string alias for event type names like `"workflow.started"`, `"persona.completed"`
- `Registry` — schema version tracker; `Upcast()` walks registered upcasters from old → current version. Unknown types pass through

## Event types (grouped)
- **Workflow lifecycle**: `WorkflowRequested`, `WorkflowStarted` (+ `WorkflowStartedFor(id)` workflow-scoped variants), `WorkflowCompleted`, `WorkflowFailed`, `WorkflowCancelled`
- **Operator intervention**: `WorkflowPaused`, `WorkflowResumed`, `OperatorGuidance`, `WorkflowRerouted`
- **Persona lifecycle**: `PersonaCompleted`, `PersonaFailed`, `PersonaTracked` (internal — never published, used by aggregate replay)
- **Feedback / verdict**: `VerdictRendered`, `FeedbackGenerated`, `FeedbackConsumed`
- **Hint**: `HintEmitted`, `HintApproved`, `HintRejected`
- **AI**: `AIRequestSent`, `AIResponseReceived`, `AIStructuredOutput`
- **Context snapshots**: `ContextCodebase`, `ContextSchema`, `ContextGit`, `ContextEnrichment`
- **Compensation**: `CompensationStarted`, `CompensationCompleted`
- **Workspace**: `WorkspaceReady`
- **Budget**: `TokenBudgetExceeded`
- **Sentinel**: `UnhandledEventDetected`
- **Child workflow**: `ChildWorkflowCompleted`

## Key payload structs
- `WorkflowRequestedPayload` — `Prompt`, `WorkflowID` (DAG to run), `Source`, optional workspace params (`Repo`, `Ticket`, `RepoBranch`, `BaseBranch`, `Isolate`)
- `WorkflowStartedPayload` — `WorkflowID`, ordered `Phases`, optional `Source`/`Ticket`/`Prompt`
- `PersonaCompletedPayload` / `PersonaFailedPayload` — `Persona`, `Phase`, `TriggerEvent`, `TriggerID`, `Reactive`, `OutputRef` (event ID, not duplicated payload), `DurationMS`, `ChainDepth`
- `VerdictPayload` — `Phase`, `SourcePhase`, `Outcome` (`pass`/`fail`/`unknown`), `Issues[]`, `Summary`
- `FeedbackGeneratedPayload` — `TargetPhase`, `SourcePhase`, `Iteration`, `Issues[]`, `Summary`
- `HintEmittedPayload` — `Persona`, `Confidence` (0–1), `Plan`, `Blockers[]`, `TokenEstimate`, `TriggerID` for replay
- `OperatorGuidancePayload` — `Content`, `Target` persona, `AutoResume`
- `ContextCodebasePayload` / `ContextGitPayload` / `ContextSchemaPayload` / `ContextEnrichmentPayload` — ground-truth snapshots and before-hook enrichment

## Conventions
- Payloads marshalled with `MustMarshal(v)` — panics on bad input, only use for trusted in-process structs
- `Envelope` is immutable: `With*` methods return a copy, never mutate the receiver
- Caller (engine/handler) sets `AggregateID`, `Version`, `CorrelationID`, `Source` after `New()`
- `WorkflowStarted` constant must NOT be subscribed to directly — use `WorkflowStartedFor(workflowID)` for scoped subscriptions; `IsWorkflowStarted()` checks both forms
- `OutputRef` pattern: large LLM outputs are referenced by event ID rather than duplicated in `PersonaCompleted` payloads
- New event types: add `Type` constant in `envelope.go`, payload struct in `payload.go`, register in `DefaultRegistry()` if it needs versioning
- Schema bump: increment `SchemaVersion` at `New()` call site, register an `Upcaster` from old → new version

## Related
- Consumed by every package — `internal/eventstore`, `internal/eventbus`, `internal/engine`, `internal/handler`, `internal/projection`, `internal/grpchandler`, `internal/mcp`
- Produced primarily by `internal/engine` (lifecycle/feedback/hint decisions) and `internal/handler/*` implementations (persona results, context snapshots)
- Stored by `internal/eventstore` (SQLite, optimistic concurrency on `AggregateID`+`Version`); routed by `internal/eventbus`
- Workflow-scoped `WorkflowStartedFor` types are computed from `WorkflowDef.Graph` in `internal/engine/persona_runner.go`
