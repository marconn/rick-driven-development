# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build, Test, Lint

```bash
# Build
go build -o rick ./cmd/rick

# Test
go test ./...                                          # all packages
go test -race ./...                                    # with race detector
go test -v ./internal/engine                           # single package
go test -run TestNewDAGValid ./internal/engine          # single test

# Lint
golangci-lint run

# Pre-commit: always run both before committing
golangci-lint run && go test ./...
```

## Architecture

Rick is an event-sourced AI workflow system built on **DAG-based orchestration**. All state changes are immutable events in SQLite. Execution topology is defined in `WorkflowDef.Graph` — handlers are dumb workers that know nothing about when they fire. PersonaRunner reads the DAG and dispatches accordingly.

### Design Principle: DAG-Based Dispatch

Execution order lives in `WorkflowDef.Graph`, NOT in individual handlers. Handlers implement `Name()` + `Handle()` — they have no triggers, no event subscriptions, no join conditions. PersonaRunner computes subscriptions from the Graph at startup.

**Single handler, multiple workflows**: The same handler (e.g., "developer") participates in `workspace-dev`, `jira-dev`, `pr-feedback`, and `ci-fix` — the Graph scopes which handlers fire per workflow. No more prefix hacks (`jira-developer`, `pr-reviewer`).

### Event Flow

**Workspace dev workflow** (`workspace-dev`): Provisions git branch first, escalates on max iterations.
```
WorkflowStarted
  → workspace (provisions git branch)
    → context-snapshot (captures codebase state)
      → developer (after: [context-snapshot], retries on FeedbackGenerated)
        → reviewer (after: [developer])  ← PARALLEL
        → qa      (after: [developer])   ← PARALLEL
          → quality-gate (after: [reviewer, qa], runs lint/test in VM)
            → committer (after: [quality-gate])
              → Engine: all required done → WorkflowCompleted
```
Set `RICK_DISABLE_QUALITY_GATE=1` to skip quality-gate — committer depends directly on `[reviewer, qa]`. Use on machines without VM support.

**PR Review workflow** (`pr-review`): Full PR review pipeline — clones repo, fetches Jira context, runs three parallel reviewers, consolidates findings into a single PR comment, and cleans up the workspace.
```
WorkflowStarted
  → pr-workspace (isolated clone of repo at PR branch)
    → pr-jira-context (extracts ticket ID from PR title/body/branch, fetches ticket from Jira)
      → architect (after: [pr-jira-context])   ← PARALLEL
      → reviewer  (after: [pr-jira-context])   ← PARALLEL
      → qa        (after: [pr-jira-context])    ← PARALLEL
        → pr-consolidator (after: [architect, reviewer, qa])
            → AI merges 3 reviews into single consolidated comment
            → posts to PR via `gh pr comment`
          → pr-cleanup (after: [pr-consolidator])
            → removes isolated workspace directory
              → WorkflowCompleted
```
Trigger: MCP `rick_run_workflow` with `dag=pr-review`, `source=gh:owner/repo#N`. Requires `JIRA_URL`, `JIRA_EMAIL`, `JIRA_TOKEN` env vars for Jira context enrichment (non-fatal if missing). Workspace uses `$RICK_REPOS_PATH` to locate repos.

**Jira dev workflow** (`jira-dev`): Full development pipeline with Jira ticket context and isolated workspace.
```
WorkflowStarted
  → jira-context (fetches Jira ticket, resolves repo from labels/components)
    → workspace (after: [jira-context], clones repo, creates branch)
      → context-snapshot (after: [workspace], captures codebase state)
        → researcher (after: [context-snapshot])
          → architect (after: [researcher])
            → developer (after: [architect], retries on FeedbackGenerated)
              → reviewer (after: [developer])  ← PARALLEL
              → qa      (after: [developer])   ← PARALLEL
                → quality-gate (after: [reviewer, qa], runs lint/test in VM)
                  → committer (after: [quality-gate])
                    → Engine: all required done → WorkflowCompleted
```
Trigger: MCP `rick_run_workflow` with `dag=jira-dev`, `ticket=PROJ-123`. Repo resolved from Jira labels (`repo:name`) or first component. Workspace uses `$RICK_REPOS_PATH` to locate repos. Set `RICK_DISABLE_QUALITY_GATE=1` to skip quality-gate (see workspace-dev).

**BTU Planning workflow** (`plan-btu`): Technical planning from Confluence BTU docs. Two hint pauses for human review.
```
WorkflowStarted
  → confluence-reader (reads BTU from Confluence, parses sections)
    → codebase-researcher (parallel AI research via claude/gemini at $RICK_REPOS_PATH)
      → plan-architect (generates plan in Spanish)
        → HintEmitted ← PAUSES for human review
        → HintApproved → PersonaCompleted{plan-architect}
          → estimator (Fibonacci points per task, calibrated by SQLite history)
            → HintEmitted ← PAUSES for estimate review
            → HintApproved → PersonaCompleted{estimator}
              → confluence-writer (writes plan to Confluence after "🛠️ Plan Técnico" heading)
                → WorkflowCompleted
```
Trigger: MCP `rick_plan_btu` or `rick_run_workflow` with `dag=plan-btu`. Requires `CONFLUENCE_URL`, `CONFLUENCE_EMAIL`, `CONFLUENCE_TOKEN` env vars. Optional `RICK_REPOS_PATH` for codebase research, `ESTIMATION_DB` for calibrated estimates.

