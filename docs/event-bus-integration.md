# Event Bus Integration Guide

Rick is an event-sourced system. Every state change is an immutable event persisted in SQLite. The event bus delivers these events to subscribers in real time. Any component — persona, projection, side system — can subscribe and react independently.

## Architecture Overview

```
                         ┌──────────────┐
                         │  Event Store │  (SQLite, WAL mode)
                         │              │
                         │  Append()    │  ← Engine, PersonaRunner
                         │  Load()      │  ← Aggregate replay
                         │  LoadByCorr()│  ← Join checks, prompt context
                         └──────┬───────┘
                                │
                         ┌──────▼───────┐
                         │   Event Bus  │  (ChannelBus or OutboxBus)
                         │              │
                         │  Publish()   │  ← Engine, PersonaRunner
                         │  Subscribe() │  ← Targeted event types
                         │ SubscribeAll()│  ← All events (projections)
                         └──────┬───────┘
                                │
              ┌─────────────────┼─────────────────┐
              │                 │                  │
      ┌───────▼──────┐  ┌──────▼───────┐  ┌──────▼───────┐
      │   Personas   │  │  Projections │  │ Side Systems │
      │              │  │              │  │              │
      │ researcher   │  │ token-usage  │  │ slack notify │
      │ architect    │  │ workflow-    │  │ metrics      │
      │ developer    │  │   status     │  │ audit log    │
      │ reviewer     │  │ timeline     │  │ webhooks     │
      │ qa           │  │ verdict      │  │              │
      │              │  └──────────────┘  └──────────────┘
      │ documenter   │
      │ committer    │
      └──────────────┘
```

## The Event Envelope

Every event flowing through the bus is an `event.Envelope`:

```go
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
```

### Key fields for subscribers

| Field | What it means | Use it for |
|-------|--------------|------------|
| `Type` | Event type constant | Filtering — only process events you care about |
| `CorrelationID` | Workflow run ID | Grouping — all events in a workflow share this |
| `AggregateID` | Entity scope | Routing — workflow vs persona-scoped aggregates |
| `Payload` | JSON body | Data — unmarshal into the matching payload struct |
| `CausationID` | Parent event ID | Tracing — build causal chains |
| `Source` | Emitter identity | Debugging — who produced this event |

### Correlation vs Aggregate

```
CorrelationID: "e5e66665"  (= workflow aggregate ID, set once, never changes)

AggregateID varies:
  "e5e66665"                        ← workflow lifecycle events
  "e5e66665:persona:developer"      ← developer's events
  "e5e66665:persona:reviewer"       ← reviewer's events
  "e5e66665:persona:committer"      ← committer's events
```

Use `CorrelationID` to filter events belonging to a specific workflow run.
Use `AggregateID` to filter events from a specific persona within that run.

---

## Event Catalog

### Workflow Lifecycle

These events track the overall workflow state machine: `requested → started → completed|failed|cancelled|paused`.

| Event | Payload | Emitted by | When |
|-------|---------|-----------|------|
| `workflow.requested` | `WorkflowRequestedPayload` | CLI / MCP | User initiates a workflow |
| `workflow.started` | `WorkflowStartedPayload` | Engine | Aggregate accepts the request |
| `workflow.completed` | `WorkflowCompletedPayload` | Engine | All required personas completed |
| `workflow.failed` | `WorkflowFailedPayload` | Engine | Persona failed, budget exceeded, or max iterations |
| `workflow.cancelled` | `WorkflowCancelledPayload` | Operator | User cancellation |
| `workflow.paused` | `WorkflowPausedPayload` | Engine / Operator | Auto-escalation at max iterations, or operator pause |
| `workflow.resumed` | `WorkflowResumedPayload` | Operator | Operator resumes a paused workflow |

