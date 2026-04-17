# CLAUDE.md

Guidance for Claude Code when working in this repo.

## Build, Test, Lint

```bash
go build -o rick ./cmd/rick
go test ./...                                  # all packages
go test -race ./...                            # with race detector
go test -run TestNewDAGValid ./internal/engine # single test
golangci-lint run
# Pre-commit
golangci-lint run && go test ./...
```

## Architecture

Rick is an event-sourced AI workflow system built on **DAG-based orchestration**. All state changes are immutable events in SQLite. Execution topology lives in `WorkflowDef.Graph` — handlers are dumb workers (just `Name()` + `Handle()`, no triggers or join logic). `PersonaRunner` reads the DAG and dispatches accordingly, so the same handler can participate in multiple workflows without name prefixing.

### DAG dispatch model (`internal/engine/workflow_def.go`)

- `Graph: map[string][]string` — handler → predecessors. Empty deps = root (fires on `WorkflowStarted`). Absent from Graph = not in workflow.
- `RetriggeredBy: map[string][]event.Type` — re-fire triggers (e.g. developer on `FeedbackGenerated`).
- `PhaseMap: map[string]string` — phase verb → handler (used by verdict resolution).
- `WithoutHandler(def, name)` rewires dependents to inherit predecessors — used by `RICK_DISABLE_QUALITY_GATE`.
- Handlers not in any Graph fall back to `TriggeredHandler.Trigger()` (gRPC proxy handlers only).

`PersonaRunner` (`internal/engine/persona_runner.go`) is the **sole dispatcher**. Maintains a `correlationID → workflowID` cache, a per-(handler, correlation) priority queue (OperatorGuidance(0) > FeedbackGenerated(10) > PersonaCompleted(20) > default(30)), self-trigger/chain-depth/width guards (default 10 concurrent), event + join-gate dedup. Stores results under `{correlationID}:persona:{handlerName}`. `WithBeforeHook(target, hook)` injects runtime join conditions.

### Engine = lifecycle only

`WorkflowAggregate.Decide(env)` handles: `WorkflowStarted` emission, `WorkflowCompleted` detection, `VerdictRendered → FeedbackGenerated`, `WorkflowResumed` re-trigger, iteration/budget enforcement, `HintEmitted` auto-approve/pause, `HintRejected` skip/fail. **Zero dispatch logic.** Feedback loop: failed verdict → `FeedbackGenerated` → developer re-triggers → reviewer/qa re-run (reactive handlers must be idempotent).

### Hint system

Handlers implementing `Hinter` get two-phase dispatch: `Hint()` returns `HintEmitted{confidence, plan, blockers}`; engine auto-approves if confidence ≥ `HintThreshold` (default 0.7) and no blockers, else pauses for operator. `HintApproved` triggers `Handle()`. `HintRejected{action}` skips or fails.

### Sentinel + tag index

`Sentinel` watches the bus for events no handler consumes (excluding 30+ internal types) and emits `UnhandledEventDetected`. Engine auto-indexes `WorkflowRequested` business keys (`source`, `workflow_id`, `ticket`, `repo`, `repo_branch`) into the `event_tags` table; look up correlations via `store.LoadByTag(ctx, key, value)`.

## Built-in workflows

| ID | Flow (abbreviated) | Trigger |
|---|---|---|
| `workspace-dev` | workspace → context-snapshot → developer → (reviewer ∥ qa) → quality-gate → committer | local dev |
| `jira-dev` | jira-context → workspace → context-snapshot → researcher → architect → developer → (reviewer ∥ qa) → quality-gate → committer | `dag=jira-dev, ticket=PROJ-123` |
| `pr-review` | pr-workspace → pr-jira-context → (architect ∥ reviewer ∥ qa) → pr-consolidator → pr-cleanup | `dag=pr-review, source=gh:owner/repo#N` |
| `pr-feedback` | reads pending PR review comments, dispatches fixes via developer, posts summary | post-review |
| `ci-fix` | reacts to CI failures on a PR branch | CI webhook |
| `plan-btu` | confluence-reader → codebase-researcher → plan-architect → ⏸hint → estimator → ⏸hint → confluence-writer | `rick_plan_btu` |
| `plan-jira` | page-reader → project-manager → ⏸hint → jira-task-creator | `dag=plan-jira, source=confluence:<id>` |
| `task-creator` | task-creator (single handler, no hint pause) | `dag=task-creator, prompt="..."` |
| `jira-qa-steps` | qa-context → qa-analyzer → qa-jira-writer | `dag=jira-qa-steps, ticket=PROJ-123` |
| `develop-only` | developer only | testing |

Parallel phases marked with `∥`. `⏸hint` = `HintEmitted` pause for operator review. `RICK_DISABLE_QUALITY_GATE=1` strips quality-gate from all DAGs (committer depends directly on reviewer+qa).