**Jira Planning workflow** (`plan-jira`): Reads Confluence page, AI generates structured project plan (tasks, risks, dependencies), creates Jira epic + tasks. One hint pause for plan review before Jira issue creation.
```
WorkflowStarted
  → page-reader (reads Confluence page via REST API)
    → project-manager (AI analysis: goal, tasks, risks, deps with Fibonacci points)
      → HintEmitted (confidence=0.5) ← PAUSES for human review
      → HintApproved → PersonaCompleted{project-manager}
        → jira-task-creator (creates Epic + Tasks in Jira, links dependencies)
          → WorkflowCompleted
```
Trigger: MCP `rick_run_workflow` with `dag=plan-jira`, `source=confluence:<page-id>`. Requires `JIRA_URL`, `JIRA_EMAIL`, `JIRA_TOKEN` and `CONFLUENCE_URL` env vars. Jira issues use ADF formatting (bold, bullet lists, headings). Task dependencies are linked via "Blocks" issue links.

**Task Creator workflow** (`task-creator`): Standalone — generates Jira epic + tasks from a plain text prompt without Confluence. Single handler, no hint pause. Designed for direct invocation from the agent UI.
```
WorkflowStarted
  → task-creator (AI generates plan from prompt → creates Jira epic + tasks)
    → WorkflowCompleted
```
Trigger: MCP `rick_run_workflow` with `dag=task-creator`, `prompt="..."`. Requires `JIRA_URL`, `JIRA_EMAIL`, `JIRA_TOKEN` env vars.

**Jira QA Steps workflow** (`jira-qa-steps`): Reads a Jira ticket, finds the associated PR, AI generates QA test scenarios tailored to repo type (backend/frontend/fullstack), and writes them back to Jira's QA Steps field.
```
WorkflowStarted
  → qa-context (fetches Jira ticket + finds PR + gets diff + detects repo type)
    → qa-analyzer (after: [qa-context], AI: generates QA test scenarios)
      → qa-jira-writer (after: [qa-analyzer], writes QA steps to Jira field)
        → WorkflowCompleted
```
Trigger: MCP `rick_run_workflow` with `dag=jira-qa-steps`, `ticket=PROJ-123`. Optionally `source=gh:owner/repo#N` or `repo=owner/repo`. Requires `JIRA_URL`, `JIRA_EMAIL`, `JIRA_TOKEN` env vars. Optional `JIRA_QA_STEPS_FIELD` (default: `customfield_10037`).

**Dynamic workflows**: External systems can register custom workflow definitions via gRPC (`RegisterWorkflowRequest`). A workflow def includes Required (completion manifest) and optionally Graph (DAG for dispatch ordering). Any combination of local and gRPC-connected handlers can be referenced. Built-in workflows: `develop-only`, `workspace-dev`, `pr-review`, `pr-feedback`, `jira-dev`, `ci-fix`, `plan-btu`, `plan-jira`, `task-creator`, `jira-qa-steps`.

### DAG Dispatch Model

Execution topology is defined in `WorkflowDef.Graph` (`internal/engine/workflow_def.go`):
- `Graph`: `map[string][]string` — handler → predecessors. Empty deps `[]string{}` = root (fires on WorkflowStarted). Handler not in Graph = not in this workflow.
- `RetriggeredBy`: `map[string][]event.Type` — handlers that re-fire on specific events (e.g., developer → FeedbackGenerated for feedback loops).
- `PhaseMap`: `map[string]string` — phase verb → handler name (e.g., "develop" → "developer"). Used by verdict resolution to map phase names back to persona handlers.

PersonaRunner computes subscriptions from Graph at startup via `resolveEventsFromDAG()`. On PersonaCompleted, it resolves the workflow via a `correlationID → workflowID` cache, looks up the Graph, and dispatches only handlers whose predecessors include the completing persona. Handlers not in the workflow's Graph are never dispatched for that correlation.

**DAG surgery**: `WithoutHandler(def, "handler-name")` removes a handler from `Required` and `Graph`, rewiring all dependents to inherit its predecessors. Used by `RICK_DISABLE_QUALITY_GATE` to strip quality-gate at workflow construction time.

**Fallback for gRPC handlers**: Handlers not in any Graph fall back to `TriggeredHandler.Trigger()` — the deprecated handler-declared triggers. This maintains backward compatibility for external handlers that register via gRPC.

### PersonaRunner