```go
// workflow.requested
type WorkflowRequestedPayload struct {
    Prompt     string `json:"prompt"`               // User's task description
    WorkflowID string `json:"workflow_id"`           // "default", "workspace-dev", etc.
    Source     string `json:"source"`                // "raw", "gh:owner/repo#1", "jira:KEY-123"
    Repo       string `json:"repo,omitempty"`        // Repository name for workspace
    Ticket     string `json:"ticket,omitempty"`      // Ticket/branch name
    BaseBranch string `json:"base_branch,omitempty"` // Git base branch
    Isolate    bool   `json:"isolate,omitempty"`     // Copy-based workspace isolation
}

// workflow.started
type WorkflowStartedPayload struct {
    WorkflowID string   `json:"workflow_id"`
    Phases     []string `json:"phases"`    // Required persona names
}

// workflow.completed
type WorkflowCompletedPayload struct {
    Result string `json:"result"`
}

// workflow.failed
type WorkflowFailedPayload struct {
    Reason string `json:"reason"`
    Phase  string `json:"phase,omitempty"` // Which persona caused failure
}

// workflow.paused
type WorkflowPausedPayload struct {
    Reason string `json:"reason"`
    Source string `json:"source,omitempty"` // "operator", "engine:auto-escalation"
}

// workflow.resumed
type WorkflowResumedPayload struct {
    Reason string `json:"reason,omitempty"`
}
```

### Persona Lifecycle

Core choreography events. Every persona execution produces exactly one of these.

| Event | Payload | Emitted by | When |
|-------|---------|-----------|------|
| `persona.completed` | `PersonaCompletedPayload` | PersonaRunner | Handler returned successfully |
| `persona.failed` | `PersonaFailedPayload` | PersonaRunner | Handler returned an error |

```go
type PersonaCompletedPayload struct {
    Persona      string `json:"persona"`              // "developer", "reviewer", etc.
    Phase        string `json:"phase,omitempty"`       // Workflow phase name
    TriggerEvent string `json:"trigger_event"`         // Event type that woke this persona
    TriggerID    string `json:"trigger_id"`            // ID of the triggering event
    Reactive     bool   `json:"reactive"`              // true = bus-triggered
    OutputRef    string `json:"output_ref,omitempty"`  // Event ID of AIResponseReceived
    DurationMS   int64  `json:"duration_ms"`           // Execution wall time
    ChainDepth   int    `json:"chain_depth"`           // Reactive chain depth
}

type PersonaFailedPayload struct {
    Persona      string `json:"persona"`
    Phase        string `json:"phase,omitempty"`
    TriggerEvent string `json:"trigger_event"`
    TriggerID    string `json:"trigger_id"`
    Reactive     bool   `json:"reactive"`
    Error        string `json:"error"`
    DurationMS   int64  `json:"duration_ms"`
    ChainDepth   int    `json:"chain_depth"`
}
```

**`OutputRef`**: Points to the `AIResponseReceived` event ID containing the full LLM output. Use `store.LoadByCorrelation(ctx, corrID)` to find it. This avoids duplicating 100KB+ LLM responses in PersonaCompleted payloads.

**`ChainDepth`**: Incremented per reactive hop. A persona triggered by `WorkflowStarted` has depth 0. A persona triggered by that persona's `PersonaCompleted` has depth 1. Max default: 5.

### AI Operations

| Event | Payload | Emitted by | When |
|-------|---------|-----------|------|
| `ai.request.sent` | `AIRequestPayload` | AI handlers | Before calling the LLM backend |
| `ai.response.received` | `AIResponsePayload` | AI handlers | After LLM returns |

```go
type AIRequestPayload struct {
    Phase      string `json:"phase"`
    Backend    string `json:"backend"`     // "claude", "gemini"
    Persona    string `json:"persona"`
    PromptHash string `json:"prompt_hash"` // SHA-256 prefix (not the full prompt)
}

type AIResponsePayload struct {
    Phase      string          `json:"phase"`
    Backend    string          `json:"backend"`
    TokensUsed int             `json:"tokens_used,omitempty"`
    DurationMS int64           `json:"duration_ms"`
    Structured bool            `json:"structured"`
    Output     json.RawMessage `json:"output,omitempty"`     // Full LLM response
    OutputRef  string          `json:"output_ref,omitempty"`
}
```

### Feedback Cycle

| Event | Payload | Emitted by | When |
|-------|---------|-----------|------|
| `verdict.rendered` | `VerdictPayload` | Reviewer / QA handlers | After evaluating a persona's output |
| `feedback.generated` | `FeedbackGeneratedPayload` | Engine (aggregate) | When verdict is `fail` and iterations remain |