External systems can register custom workflow definitions at runtime via gRPC (`RegisterWorkflowRequest`) — see `internal/grpchandler/CLAUDE.md`.

## Key interfaces

- **`handler.Handler`** (`internal/handler/`): `Name()`, `Subscribes()`, `Handle()`. Optional: `Hinter`, `Phased`, `LifecycleHook`. `ErrIncomplete` = processed but more work pending; PersonaRunner persists events but skips PersonaCompleted.
- **`eventstore.Store`** (`internal/eventstore/`): SQLite+WAL, optimistic concurrency, `LoadByCorrelation`, `SaveTags`/`LoadByTag`.
- **`eventbus.Bus`** (`internal/eventbus/`): Channel + Outbox variants. 7 middleware (Logging, Retry, CircuitBreaker, Recovery, Timeout, Metrics, Idempotency).
- **`engine.Dispatcher`** — `LocalDispatcher` (in-process) + `grpchandler.StreamDispatcher` (external), chained via `CompositeDispatcher`.
- **Projections** (`internal/projection/`): status, token usage, phase timeline, verdicts. `NotificationBroker` uses them to enrich terminal-state pushes over gRPC.
- **`PersonaCompleted/Failed`** payload: `Persona`, `TriggerEvent`, `TriggerID`, `Reactive`, `OutputRef` (event ID of AIResponseReceived), `ChainDepth`, `DurationMS`.

## External integration (gRPC)

External handlers register via bidirectional gRPC streams — stream lifecycle IS service discovery (open = register, close = deregister). The Go client (`internal/grpchandler/client.go`) handles reconnection with exponential backoff (1s→30s). Full integration guide, proto reference, trigger patterns, and examples: **see `internal/grpchandler/CLAUDE.md`**.

## MCP + agent UI

`internal/mcp/` exposes 48 tools over JSON-RPC 2.0 (stdio/HTTP) in 7 categories: workflow (16), jobs (6), workspace (3), jira (10), wave (5), observability (7), confluence (2). Used by Claude Desktop/Cursor and the `agent/` Wails desktop UI. Full tool catalog: `internal/mcp/CLAUDE.md`. Agent UI architecture + slash commands: `agent/CLAUDE.md`.

**Execution mode**: `rick serve --addr :58077 --grpc-addr :59077 --db rick.db --backend claude` starts HTTP (MCP) + gRPC. This is the primary mode — `rick run` is deprecated. Serve defaults `--yolo=true` (headless auto-approve).

## Environment variables

Set in `~/.config/rick/env` or shell.

| Variable | Effect |
|---|---|
| `RICK_DISABLE_QUALITY_GATE` | Strip quality-gate from all DAGs. Use on VM-less machines. |
| `RICK_MAX_WORKFLOWS` | Concurrent workflow cap (0 = unlimited). Excess requests queue. |
| `RICK_REPOS_PATH` | Root for isolated workspaces + repo clones. Required by workspace/wave tools. |
| `RICK_CLAUDE_BIN` / `RICK_GEMINI_BIN` | CLI binary paths. |
| `RICK_MODEL` | Override default LLM model. |
| `RICK_SERVER_URL` | Agent UI → rick-server URL. |
| `JIRA_URL`, `JIRA_EMAIL`, `JIRA_TOKEN` | Jira integration. |
| `CONFLUENCE_URL`, `CONFLUENCE_EMAIL`, `CONFLUENCE_TOKEN` | Confluence integration. |
| `ESTIMATION_DB` | Calibrated-estimator history DB (plan-btu). |
| `RICK_GITHUB_GRAPHQL` | Opt-in batched GraphQL for wave planner. |

## Subdirectory navigation

Every directory has a `CLAUDE.md` — **read the closest one first**. Top-level map: `cmd/rick/` (binary entry), `internal/` (index of all packages, grouped by concern), `agent/` (Wails desktop app, separate Go module), `deploy/` (packaging + systemd units), `docs/` (architecture deep-dive).

**Auto-generated, do NOT edit or document**: `agent/frontend/wailsjs/`, `agent/frontend/src/wailsjs/` (produced by `wails build` / `wails generate module`).

## Conventions

- All code in `internal/` — no public API exports.
- Functional options: `WithName()`, `WithLogger()`, `WithTimeout()`.
- Sentinel errors: `ErrConcurrencyConflict`, `ErrHandlerNotFound`, `ErrIncomplete`.
- Errors wrapped with package context: `fmt.Errorf("engine: load aggregate: %w", err)`.
- Tests use in-memory SQLite (`:memory:`) with `t.Helper()` and `t.Cleanup()`.
- Go 1.24; deps: `google/uuid`, `modernc.org/sqlite` (pure-Go), `spf13/cobra`, `google.golang.org/grpc` + `protobuf`.
- Handlers return events; never persist or publish directly — caller owns atomicity.