`PersonaRunner` (`internal/engine/persona_runner.go`) is the **sole dispatcher** for all persona handlers. Uses DAG-based dispatch with workflow-scoped handler resolution. Safety guards: self-trigger prevention, chain depth limiting (auto-scaled), width limiting (default: 10 concurrent), event dedup (10K entry bounded cache), join-gate dedup (fingerprint-based), graceful drain with timeout. Stores results under persona-scoped aggregates: `{correlationID}:persona:{handlerName}`.

**Workflow registration**: `RegisterWorkflow(def)` stores the DAG. Called at startup for built-in workflows and at runtime for gRPC-registered workflows.

**Correlation cache**: `subscribeWorkflowStarted()` populates `correlationID → workflowID` on every `workflow.started.*` event. Evicted on terminal events (completed/failed/cancelled). O(1) lookup for all subsequent dispatch decisions.

**Dispatch queue**: Per-(handler, correlation) priority queue serializes event processing per handler per workflow. Priority: OperatorGuidance(0) > FeedbackGenerated(10) > PersonaCompleted(20) > default(30). FIFO within same priority level.

**Before-hooks**: `WithBeforeHook("developer", "frontend-enricher")` injects additional join conditions at runtime. The hook personas are merged with DAG predecessors. PersonaRunner auto-subscribes the gated handler to `persona.completed` if not already subscribed via DAG.

### Hint System (Pre-Check)

Handlers that implement the `Hinter` interface get a two-phase dispatch: `Hint()` runs first (lightweight pre-check), then `Handle()` only fires after `HintApproved`. Handlers that don't implement `Hinter` execute immediately as before (opt-in, non-breaking).

**Flow**: PersonaRunner calls `Hint()` → handler returns `HintEmitted{confidence, plan, blockers}` → Engine auto-approves if confidence >= threshold and no blockers, otherwise pauses for operator review → `HintApproved` triggers full `Handle()` execution. `HintRejected{action: "skip"}` marks persona complete without running; `HintRejected{action: "fail"}` fails the workflow. `HintThreshold` on `WorkflowDef` controls auto-approve sensitivity (default: 0.7).

### Sentinel (Unhandled Event Detection)

`Sentinel` (`internal/engine/sentinel.go`) monitors the event bus via `SubscribeAll` for events that no handler is subscribed to process. Skips internal events (lifecycle, AI, feedback, hints, context — 30+ types). When an unhandled event is detected, emits `UnhandledEventDetected` with the original event's type, ID, correlation, and source. Catches misconfigured workflows and orphan events.

### gRPC Service Discovery

External systems register as handlers via bidirectional gRPC streams (`internal/grpchandler/`). The stream lifecycle IS the service discovery — opening registers, closing deregisters. `PersonaService.HandleStream` accepts connections; clients send `HandlerRegistration` (name, event_types, after_personas, before_hook_targets) as the first message. Rick dispatches events down the stream and waits for `HandlerResult` back. All safety guards remain in PersonaRunner — external handlers are pure event processors.

`CompositeDispatcher` routes: LocalDispatcher (built-in personas, in-process) → StreamDispatcher (external gRPC handlers) fallback. `PersonaRunner.RegisterHandler()` and `RegisterHook()` enable dynamic registration after `Start()`.

Reconnecting client: `Client.Run(ctx)` (`internal/grpchandler/client.go`) wraps the stream with exponential backoff (1s→30s cap). Re-registers automatically on each reconnect. External Go services import this instead of managing streams directly.

**Workflow notifications**: `NotificationBroker` (`internal/grpchandler/notification_broker.go`) pushes realtime `WorkflowNotification` messages to gRPC-connected clients when a workflow reaches a terminal state (completed, failed, cancelled). Uses a Watch/Unwatch model through the existing bidirectional stream — client sends `WatchRequest` with correlation IDs (empty = watch all), broker subscribes to terminal bus events, matches against watchers, builds a summary from projections (status, duration, tokens, per-phase timeline, verdicts), and pushes through the client's `sendCh`. Verdict enrichment: `VerdictProjection` accumulates all `VerdictRendered` events per correlation; the broker includes them as `repeated VerdictDetail` (phase, source_phase, outcome, summary, issues) in the notification. This eliminates the race between verdict dispatch and workflow completion — by the time `WorkflowCompleted` fires, all verdicts are already projected. Catch-up on watch: immediately checks projections for already-terminal workflows to handle the race between watch registration and event emission. Disconnect cleanup via `UnwatchAll`. Client-side: `ClientConfig.NotificationHandler` callback, `WatchAll`/`WatchCorrelations` config for auto-watch on connect, `WatchWorkflow()`/`UnwatchWorkflow()` for dynamic watching.

**Dynamic workflow registration**: External systems can register custom workflow definitions through the gRPC stream via `RegisterWorkflowRequest`. A workflow def is a completion manifest — `{workflow_id, required[], max_iterations, escalate_on_max_iter}`. The server calls `Engine.RegisterWorkflow` and returns `RegisterWorkflowResult` with `available_handlers` (currently registered, local or gRPC) and `missing_handlers` (may connect later). This allows external systems to compose workflows from any combination of local handlers, other gRPC handlers, or themselves — no Go code changes needed. Client-side: `Client.RegisterWorkflow(ctx, workflowID, required, opts...)`.