```go
type VerdictPayload struct {
    Phase       string         `json:"phase"`        // Phase being evaluated (e.g., "developer")
    SourcePhase string         `json:"source_phase"` // Who rendered the verdict (e.g., "reviewer")
    Outcome     VerdictOutcome `json:"outcome"`      // "pass", "fail", "unknown"
    Issues      []Issue        `json:"issues,omitempty"`
    Summary     string         `json:"summary"`
}

type FeedbackGeneratedPayload struct {
    TargetPhase string  `json:"target_phase"`           // Phase to re-run
    SourcePhase string  `json:"source_phase,omitempty"` // Phase that generated feedback
    Iteration   int     `json:"iteration"`
    Issues      []Issue `json:"issues"`
    Summary     string  `json:"summary"`
}

type Issue struct {
    Severity    string `json:"severity"`    // "critical", "major", "minor"
    Category    string `json:"category"`    // "correctness", "security", "style"
    Description string `json:"description"`
    File        string `json:"file,omitempty"`
    Line        int    `json:"line,omitempty"`
}
```

### Context Snapshots

| Event | Payload | Emitted by | When |
|-------|---------|-----------|------|
| `context.codebase` | `ContextCodebasePayload` | Context-snapshot handler | After scanning workspace |
| `context.schema` | `ContextSchemaPayload` | Context-snapshot handler | After scanning proto/SQL/GraphQL |
| `context.git` | `ContextGitPayload` | Context-snapshot handler | After capturing git state |
| `context.enrichment` | `ContextEnrichmentPayload` | Before-hook handlers | External system injected library/component suggestions |

```go
type ContextEnrichmentPayload struct {
    Source  string           `json:"source"`            // enricher identity
    Kind    string           `json:"kind"`              // "libraries", "components", "patterns"
    Items   []EnrichmentItem `json:"items"`
    Summary string           `json:"summary,omitempty"`
}

type EnrichmentItem struct {
    Name       string `json:"name"`
    Version    string `json:"version,omitempty"`
    Reason     string `json:"reason"`
    DocURL     string `json:"doc_url,omitempty"`
    ImportPath string `json:"import_path,omitempty"`
}
```

### Workspace

| Event | Payload | Emitted by | When |
|-------|---------|-----------|------|
| `workspace.ready` | `WorkspaceReadyPayload` | Workspace handler | Git workspace provisioned |

```go
type WorkspaceReadyPayload struct {
    Path     string `json:"path"`     // Absolute filesystem path
    Branch   string `json:"branch"`   // Git branch name (e.g., "issue-368")
    Base     string `json:"base"`     // Base branch (e.g., "main")
    Isolated bool   `json:"isolated"` // True if copy-based isolation
}
```

### Budget

| Event | Payload | Emitted by | When |
|-------|---------|-----------|------|
| `token.budget.exceeded` | `TokenBudgetExceededPayload` | Token tracking | Cumulative tokens exceed budget |

```go
type TokenBudgetExceededPayload struct {
    TotalUsed int    `json:"total_used"`
    Budget    int    `json:"budget"`
    Phase     string `json:"phase"` // Phase that triggered the breach
}
```

### Operator Intervention

| Event | Payload | Emitted by | When |
|-------|---------|-----------|------|
| `operator.guidance` | `OperatorGuidancePayload` | Operator (CLI/MCP) | Operator injects context into a running workflow |

```go
type OperatorGuidancePayload struct {
    Content    string `json:"content"`                // Operator's text input
    Target     string `json:"target,omitempty"`       // Target persona (optional)
    AutoResume bool   `json:"auto_resume,omitempty"`  // Resume workflow after injection
}
```

**Resume-after-escalation flow:**

When `AutoResume` is true and the workflow was paused by auto-escalation (MaxIterations reached):

1. `OperatorGuidance` event persisted in correlation chain
2. `WorkflowResumed` event → Engine detects escalation pause
3. Engine bumps `MaxIterations` by 1 and re-emits `FeedbackGenerated`
4. Target persona re-triggers with operator guidance visible via `store.LoadByCorrelation()`

### Other Events

| Event | Status |
|-------|--------|
| `ai.structured_output` | Reserved for future structured extraction |
| `feedback.consumed` | Emitted when a handler acknowledges feedback |
| `compensation.started` | Emitted during rollback operations |
| `compensation.completed` | Emitted when rollback finishes |

---

## Real Event Traces (from test suite)

