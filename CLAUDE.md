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

### golangci-lint cache false positives (SA5011 on `t.Fatal` guards)

If `make check` reports `staticcheck SA5011: possible nil pointer dereference` on
test code where the deref is already guarded by `t.Fatal`/`t.Fatalf`
(e.g. `x := f(); if x == nil { t.Fatal(...) }; x.Field`), **it is a false
positive — do not "fix" the tests.** golangci-lint's analysis pipeline
intermittently drops staticcheck's terminating-call fact for `t.Fatal` from its
cache, so it reports nil derefs that standalone `staticcheck` does not
(golangci-lint #5979, #1768). The guards are correct. Clear the cache and re-run:

```bash
golangci-lint cache clean && golangci-lint run
```

Note `make check`'s default `max-same-issues: 3` hides most instances, so the
real count is far higher than what the first run prints — another tell that it's
the cache bug, not a genuine finding.

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
| `pr-review` | pr-workspace → pr-jira-context → (13 pr-category reviewers ∥) → pr-consolidator → pr-cleanup | `dag=pr-review, source=gh:owner/repo#N` |
| `pr-feedback` | reads pending PR review comments, dispatches fixes via developer, posts reply | post-review |
| `ci-fix` | reacts to CI failures on a PR branch | CI webhook |
| `plan-btu` | confluence-reader → codebase-researcher → plan-architect → ⏸hint → estimator → ⏸hint → confluence-writer | `rick_plan_btu` |
| `plan-jira` | page-reader → project-manager → ⏸hint → jira-task-creator | `dag=plan-jira, source=confluence:<id>` |
| `task-creator` | task-creator (single handler, no hint pause) | `dag=task-creator, prompt="..."` |
| `jira-qa-steps` | qa-context → qa-analyzer → qa-jira-writer | `dag=jira-qa-steps, ticket=PROJ-123` |
| `develop-only` | developer only | testing |

Parallel phases marked with `∥`. `⏸hint` = `HintEmitted` pause for operator review. `RICK_DISABLE_QUALITY_GATE=1` strips quality-gate from all DAGs (committer depends directly on reviewer+qa).

The `pr-review` category reviewers are: `pr-security`, `pr-concurrency`, `pr-error-handling`, `pr-observability`, `pr-api-contract`, `pr-idempotency`, `pr-testing`, `pr-integration`, `pr-performance`, `pr-data`, `pr-hygiene`, `pr-vendor-resilience`, `pr-docs-concordance`. Each has a narrow `## Your Domain (ONLY these)` scope in `internal/persona/prompts/pr-*.md` plus explicit boundary rules against the others. `pr-vendor-resilience` is polyglot (Go / JS-TS / PHP / network vendors) and self-scopes from the diff. `pr-docs-concordance` catches documentation/comment drift the diff introduces (a doc-comment that now lies about the changed code); it anchors findings on changed lines, so it does **not** catch stale references in files the PR doesn't touch — that cross-file gap is closed by the `pr-stale-reference` sweep (below).

The **`pr-stale-reference`** handler (non-AI, opt-in via `RICK_ENABLE_STALE_REF_SWEEP`) runs parallel to the reviewers and closes that gap deterministically: it extracts the exported symbols / consts / env vars a PR renamed or deleted, then `git grep`s the *unchanged* files for lingering references, keeping hits in doc files (`*.md`/`*.txt`/`*.rst`/`*.adoc`) and code comments. A grep hit is ground truth, so it bypasses the diff-grounding filter and feeds `pr-consolidator` a `ContextEnrichment{Kind:"stale-references"}` folded in as an **advisory, non-blocking** "Documentation Reference Check" section (never `REQUEST_CHANGES`). It does not catch value-drift (a const whose value changed but name didn't) — grep can't see that. See `internal/handler/pr_stale_reference.go`.

Outside `pr-review`, `reviewer` and `qa` run in parallel after `developer`. Boundary: `reviewer` owns code-as-written (correctness, concurrency, data integrity, observability, API stability); `qa` owns ship-readiness (test coverage + quality, flakiness, rollback, release-readiness). See `internal/persona/prompts/reviewer.md` and `qa.md` for the scope split enforced in-prompt.

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

`internal/mcp/` exposes 52 tools over JSON-RPC 2.0 (stdio/HTTP) in 7 categories: workflow (16), jobs (7), workspace (3), jira (12), wave (5), observability (7), confluence (2). Used by Claude Desktop/Cursor and the `agent/` Wails desktop UI. Full tool catalog: `internal/mcp/CLAUDE.md`. Agent UI architecture + slash commands: `agent/CLAUDE.md`.

**Execution mode**: `rick serve --addr :58077 --grpc-addr :59077 --db rick.db --backend claude` starts HTTP (MCP) + gRPC. This is the primary mode — `rick run` is deprecated. Serve defaults `--yolo=true` (headless auto-approve).

## Environment variables

Set in `~/.config/rick/env` or shell.

| Variable | Effect |
|---|---|
| `RICK_DISABLE_QUALITY_GATE` | Strip quality-gate from all DAGs. Use on VM-less machines. |
| `RICK_ENABLE_STALE_REF_SWEEP` | Opt-in (default-off). Injects the `pr-stale-reference` cross-file documentation sweep into `pr-review` via `WithStaleReferenceSweep` in `selectWorkflowDef`. Unset = not in the DAG. |
| `RICK_ENABLE_SESSION_RESUME` | Opt-in (default-off). On a feedback-driven re-run the **developer** resumes its prior backend CLI session (`exec resume` / `--resume`) and sends only the feedback delta instead of the full context prompt, cutting re-sent input tokens. Strictly gated (`AIHandler.resolveResume`): same backend that opened the session, no auto-retry rotation active, feedback present, single non-RoundRobin backend. Only codex/claude capture a resumable session id. Set to `1` to enable. |
| `RICK_QUALITY_MANIFESTS_DIR` | Operator-local directory holding per-repo quality manifests (`<owner>/<name>.yaml` or `<name>.yaml`). Defaults to `$XDG_CONFIG_HOME/rick/quality-manifests` then `$HOME/.config/rick/quality-manifests`. See `internal/handler/CLAUDE.md` for the manifest schema. |
| `RICK_ALLOW_HOST_RUNTIME` | Set to `1` to allow `runtime: host` quality manifests to execute commands directly on the host (no stack VM isolation). Default-deny — required for repos that cannot be stack-virtualized (e.g. huli's Go monorepo with no docker-compose). |
| `RICK_MAX_ITERATION` | Override every workflow's `MaxIterations` (positive int). Replaces per-workflow hardcoded values at registration time. Unset = baked-in defaults (3 for most code-producing workflows, 1-2 for plan/PR workflows). |
| `RICK_MAX_WORKFLOWS` | Concurrent workflow cap (0 = unlimited). Excess requests queue. |
| `RICK_REPOS_PATH` | Root for isolated workspaces + repo clones. Required by workspace/wave tools. |
| `RICK_CLAUDE_BIN` / `RICK_GEMINI_BIN` / `RICK_CODEX_BIN` / `RICK_ANTIGRAVITY_BIN` / `RICK_OPENCODE_BIN` | CLI binary paths (antigravity default `agy`, opencode default `opencode`). |
| `RICK_MODEL` | Override default LLM model. Set by the agent UI to choose its model; flows into `backend.Request.Model`. The `antigravity` backend ignores it — `agy` has no model flag and picks the model from the logged-in Antigravity session. The `opencode` backend forwards it only when provider-qualified (`provider/model`, e.g. `google/gemini-2.5-pro`); a bare name is dropped and opencode uses its own configured default (a bare name like `gemini-2.5-flash` is rejected by opencode, so forwarding it would crash the call). |
| `RICK_REVIEW_BACKENDS` | Comma-separated rotation for review-phase handlers (default `antigravity,claude`). Set to a subset to limit to installed CLIs, or one name to disable rotation. `codex`, `opencode`, and `gemini` are supported but must be opted in explicitly here — `codex` is reserved for exclusive on-call use and is no longer in the default rotation. |
| `RICK_BACKEND_TIMEOUT` | Wall-clock cap on developer-phase backend calls (default `20m`). `0` disables. |
| `RICK_REVIEW_BACKEND_TIMEOUT` | Wall-clock cap on review/commit/feedback-phase backend calls (default `15m`). `0` disables. |
| `RICK_BACKEND_STALL_TIMEOUT` | Idle (byte-level) watchdog: kills a claude/gemini/codex subprocess that emits no stdout for this long (default `6m`). `0` disables. Catches fully-silent wedges; blind to chatty ones. |
| `RICK_BACKEND_PROGRESS_TIMEOUT` | Completion-progress watchdog (claude-only, **default-off**): kills a subprocess that keeps emitting bytes but no assistant text for this long. Catches the tool-loop wedge the byte watchdog misses (incident 1a332d59). Set above any legitimate tool-only gap, below `RICK_BACKEND_TIMEOUT` (e.g. `15m`). |
| `RICK_PERSONA_MANIFESTS_DIR` | Opt-in (default-off). Operator-local dir of data-driven persona/skill manifests (`<dir>/personas/<name>/SKILL.md`, `<dir>/skills/<name>/SKILL.md`). A manifest persona's composed prompt (identity + ordered skill fragments) **wins** over the embedded/code prompt — override or recompose a persona with no recompile. Loaded through the shared `Registry.LoadSystemPrompt` path (applies to AIHandler, PRConsolidator, and the `rick_consult`/`rick_run` MCP jobs). A bad manifest fails only itself. Unset ⇒ embedded-only (byte-for-byte prior behavior). Manifests own **prompt composition only** — safety/runtime keys (`yolo`, `plaintext`, `verdict_bearing`, `backend`, `timeout`, `target`, `phase`, `effort`) are rejected loudly at load. |
| `RICK_KNOWLEDGE_DIR` | Opt-in (default-off). Operator-local, per-repo knowledge packs (`<dir>/<owner>/<repo>/<pack>/SKILL.md`) a persona manifest references with a `criticality`. Phase 1 delivers them only on **MCP-capable** backends (claude) as a `retrieve_knowledge` tool (progressive disclosure). On a non-capable backend: `required` knowledge **pins** the persona to a capable backend or **fails dispatch** loudly; `optional` runs degraded and emits a `knowledge_unavailable` signal (the deferred-eager-policy input). Eager inlining is deferred until that signal quantifies the gap. Unset ⇒ no knowledge layer. |
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