### PersonaCompleted / PersonaFailed

Core choreography events. Payload includes `Persona`, `TriggerEvent`, `TriggerID`, `Reactive`, `OutputRef` (event ID of AIResponseReceived — avoids duplicating large LLM output), `ChainDepth`, `DurationMS`.

### Engine (lifecycle only)

`WorkflowAggregate` (`internal/engine/aggregate.go`):
- `Apply(env)`: side-effect-free state rebuild from events
- `Decide(env)`: lifecycle decisions — WorkflowStarted emission, WorkflowCompleted detection, VerdictRendered → FeedbackGenerated, WorkflowResumed → re-trigger (bumps MaxIterations, re-emits FeedbackGenerated after operator escalation resume), iteration/budget enforcement, HintEmitted → auto-approve/pause, HintRejected → skip/fail

Engine subscribes to lifecycle events, loads aggregate, calls Decide, persists+publishes. **Zero dispatch logic.**

### Feedback Loops

`VerdictRendered{fail}` → aggregate emits `FeedbackGenerated` → developer re-triggers (subscribes to FeedbackGenerated) → PersonaCompleted{developer} → reviewer, qa fire again. Reactive handlers MUST be idempotent. Max iterations enforced by aggregate. Stale event guard (`FeedbackPending`) prevents premature re-tracking of cleared personas.

### Tag-Based Correlation Lookup

The Engine automatically indexes business keys from `WorkflowRequested` as tags: `source`, `workflow_id`, `ticket`, `repo`, `repo_branch`. External systems discover correlation IDs via `store.LoadByTag(ctx, "ticket", "PROJ-123")`. Multiple workflows per tag and multiple tags per workflow are supported. Tags are stored in the `event_tags` SQLite table.

### Key Interfaces

- **`handler.Handler`** (`internal/handler/handler.go`): `Name()`, `Subscribes()`, `Handle(ctx, env) → ([]Envelope, error)`. Optional interfaces: `Hinter` (two-phase hint/execute dispatch), `Phased` (custom phase name for verdict resolution), `LifecycleHook` (Init/Shutdown for resource management). `TriggeredHandler` (deprecated, `internal/handler/trigger.go`) — only gRPC proxy handlers implement it; PersonaRunner falls back to `Trigger()` for handlers not in any workflow Graph. `ErrIncomplete` sentinel: handler processed event but has more work — PersonaRunner persists result events but skips PersonaCompleted/PersonaFailed; handler re-triggers on future subscribed events.
- **`eventstore.Store`** (`internal/eventstore/store.go`): 14-method interface. SQLite with WAL, optimistic concurrency. `LoadByCorrelation` queries across all aggregates — critical for join condition checks. `SaveTags`/`LoadByTag` enable business-key lookup against the `event_tags` table.
- **`eventbus.Bus`** (`internal/eventbus/bus.go`): `Publish`, `Subscribe` (returns unsubscribe func), `SubscribeAll`. ChannelBus and OutboxBus. 7 middleware (Logging, Retry, CircuitBreaker, Recovery, Timeout, Metrics, Idempotency).
- **`engine.Dispatcher`** (`internal/engine/dispatcher.go`): Routes by handler name. `LocalDispatcher` wraps handler.Registry.
- **`grpchandler.StreamDispatcher`** (`internal/grpchandler/stream_dispatcher.go`): Implements `Dispatcher` for gRPC-connected handlers. `CompositeDispatcher` chains local + stream.
- **`grpchandler.Server`** (`internal/grpchandler/server.go`): gRPC `PersonaService` implementation. Manages stream lifecycle, proxy handler registration, dynamic hooks, watch/unwatch routing, dynamic workflow registration.
- **`grpchandler.NotificationBroker`** (`internal/grpchandler/notification_broker.go`): Bus subscriber that routes terminal workflow events to watching gRPC streams. Builds `WorkflowNotification` with summary data from projections.

### Backend System

`backend.Backend` (`internal/backend/`): Claude and Gemini via CLI subprocess with streaming NDJSON parsing. `persona.PromptBuilder` assembles prompts from embedded templates + event store context.

### Projections

Four read-model projections (`internal/projection/`): workflow status, token usage, phase timeline, verdict. Runner does catch-up from global event stream then live subscription. `TokenUsageProjection.ForWorkflow(correlationID)` aggregates tokens across all persona-scoped aggregates for a workflow. `VerdictProjection.ForWorkflow(correlationID)` accumulates all `VerdictRendered` events for a workflow — keeps all iterations (retries produce multiple verdicts per source phase). Both are used by `NotificationBroker` to enrich `WorkflowNotification`.

### MCP Server