The following traces were captured from `internal/engine/trace_test.go` — actual events flowing through the system with real choreography.

### Trace A: Default Workflow — Happy Path (6 personas, no feedback)

9 events total: `workflow.requested ×1, workflow.started ×1, persona.completed ×6, workflow.completed ×1`

```
Offset  Event                  Source                          Details
──────────────────────────────────────────────────────────────────────────────────────
0ms     workflow.requested     wf-1 [cli]                      prompt="Build REST API" wf=default
0ms     workflow.started       wf-1 [engine:aggregate]         phases=[researcher architect developer reviewer qa committer]
1ms     persona.completed      wf-1:persona:researcher         persona=researcher trigger=workflow.started chain=0
                               [persona-runner:researcher]
1ms     persona.completed      wf-1:persona:architect          persona=architect trigger=persona.completed chain=1
                               [persona-runner:architect]
2ms     persona.completed      wf-1:persona:developer          persona=developer trigger=persona.completed chain=2
                               [persona-runner:developer]
        ┌──────────── PARALLEL FAN-OUT ────────────┐
2ms     │ persona.completed    wf-1:persona:qa                persona=qa trigger=persona.completed chain=3
        │                      [persona-runner:qa]
3ms     │ persona.completed    wf-1:persona:reviewer           persona=reviewer trigger=persona.completed chain=3
        │                      [persona-runner:reviewer]
        └──────────────────────────────────────────┘
3ms     persona.completed      wf-1:persona:committer          persona=committer trigger=persona.completed chain=4
                               [persona-runner:committer]       (join gate: reviewer ✓ qa ✓)
4ms     workflow.completed     wf-1 [engine:aggregate]         all required personas completed
```

### Trace B: Default Workflow — With Feedback Loop (reviewer fails once)

15 events total: `workflow.requested ×1, workflow.started ×1, persona.completed ×10, verdict.rendered ×1, feedback.generated ×1, workflow.completed ×1`

```
Offset  Event                  Source                          Details
──────────────────────────────────────────────────────────────────────────────────────

        ═══════ ROUND 1 ═══════
0ms     workflow.requested     wf-1 [cli]                      prompt="Build REST API" wf=default
0ms     workflow.started       wf-1 [engine:aggregate]         phases=[researcher architect developer reviewer qa committer]
0ms     persona.completed      wf-1:persona:researcher         persona=researcher trigger=workflow.started chain=0
1ms     persona.completed      wf-1:persona:architect          persona=architect trigger=persona.completed chain=1
2ms     persona.completed      wf-1:persona:developer          persona=developer trigger=persona.completed chain=2
        ┌──────────── PARALLEL FAN-OUT ────────────┐
2ms     │ persona.completed    wf-1:persona:reviewer           persona=reviewer chain=3
        │ verdict.rendered     wf-1:persona:reviewer           outcome=FAIL phase=developer source=reviewer issues=2
        │                                                      "missing error handling in handler.go:42"
3ms     │ persona.completed    wf-1:persona:qa                persona=qa chain=3
        └──────────────────────────────────────────┘
3ms     persona.completed      wf-1:persona:committer          persona=committer chain=4 (stale — cleared by feedback)

        ═══════ FEEDBACK LOOP ═══════
4ms     feedback.generated     wf-1 [engine:aggregate]         target=developer source=reviewer iter=1
                                                               Engine clears CompletedPersonas[developer, reviewer]

        ═══════ ROUND 2 ═══════
4ms     persona.completed      wf-1:persona:developer          persona=developer trigger=feedback.generated chain=0
        ┌──────────── PARALLEL RE-FIRE ────────────┐
5ms     │ persona.completed    wf-1:persona:reviewer           persona=reviewer chain=1 (PASS — no verdict)
5ms     │ persona.completed    wf-1:persona:qa                persona=qa chain=1
        └──────────────────────────────────────────┘
6ms     persona.completed      wf-1:persona:committer          persona=committer chain=2 (join: reviewer ✓ qa ✓)
6ms     workflow.completed     wf-1 [engine:aggregate]         all required personas completed
```

### Trace C: Workspace-Dev Workflow — Happy Path

9 events total: `workflow.requested ×1, workflow.started ×1, persona.completed ×5, context.codebase ×1, workflow.completed ×1`