`internal/mcp/`: JSON-RPC 2.0 over stdio/HTTP with 46 tools across 7 categories. Used by Claude Desktop/Cursor and the agent UI.

**Workflow tools** (16, `tools.go`): `rick_run_workflow`, `rick_workflow_status`, `rick_list_workflows`, `rick_list_events`, `rick_token_usage`, `rick_phase_timeline`, `rick_workflow_verdicts`, `rick_persona_output`, `rick_list_dead_letters`, `rick_cancel_workflow`, `rick_pause_workflow`, `rick_resume_workflow`, `rick_inject_guidance`, `rick_plan_btu`, `rick_approve_hint`, `rick_reject_hint`

**Job tools** (6, `tools_jobs.go`): `rick_consult` (one-shot AI advisory), `rick_run` (direct AI execution with tools), `rick_job_status`, `rick_job_output`, `rick_job_cancel`, `rick_jobs`. No events or workflows — spawns backend subprocess, returns job ID for async polling. `JobManager` (`job.go`) tracks in-memory with background reaper (>2h timeout).

**Workspace tools** (3, `tools_workspace.go`): `rick_workspace_setup` (isolated clone under `$RICK_REPOS_PATH`), `rick_workspace_cleanup` (safe deletion with `*-rick-ws-*` pattern guard), `rick_workspace_list` (scan `$RICK_REPOS_PATH` for isolated workspaces).



**Jira tools** (10, `tools_jira.go`): `rick_jira_read`, `rick_jira_write`, `rick_jira_transition`, `rick_jira_comment`, `rick_jira_epic_issues`, `rick_jira_search`, `rick_jira_link`, `rick_jira_delete_link`, `rick_jira_set_microservice`, `rick_jira_pr_links`. Requires `JIRA_URL`, `JIRA_EMAIL`, `JIRA_TOKEN` env vars.

**Wave tools** (4, `tools_wave.go`): `rick_wave_plan` (topological sort of epic children into parallel waves), `rick_wave_launch` (batch-start `jira-dev` workflows per wave), `rick_wave_status` (monitor wave progress via tag lookup), `rick_wave_cleanup` (remove wave workspaces).

**Observability tools** (6, `tools_observability.go`): `rick_search_workflows` (find by ticket/source/repo tag), `rick_retry_workflow` (restart failed from checkpoint), `rick_workflow_output` (consolidated all-phase output), `rick_diff` (git diff from workspace), `rick_create_pr` (push + gh pr create), `rick_project_sync` (Mermaid dependency diagram from epic).

**Confluence tools** (2, `tools_confluence.go`): `rick_confluence_read`, `rick_confluence_write`. Requires `CONFLUENCE_URL`, `CONFLUENCE_EMAIL`, `CONFLUENCE_TOKEN` env vars.

## External System Integration Guide

External systems connect to Rick via a bidirectional gRPC stream. The Go client (`internal/grpchandler/client.go`) handles reconnection, re-registration, and protocol details. Any language with gRPC support can connect using the proto definition directly.

### Go Client Quick Start

```go
import (
    "github.com/marconn/rick-event-driven-development/internal/grpchandler"
    pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
)

conn, _ := grpc.NewClient("localhost:59077",
    grpc.WithTransportCredentials(insecure.NewCredentials()))
defer conn.Close()

client := grpchandler.NewClient(conn, grpchandler.ClientConfig{
    // --- Identity & Triggers ---
    Name:          "security-scanner",                          // unique handler name
    EventTypes:    []string{"persona.completed"},               // events to subscribe to
    AfterPersonas: []string{"developer"},                       // join: fire after developer completes
    // AfterPersonas: []string{"reviewer", "qa"},               // multi-join: fire after BOTH complete

    // --- Before-Hooks (optional) ---
    BeforeHookTargets: []string{"committer"},                   // gate committer until this handler completes

    // --- Handler ---
    Handler: func(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
        // Process the event, return result events.
        // Return nil, nil for no-op (PersonaCompleted still emitted).
        // Return nil, err to emit PersonaFailed.
        return []event.Envelope{
            event.New("context.enrichment", 1, enrichmentPayload),
        }, nil
    },

    // --- Hint Handler (optional, two-phase dispatch) ---
    HintHandler: func(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
        // Lightweight pre-check. Return HintEmitted event.
        // If nil, falls back to Handler for hint dispatches.
        return []event.Envelope{
            event.New("hint.emitted", 1, hintPayload),
        }, nil
    },

    // --- Workflow Notifications (optional) ---
    NotificationHandler: func(ctx context.Context, notif *pb.WorkflowNotification) {
        fmt.Printf("Workflow %s: %s\n", notif.CorrelationId, notif.Status)
    },
    WatchAll: true,                                             // receive all workflow completions
    // WatchCorrelations: []string{"wf-123"},                   // or watch specific workflows

    // --- Reconnection ---
    Logger:     slog.Default(),
    MaxRetries: 0,                                              // 0 = unlimited
    BaseDelay:  1 * time.Second,                                // initial backoff
    MaxDelay:   30 * time.Second,                               // max backoff cap
})

// Run blocks until ctx is cancelled. Reconnects automatically.
err := client.Run(ctx)
```