```
Offset  Event                  Source                          Details
──────────────────────────────────────────────────────────────────────────────────────
0ms     workflow.requested     wf-1 [cli]                      prompt="Fix NULL scan" wf=workspace-dev
0ms     workflow.started       wf-1 [engine:aggregate]         phases=[workspace context-snapshot developer reviewer committer]
0ms     persona.completed      wf-1:persona:workspace          persona=workspace trigger=workflow.started chain=0
1ms     persona.completed      wf-1:persona:context-snapshot   persona=context-snapshot trigger=persona.completed chain=1
1ms     context.codebase       wf-1:persona:context-snapshot   language=go framework=grpc files=3
1ms     persona.completed      wf-1:persona:developer          persona=developer trigger=persona.completed chain=2
2ms     persona.completed      wf-1:persona:reviewer           persona=reviewer trigger=persona.completed chain=3
2ms     persona.completed      wf-1:persona:committer          persona=committer trigger=persona.completed chain=4
2ms     workflow.completed     wf-1 [engine:aggregate]         all required personas completed
```

### Trace D: Auto-Escalation (max iterations → pause)

10 events total: `workflow.requested ×1, workflow.started ×1, persona.completed ×4, verdict.rendered ×2, feedback.generated ×1, workflow.paused ×1`

```
Offset  Event                  Source                          Details
──────────────────────────────────────────────────────────────────────────────────────
0ms     workflow.requested     wf-1 [cli]                      prompt="Fix bug" wf=escalate-test
0ms     workflow.started       wf-1 [engine:aggregate]         phases=[developer reviewer] maxIter=1

        ═══════ ROUND 1 ═══════
0ms     persona.completed      wf-1:persona:developer          persona=developer trigger=workflow.started chain=0
1ms     persona.completed      wf-1:persona:reviewer           persona=reviewer chain=1
1ms     verdict.rendered       wf-1:persona:reviewer           outcome=FAIL phase=developer source=reviewer
1ms     feedback.generated     wf-1 [engine:aggregate]         target=developer source=reviewer iter=1

        ═══════ ROUND 2 ═══════
1ms     persona.completed      wf-1:persona:developer          persona=developer trigger=feedback.generated chain=0
2ms     persona.completed      wf-1:persona:reviewer           persona=reviewer chain=1
2ms     verdict.rendered       wf-1:persona:reviewer           outcome=FAIL phase=developer source=reviewer

        ═══════ ESCALATION ═══════
2ms     workflow.paused        wf-1 [engine:aggregate]         "max iterations (1) reached for developer — escalated to operator"
                                                               source=engine:auto-escalation
                                                               → Operator must resume with guidance or increased limits
```

---

## How to Subscribe

### Option 1: Subscribe to specific event types

For side systems that react to specific events (notifications, webhooks, metrics):

```go
unsub := bus.Subscribe(event.WorkflowCompleted, func(ctx context.Context, env event.Envelope) error {
    var p event.WorkflowCompletedPayload
    if err := json.Unmarshal(env.Payload, &p); err != nil {
        return err
    }
    log.Printf("Workflow %s completed: %s", env.AggregateID[:8], p.Result)
    return nil
}, eventbus.WithName("my-notifier"))

// Call unsub() to unsubscribe when done.
defer unsub()
```

### Option 2: Subscribe to all events

For projections, audit logs, or metrics collectors that need full visibility:

```go
unsub := bus.SubscribeAll(func(ctx context.Context, env event.Envelope) error {
    metrics.Inc("events.total", "type", string(env.Type))
    return nil
}, eventbus.WithName("metrics-collector"))
```

### Option 3: Projection runner (catch-up + live)

For read models that need both historical replay and live updates:

```go
projRunner := projection.NewRunner(store, bus, logger)
projRunner.Register(myProjection)       // implements projection.Projector
if err := projRunner.Start(ctx); err != nil {
    return err
}
defer projRunner.Stop()
```

The runner replays all historical events from the store first, then subscribes to live events. No gaps.

### Option 4: Before-hook (enrichment interceptor)

For external systems that need to inject context before a persona runs — without modifying handler code:

```go
// 1. Register the enrichment handler with its own trigger.
enricher := &myEnricherHandler{
    // Subscribes to PersonaCompleted, AfterPersonas: ["architect"]
}
registry.Register(enricher)

// 2. Configure the hook — developer waits for enricher before dispatching.
runner := NewPersonaRunner(store, bus, dispatcher, logger,
    WithBeforeHook("developer", "my-enricher"),
)
```