### Connection Lifecycle

1. `Client.Run(ctx)` opens a gRPC stream to `PersonaService.HandleStream`
2. Sends `HandlerRegistration` with name, event_types, after_personas, before_hook_targets
3. Server responds with `RegistrationAck{status: "ok"}`
4. If `WatchAll` or `WatchCorrelations` is set, sends `WatchRequest` automatically
5. Server dispatches events via `DispatchRequest` when trigger conditions are met
6. Client calls `Handler` (or `HintHandler` for `hint_only` dispatches) and returns `HandlerResult`
7. On stream error: exponential backoff (1s→30s), re-registration, resume processing
8. On disconnect: server automatically cleans up hooks, watches, and bus subscriptions

### Capabilities

**Event Processing** — handle dispatched events and return result events:
```go
Handler: func(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
    // env.Type, env.CorrelationID, env.Payload — the triggering event
    // Return events to emit (enrichment, AI responses, etc.)
    return resultEvents, nil
}
```

**Event Injection** — push events into a running workflow:
```go
eventID, err := client.InjectEvent(ctx, correlationID, event.OperatorGuidance, payload)
```

**Dynamic Watch** — subscribe to workflow completions after connect:
```go
client.WatchWorkflow(ctx, "wf-123", "wf-456")
client.UnwatchWorkflow(ctx, "wf-123")
```

**Workflow Registration** — register custom workflow definitions:
```go
result, err := client.RegisterWorkflow(ctx, "ci-pipeline",
    []string{"security-scanner", "developer", "reviewer"},  // required handlers
    grpchandler.WithMaxIterations(2),
    grpchandler.WithEscalateOnMaxIter(),
)
// result.AvailableHandlers — already registered (local or gRPC)
// result.MissingHandlers  — may connect later
```

### Trigger Patterns

| Pattern | Config | Use Case |
|---------|--------|----------|
| Fire on workflow start | `EventTypes: ["workflow.started"]` | First phase, no dependencies |
| Fire after one persona | `EventTypes: ["persona.completed"], AfterPersonas: ["architect"]` | Sequential chain |
| Fire after multiple (join) | `AfterPersonas: ["reviewer", "qa"]` | Wait for parallel phases |
| Gate another persona | `BeforeHookTargets: ["developer"]` | Enrichment before execution |
| Fire on feedback | `EventTypes: ["persona.completed", "feedback.generated"]` | Retry loops |
| Producer-only (no events) | `EventTypes: []` | Inject events, watch workflows |

### Proto Reference (`internal/grpchandler/proto/handler.proto`)

**Client → Server** (`HandlerMessage` oneof):
- `HandlerRegistration` — first message, declares name + triggers + hooks
- `HandlerResult` — response to `DispatchRequest` with result events
- `Heartbeat` — keep-alive during idle periods
- `InjectEventRequest` — push event into a workflow
- `WatchRequest` — subscribe to workflow notifications
- `UnwatchRequest` — unsubscribe from notifications
- `RegisterWorkflowRequest` — register a custom workflow definition

**Server → Client** (`DispatchMessage` oneof):
- `RegistrationAck` — confirms registration
- `DispatchRequest` — event to process (includes `hint_only` flag)
- `InjectEventResult` — response to inject request
- `WorkflowNotification` — workflow terminal state push (status, tokens, phases, duration, verdicts)
- `RegisterWorkflowResult` — response to workflow registration (available/missing handlers)
- `DisplacedNotification` — sent when a new client registers with the same handler name, displacing this stream (fields: `handler`, `reason`)

### Example: Security Scanner

An external security scanner that gates the committer phase:

```go
client := grpchandler.NewClient(conn, grpchandler.ClientConfig{
    Name:              "security-scanner",
    EventTypes:        []string{"persona.completed"},
    AfterPersonas:     []string{"developer"},
    BeforeHookTargets: []string{"committer"},        // committer waits for this
    Handler: func(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
        // Run security scan on the developed code...
        // Return enrichment context for downstream personas.
        return []event.Envelope{
            event.New("context.enrichment", 1, scanResults),
        }, nil
    },
    WatchAll: true,
    NotificationHandler: func(_ context.Context, n *pb.WorkflowNotification) {
        log.Printf("workflow %s finished: %s", n.CorrelationId, n.Status)
    },
})
```

### Example: Custom Review Workflow

An external system registers a custom workflow using its own handlers + Rick's built-in ones:

```go
// 1. Connect and register as a handler.
client := grpchandler.NewClient(conn, grpchandler.ClientConfig{
    Name:       "compliance-checker",
    EventTypes: []string{"persona.completed"},
    AfterPersonas: []string{"pr-architect"},
    Handler:    complianceHandler,
})
go client.Run(ctx)
time.Sleep(200 * time.Millisecond) // wait for registration

// 2. Register a custom workflow referencing local + remote handlers.
result, _ := client.RegisterWorkflow(ctx, "compliance-review",
    []string{"pr-architect", "compliance-checker", "pr-qa"},
)
// pr-architect and pr-qa are local Rick handlers
// compliance-checker is this gRPC client
// result.AvailableHandlers = ["pr-architect", "compliance-checker", "pr-qa"]

// 3. Start a workflow by injecting WorkflowRequested.
client.InjectEvent(ctx, "wf-compliance-1", event.WorkflowRequested,
    event.MustMarshal(event.WorkflowRequestedPayload{
        Prompt:     "Review PR #456 for compliance",
        WorkflowID: "compliance-review",
    }),
)
```

### Server Startup

The `rick serve` command starts both HTTP (MCP) and gRPC listeners. **This is the primary execution mode** — the CLI `rick run` command is deprecated. All workflow execution goes through serve/MCP and the agent UI.

```bash
rick serve --addr :58077 --grpc-addr :59077 --db rick.db --backend claude
```

Serve mode defaults to `--yolo=true` (auto-approves AI backend tool permissions) since it runs headless.

**Environment variables** (set in `~/.config/rick/env` or shell):

| Variable | Effect |
|----------|--------|
| `RICK_DISABLE_QUALITY_GATE` | When non-empty, removes quality-gate from all workflow DAGs. Committer depends directly on `[reviewer, qa]`. Use on machines that lack Multipass/VM support. Affects `workspace-dev`, `jira-dev`, `ci-fix`. |
| `RICK_MAX_WORKFLOWS` | Maximum number of concurrently running workflows (default: `0` = unlimited). Excess `WorkflowRequested` events are queued and auto-started when a slot frees up. Useful for resource-constrained machines and wave launches. |
| `RICK_REPOS_PATH` | Root directory for isolated workspaces and repo clones. Required by workspace/wave tools. |
| `RICK_CLAUDE_BIN` | Path to claude CLI binary (default: `claude`). |
| `RICK_GEMINI_BIN` | Path to gemini CLI binary (default: `gemini`). |
| `RICK_MODEL` | Override default LLM model for AI backends. |
| `RICK_SERVER_URL` | Agent UI → rick-server connection URL. |

### Agent UI (rick-agent)

Desktop application built with **Wails v2 + Svelte 5**. Provides a chat interface backed by a Gemini ADK operator that calls Rick's MCP tools. The agent UI is located in `agent/`.

**Architecture**: Svelte frontend → Wails Go bindings → Gemini ADK operator → MCP HTTP → rick-server. The operator uses Google ADK with MCP toolset for automatic tool calling loops. Config loaded from `~/.config/rick/env` (`RICK_SERVER_URL`, `RICK_MODEL`, `GOOGLE_API_KEY`).

**Build**: `cd agent && wails build` produces `agent/build/bin/rick-agent`. Test: `cd agent && go test ./...`.

**Design**: Typora-inspired light theme — white backgrounds, Inter font (18px base), clean typography with generous line-height (1.75), minimal borders (`border-gray-200`), `github.css` syntax highlighting for code blocks. Status colors: blue (running), emerald (completed), red (failed), amber (paused), teal (hints). Dark send button (`bg-gray-800`) as the single high-contrast element. Autocomplete popup for slash commands with muted styling.

**Packaging**: `.deb` package at `deploy/rick-agent_<version>_amd64.deb`. Includes binary (`/usr/bin/rick-agent`), desktop entry (`/usr/share/applications/rick-agent.desktop`), and SVG icon (`/usr/share/icons/hicolor/scalable/apps/rick-agent.svg`). Build with `make package`, install with `make install-agent`. Source assets in `deploy/rick-agent.{desktop,svg}`.

**Three tabs**: Chat (operator interaction), Workflows (dashboard with actions), Events (real-time event stream).

**Slash commands** (client-side, instant, zero token cost — never reach the LLM):

| Command | Action |
|---------|--------|
| `/clear` | Clear chat history |
| `/help` | List available commands |
| `/status` | Check rick-server connectivity |
| `/reconnect` | Re-discover MCP tools |
| `/config` | Show model and server URL |
| `/workflows` | Quick-list all workflows |
| `/deadletters` | Check dead letter queue |
| `/model` | Show current AI model |
| `/remember [category] <text>` | Save a memory for future sessions |
| `/memories` | List all saved memories |
| `/forget <id>` | Delete a saved memory |
| `/cancel <id> [reason]` | Cancel a running workflow |
| `/pause <id> [reason]` | Pause a running workflow |
| `/resume <id> [reason]` | Resume a paused workflow |
| `/events [id] [limit]` | List recent events (global or per-workflow) |
| `/tokens <id>` | Show token usage breakdown |
| `/phases <id>` | Show phase timeline |
| `/verdicts <id>` | Show review verdicts |
| `/approve <id> <persona> [guidance]` | Approve a pending hint |
| `/reject <id> <persona> [reason]` | Reject a pending hint (skip) |
| `/guide <id> <message>` | Inject operator guidance |