The enricher runs after architect completes, emits `context.enrichment` events, and developer's join condition is dynamically augmented to include the enricher. The developer sees enrichment events in `store.LoadByCorrelation()`.

### Option 5: gRPC stream (external service)

For external systems in any language that need to participate in the choreography as full handlers:

```go
// Server-side setup (Rick)
streamD := grpchandler.NewStreamDispatcher(logger)
compositeD := grpchandler.NewCompositeDispatcher(localDispatcher, streamD)
runner := engine.NewPersonaRunner(store, bus, compositeD, logger)

srv := grpchandler.NewServer(streamD, runner, logger)
pb.RegisterPersonaServiceServer(grpcServer, srv)
```

```python
# Client-side (Python example)
stream = stub.HandleStream()

# Register as a handler
stream.send(HandlerMessage(registration=HandlerRegistration(
    name="frontend-enricher",
    event_types=["persona.completed"],
    after_personas=["architect"],
    before_hook_targets=["developer"],
)))

ack = stream.recv()  # RegistrationAck{status: "ok"}

# Handle dispatched events
for msg in stream:
    dispatch = msg.dispatch
    # ... process event, build enrichment ...
    stream.send(HandlerMessage(result=HandlerResult(
        dispatch_id=dispatch.dispatch_id,
        events=[enrichment_event],
    )))
```

The stream lifecycle IS the registration — opening registers, closing deregisters. All safety guards (dedup, chain depth, join conditions, priority queue) remain in Rick. The external system is a pure event processor.

### Reconnecting client library

For production reliability, use `grpchandler.Client` instead of managing streams directly:

```go
client := grpchandler.NewClient(conn, grpchandler.ClientConfig{
    Name:       "security-scanner",
    EventTypes: []string{"persona.completed"},
    Handler:    myHandlerFunc,
    MaxRetries: 0, // 0 = unlimited
})
err := client.Run(ctx) // blocks, reconnects on stream failure with exponential backoff
```

Backoff: 1s → 2s → 4s → ... capped at 30s. Re-sends `HandlerRegistration` on each reconnect.

---

## Subscriber Guarantees

### What you CAN rely on

| Guarantee | Details |
|-----------|---------|
| **At-least-once delivery** | Every published event is delivered to every matching subscriber. Failures are retried (with middleware) or dead-lettered. |
| **Per-subscriber ordering** | A single subscriber receives events in publication order when using `WithSync()`. Async subscribers (default) may receive events out of order. |
| **Correlation grouping** | All events in a workflow share the same `CorrelationID`. Use `store.LoadByCorrelation(ctx, corrID)` to get the full history. |
| **Event immutability** | Events are never modified or deleted. The store is append-only. |
| **Schema versioning** | Every event has a `SchemaVersion` field. Old events can be upcasted to new schemas. |
| **Crash recovery** | `OutboxBus` persists events before publishing. Replay from `LastPosition()` on restart. |
| **External handler isolation** | gRPC stream handlers get the same safety guarantees as local handlers. Join conditions, priority queue, chain depth — all enforced by PersonaRunner. |

### What you CANNOT rely on

| Non-guarantee | Why | Workaround |
|---------------|-----|------------|
| **Exactly-once delivery** | Network/process failures can cause redelivery | Use `IdempotencyMiddleware` or track event IDs yourself |
| **Cross-subscriber ordering** | Async dispatch spawns goroutines per subscriber | Use `WithSync()` if ordering matters, or use timestamps |
| **Immediate delivery** | OutboxBus polls at intervals (default 100ms) | Use `ChannelBus` for in-process; OutboxBus `Publish()` nudges the poll loop to reduce latency |
| **Subscriber error isolation** | By default, errors are logged but don't propagate | Subscriber errors never affect the publishing workflow |

### Error handling

- **Subscriber returns error** → logged + recorded as dead letter (if `DeadLetterRecorder` configured). Does NOT fail the workflow.
- **Subscriber panics** → caught by `RecoveryMiddleware` (if configured), converted to error.
- **Subscriber is slow** → default async dispatch means slow subscribers don't block others. Use `TimeoutMiddleware` to enforce limits.