**Operator Memory** (`~/.config/rick/memories.json`): Persistent memory system that survives across sessions. Memories are injected into the operator's first message per session (and re-injected when memories change mid-session). Categories: user, preference, environment, workflow, general. The operator's system instruction makes it memory-aware — it proactively suggests saving relevant context. Wails bindings: `SaveMemory(content, category)`, `ListMemories()`, `DeleteMemory(id)`, `SearchMemories(query)`.

**Design principle**: The agent UI only accesses Rick through MCP tools — no direct event store or bus access. All workflow operations go through the same MCP interface that Claude Desktop/Cursor use.

## Subdirectory Navigation

Every directory has a `CLAUDE.md` with a focused file map and the local rules. When working inside a subtree, **read the closest `CLAUDE.md` first** instead of grepping blind — they exist precisely so you can skip the discovery cost.

```
.
├── CLAUDE.md                       ← you are here (architecture overview)
├── cmd/                            → cmd/CLAUDE.md
│   └── rick/                       → cmd/rick/CLAUDE.md          (binary entry point)
├── internal/                       → internal/CLAUDE.md          (index of all packages, grouped by concern)
│   ├── adf/                        → internal/adf/CLAUDE.md
│   ├── backend/                    → internal/backend/CLAUDE.md
│   ├── cli/                        → internal/cli/CLAUDE.md      (run.go DEPRECATED)
│   ├── confluence/                 → internal/confluence/CLAUDE.md
│   ├── engine/                     → internal/engine/CLAUDE.md   (aggregate, PersonaRunner, DAG dispatch)
│   ├── estimation/                 → internal/estimation/CLAUDE.md
│   ├── event/                      → internal/event/CLAUDE.md    (envelope + event type catalog)
│   ├── eventbus/                   → internal/eventbus/CLAUDE.md
│   ├── eventstore/                 → internal/eventstore/CLAUDE.md
│   ├── github/                     → internal/github/CLAUDE.md
│   ├── grpchandler/                → internal/grpchandler/CLAUDE.md
│   │   └── proto/                  → internal/grpchandler/proto/CLAUDE.md
│   ├── handler/                    → internal/handler/CLAUDE.md  (every concrete persona handler)
│   ├── jira/                       → internal/jira/CLAUDE.md
│   ├── jiraplanner/                → internal/jiraplanner/CLAUDE.md
│   ├── jirapoller/                 → internal/jirapoller/CLAUDE.md
│   ├── mcp/                        → internal/mcp/CLAUDE.md      (46 tools)
│   ├── observe/                    → internal/observe/CLAUDE.md  (NB: not yet wired in)
│   ├── persona/                    → internal/persona/CLAUDE.md
│   │   ├── phases/                 → internal/persona/phases/CLAUDE.md   (per-phase user prompts)
│   │   └── prompts/                → internal/persona/prompts/CLAUDE.md  (per-persona system prompts)
│   ├── planning/                   → internal/planning/CLAUDE.md
│   ├── pluginstore/                → internal/pluginstore/CLAUDE.md
│   ├── projection/                 → internal/projection/CLAUDE.md
│   ├── source/                     → internal/source/CLAUDE.md
│   └── workspace/                  → internal/workspace/CLAUDE.md
├── agent/                          → agent/CLAUDE.md             (rick-agent Wails desktop app — separate Go module)
│   └── frontend/                   → agent/frontend/CLAUDE.md
│       └── src/                    → agent/frontend/src/CLAUDE.md
│           ├── lib/                → agent/frontend/src/lib/CLAUDE.md
│           ├── stores/             → agent/frontend/src/stores/CLAUDE.md
│           └── utils/              → agent/frontend/src/utils/CLAUDE.md
├── deploy/                         → deploy/CLAUDE.md
│   └── systemd-user/               → deploy/systemd-user/CLAUDE.md
└── docs/                           → docs/CLAUDE.md              (architecture deep-dive + event-bus integration)
```

**Auto-generated, do NOT edit and do NOT document**: `agent/frontend/wailsjs/`, `agent/frontend/src/wailsjs/`. Both are produced by `wails build` / `wails generate module`.

## Conventions

- All code in `internal/` — no public API exports
- Functional options pattern: `WithName()`, `WithLogger()`, `WithTimeout()`
- Sentinel errors: `ErrConcurrencyConflict`, `ErrHandlerNotFound`, `ErrIncomplete` (multi-cycle handler)
- Errors wrapped with context: `fmt.Errorf("engine: load aggregate: %w", err)`
- Tests use in-memory SQLite (`:memory:`) with `t.Helper()` and `t.Cleanup()`
- Go 1.24, deps: `google/uuid`, `modernc.org/sqlite` (pure-Go), `spf13/cobra`, `google.golang.org/grpc` + `protobuf` (service discovery)
- Handlers return events, never persist or publish directly — the caller owns atomicity