---

## Dispatch Queue (Priority)

PersonaRunner serializes events per (handler, correlation) through a priority queue. A handler never runs concurrently on the same workflow. When multiple events are pending, the highest-priority event processes first.

| Priority | Event Type | Value |
|----------|-----------|-------|
| Highest | `operator.guidance` | 0 |
| High | `feedback.generated` | 10 |
| Normal | `persona.completed` / `persona.failed` | 20 |
| Default | `workflow.started`, others | 30 |

Within the same priority level, events are processed FIFO (arrival order). Different handlers and different workflows run in parallel — the queue only serializes within a single (handler, correlation) pair.

---

## Tag-Based Correlation Lookup

The Engine automatically indexes business keys from `WorkflowRequested` as tags. External systems discover workflow correlation IDs by business identifier instead of UUID.

```go
// Discover workflow by Jira ticket
ids, err := store.LoadByTag(ctx, "ticket", "PROJ-123")
// ids = ["e5e66665-..."]  (correlation IDs)

// Then load the full event history
events, err := store.LoadByCorrelation(ctx, ids[0])
```

### Auto-indexed tags

| Tag Key | Extracted From | Example Value |
|---------|---------------|---------------|
| `source` | `WorkflowRequested.Source` | `"jira:PROJ-123"` |
| `ticket` | `WorkflowRequested.Ticket` | `"PROJ-123"` |
| `repo` | `WorkflowRequested.Repo` | `"acme/myapp"` |
| `repo_branch` | `Repo:BaseBranch` (composite) | `"acme/myapp:main"` |
| `workflow_id` | `WorkflowRequested.WorkflowID` | `"default"` |

Multiple workflows can share the same tag (e.g., re-running the same ticket). `LoadByTag` returns all matching correlation IDs. Tags are stored in the `event_tags` SQLite table and are idempotent (`INSERT OR IGNORE`).

---

## Middleware

Middleware wraps subscriber handlers. Applied at subscribe-time, not per-dispatch.

```go
mw := eventbus.Chain(
    eventbus.RecoveryMiddleware(),                        // Catch panics
    eventbus.LoggingMiddleware(logger),                   // Log event handling
    eventbus.TimeoutMiddleware(30 * time.Second),         // Max handler duration
    eventbus.RetryMiddleware(3, 100*time.Millisecond),    // Retry with backoff
    eventbus.IdempotencyMiddleware(10000),                // Dedup by event ID
)

bus := eventbus.NewChannelBus(eventbus.WithMiddleware(mw))
```

| Middleware | What it does |
|-----------|-------------|
| `RecoveryMiddleware` | Catches panics, converts to errors |
| `LoggingMiddleware` | Logs event type, duration, errors |
| `TimeoutMiddleware` | Enforces max handler execution time |
| `RetryMiddleware` | Exponential backoff retry on failure |
| `IdempotencyMiddleware` | Skips duplicate event IDs (map with capacity eviction) |
| `CircuitBreakerMiddleware` | Opens circuit after N failures, resets after timeout |
| `MetricsMiddleware` | Records processing counts, latency, errors |

---

## Context Sharing Between Personas

Personas don't pass data directly. They write events to the store and read prior events from the correlation chain.

```
Developer writes:
  ai.response.received {phase: "develop", output: "<code changes>"}

Reviewer reads:
  store.LoadByCorrelation(correlationID)
    → finds ai.response.received where phase == "develop"
    → pctx.Outputs["develop"] = "<code changes>"
```

### What each persona sees in its PromptContext

| Field | Populated from | Example |
|-------|---------------|---------|
| `Task` | `WorkflowRequested.Prompt` | "Fix NULL scan error in GetClinic" |
| `Source` | `WorkflowRequested.Source` | "gh:acme/myapp#368" |
| `Outputs["research"]` | `AIResponseReceived` where phase=research | Research findings |
| `Outputs["architect"]` | `AIResponseReceived` where phase=architect | Architecture plan |
| `Outputs["develop"]` | `AIResponseReceived` where phase=develop | Code changes |
| `Feedback` | `FeedbackGenerated` targeting this persona | "needs work: missing test" |
| `Iteration` | `FeedbackGenerated.Iteration` | 2 |
| `WorkspacePath` | `WorkspaceReady.Path` | "/home/user/repos/clinic" |
| `Ticket` | `WorkspaceReady.Branch` | "issue-368" |
| `BaseBranch` | `WorkspaceReady.Base` | "main" |
| `Codebase` | `ContextCodebase.Tree` + `Files` | File tree + key file contents |

### Reading context from a side system

Any component with store access can read the full workflow state:

```go
events, err := store.LoadByCorrelation(ctx, correlationID)
for _, env := range events {
    switch env.Type {
    case event.PersonaCompleted:
        var p event.PersonaCompletedPayload
        json.Unmarshal(env.Payload, &p)
        // p.Persona, p.DurationMS, p.OutputRef ...
    case event.AIResponseReceived:
        var p event.AIResponsePayload
        json.Unmarshal(env.Payload, &p)
        // p.Output contains the full LLM response
    }
}
```

---

## Bus Implementations

### ChannelBus (in-process)

Default. Uses Go channels and goroutines. Events are delivered immediately but not durable — a process crash loses in-flight events.

```go
bus := eventbus.NewChannelBus(
    eventbus.WithLogger(logger),
    eventbus.WithMiddleware(mw),
    eventbus.WithDeadLetterRecorder(store),
)
```

- Async dispatch by default (goroutine per subscriber)
- `WithSync()` option for ordered delivery
- `Close()` waits for in-flight handlers

### OutboxBus (persistent)

Polls the event store for new events. Survives process crashes — events are already persisted before publishing.

```go
// PollFunc adapter — OutboxBus expects []PollResult, store returns []PositionedEvent
pollFn := func(ctx context.Context, after int64, limit int) ([]eventbus.PollResult, error) {
    events, err := store.LoadAll(ctx, after, limit)
    if err != nil {
        return nil, err
    }
    results := make([]eventbus.PollResult, len(events))
    for i, pe := range events {
        results[i] = eventbus.PollResult{Position: pe.Position, Event: pe.Event}
    }
    return results, nil
}

bus := eventbus.NewOutboxBus(pollFn,
    eventbus.WithPollInterval(100 * time.Millisecond),
    eventbus.WithStartPosition(lastCheckpoint),
)
bus.Start(ctx)
defer bus.Close()
```

- Guarantees delivery of all persisted events
- `LastPosition()` returns checkpoint for recovery
- `Publish()` nudges the poll loop for faster delivery

---

## Quick Start: Adding a Side System

```go
// 1. Subscribe to the events you care about
unsub := bus.Subscribe(event.PersonaCompleted, func(ctx context.Context, env event.Envelope) error {
    // 2. Filter by correlation if you only care about specific workflows
    if env.CorrelationID != myWorkflowID {
        return nil
    }

    // 3. Unmarshal the payload
    var p event.PersonaCompletedPayload
    if err := json.Unmarshal(env.Payload, &p); err != nil {
        return fmt.Errorf("unmarshal: %w", err)
    }

    // 4. Do your thing
    log.Printf("Persona %s completed in %dms (chain depth: %d)",
        p.Persona, p.DurationMS, p.ChainDepth)

    return nil
}, eventbus.WithName("my-side-system"))

// 5. Unsubscribe when done
defer unsub()
```

### Option 4: Before-hook (enrichment interceptor)

For external systems that need to inject context before a persona runs — without modifying handler code:

```go
// 1. Register the enrichment handler with its own trigger.
enricher := &myEnricherHandler{
    // Subscribes to PersonaCompleted, AfterPersonas: ["architect"]
}
registry.Register(enricher)

// 2. Configure the hook — developer waits for enricher before dispatching.
runner := NewPersonaRunner(store, bus, dispatcher, logger,
    WithBeforeHook("developer", "my-enricher"),
)
```

The enricher runs after architect completes, emits `context.enrichment` events, and developer's join condition is dynamically augmented to include the enricher. The developer sees enrichment events in `store.LoadByCorrelation()`.

---

### Rules for subscribers

1. **Be idempotent** — you may receive the same event more than once
2. **Don't block** — long operations should be offloaded to a goroutine
3. **Errors are isolated** — returning an error logs it but never affects the workflow
4. **Filter by correlation** — the bus delivers to ALL subscribers of a type, not just your workflow
5. **Don't mutate the envelope** — it's shared across subscribers
