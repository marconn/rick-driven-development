# package handler

Defines the `Handler` plugin interface and the concrete handler implementations that workflows compose into DAGs. Handlers are dumb workers — they have no triggers, no join logic, no event subscriptions; PersonaRunner reads `WorkflowDef.Graph` and dispatches them.

## Core interfaces (`handler.go`, `trigger.go`)

- **`Handler`** — `Name() string`, `Subscribes() []event.Type`, `Handle(ctx, env) ([]Envelope, error)`. Handlers return events; the caller publishes/persists. Under DAG dispatch, `Subscribes()` typically returns `nil` (informational only — Graph drives wiring).
- **`Hinter`** — optional `Hint(ctx, env) ([]Envelope, error)` for two-phase dispatch. PersonaRunner runs `Hint()` first; full `Handle()` only fires after `HintApproved`.
- **`Phased`** — optional `Phase() string` when handler name differs from phase verb (e.g., handler `developer`, phase `develop`). Used for verdict→persona resolution.
- **`LifecycleHook`** — optional `Init() error` / `Shutdown() error` for handlers managing external resources. Registry calls them on register/unregister/`ShutdownAll`.
- **`TriggeredHandler`** (DEPRECATED) — only gRPC proxy handlers implement it. PersonaRunner falls back to `Trigger()` solely for handlers absent from every workflow Graph. Never add new uses.
- **`ErrIncomplete`** sentinel — handler processed an event but has more work; PersonaRunner persists result events but skips PersonaCompleted/PersonaFailed so the handler can re-trigger later.

## Registry (`registry.go`)

`Registry` is concurrent-safe with a `byEvent` index for O(1) lookup. `Register` rejects duplicate names and runs `LifecycleHook.Init` before commit (init failure → not registered). `Replace` swaps a handler atomically: new handler `Init` runs before mutation; old handler `Shutdown` is best-effort after the swap. `HandlersFor` returns a defensive copy. `ShutdownAll` joins errors so partial failures don't skip remaining handlers.

## Wiring (`handlers.go`)

`Deps` bundles all shared dependencies (`Backend`, `Store`, `Personas`, `Builder`, `Jira`, `Confluence`, `Estimation`, `MsMap`, `GitHub`, `PluginStore`, `Logger`, `WorkDir`, `Yolo`). Several deps are nil-tolerant when their env vars are unset (Jira, Confluence, GitHub, Estimation, MsMap, PluginStore).

`RegisterAll(reg, deps)` registers each handler exactly once. Workflow DAGs scope which handlers participate per workflow — there is no per-workflow handler duplication (no more `jira-developer` / `pr-reviewer` prefixes).

## Handler implementations (grouped by workflow)

### workspace-dev / jira-dev / ci-fix / develop-only
- `ai.go` — `AIHandler` (base for all AI personas: loads context from event store, builds prompts via `persona.PromptBuilder`, calls backend, emits `AIRequestSent` + `AIResponseReceived`). `PlainText` config skips JSON extraction.
- `review.go` — `ReviewHandler` wraps `AIHandler` for `reviewer` and `qa`. Parses `VERDICT: PASS/FAIL` plus issues, emits `VerdictRendered{TargetPhase}` so the engine can drive the feedback loop.
- `committer.go` — `CommitterHandler` wraps `AIHandler`. Pre-checks the workspace via `git status`/divergence; if no changes exist, short-circuits with `VerdictRendered{fail, phase=develop}` to force a developer retry instead of silently completing.
- `workspace.go` — provisions a git workspace from `WorkflowRequested` (+ optional `context.enrichment` from `jira-context`). Uses correlationID-derived suffix to prevent collisions. Errors out if `ticket` provided without `repo`.
- `context_snapshot.go` — non-AI; walks the workspace filesystem and git log to capture ground-truth codebase state (file tree, key files, schemas, recent commits) within size budgets. Feeds the developer prompt.
- `quality_gate.go` — runs `stack run --json` to execute `./run.sh lint` + `./run.sh test` inside a Multipass VM, parses JSON output, emits `VerdictRendered`. Stripped from DAGs by `RICK_DISABLE_QUALITY_GATE`.
- Personas registered through `AIHandler`: `researcher`, `architect`, `developer`, `feedback-analyzer`. `reviewer`/`qa` via `ReviewHandler`. `committer` via `CommitterHandler`.

### pr-review
- `pr_workspace.go` — fires on `workflow.started.pr-review`; parses `gh:owner/repo#N` Source, fetches PR branch via `gh`, calls `workspace.SetupWorkspace` in isolated mode.
- `pr_jira_context.go` — extracts Jira key from PR title/body/branch via regex, fetches the issue, emits `ContextEnrichment`. Missing ticket is non-fatal.
- `pr_consolidator.go` — joins on `pr-architect`/`pr-reviewer`/`pr-qa` (the three review personas reused from the AI handlers), calls AI to merge findings into one comment, posts via `gh pr comment`. Only handler in this DAG with an external side-effect.
- `pr_cleanup.go` — best-effort removal of the isolated workspace dir after consolidation.
- (`architect`, `reviewer`, `qa` themselves are the same `AIHandler`/`ReviewHandler` instances reused via DAG scoping.)

### plan-btu / plan-jira / task-creator
These handlers do **not** live in this package — they're defined in `internal/planning` (BTU flow: reader, researcher, architect, estimator, writer) and `internal/jiraplanner` (Jira flow: page-reader, project-manager, task-creator, standalone task-creator). `RegisterAll` constructs and registers them alongside the local handlers so they share one registry.

### jira-qa-steps
- `qa_context.go` — fetches Jira ticket details + PR diff (capped at 50KB), detects repo type. Fires on `workflow.started.jira-qa-steps`.
- `qa-analyzer` — registered via `AIHandler` with `PlainText: true` (no JSON parsing on output).
- `qa_jira_writer.go` — writes the analyzer's output to the Jira QA Steps custom field via ADF formatting.

### Cross-workflow / shared
- `jira_context.go` — `jira-context` handler used by `jira-dev`. Resolves repo from Jira labels (`repo:name`) or first component, emits `ContextEnrichment` consumed by `workspace`.
- `feedback-analyzer` (registered in `handlers.go`, uses base `AIHandler`) — used by `pr-feedback`/`ci-fix` flows.
- GitHub PR fetcher — registered conditionally as a before-hook for `feedback-analyzer` when `d.GitHub != nil`. Lives in `internal/github`, not this package.

## Patterns

- Handlers return events, never publish or persist directly — the caller (PersonaRunner) owns atomicity.
- Use `ErrIncomplete` for multi-cycle handlers that need to wait for child events without emitting `PersonaCompleted`.
- Implement `Hinter` for any handler that should pause for human review (planning architect, estimator, project-manager all do this in their respective packages).
- Implement `Phased` when the handler name differs from the phase name used in `VerdictRendered` payloads.
- `Subscribes()` is informational only when the handler is in a workflow Graph — PersonaRunner ignores it and computes subscriptions from the DAG.
- Wrappers (`ReviewHandler`, `CommitterHandler`) keep `AIHandler` composable — never inherit, always wrap and delegate.
- Side effects in non-AI handlers (`gh pr comment`, `git push`, filesystem writes) must be idempotent and tolerate retries — the engine may re-dispatch on stale events.

## Related

- `../engine` — `PersonaRunner` is the sole dispatcher; `WorkflowDef.Graph` defines topology
- `../event` — `Envelope`, `Type`, payload structs (`VerdictPayload`, `ContextEnrichmentPayload`, etc.)
- `../eventstore` — `LoadByCorrelation` is how handlers reconstruct workflow context
- `../backend` — `claude` / `gemini` CLI subprocess wrappers
- `../persona` — `PromptBuilder` and persona registry for system prompts
- `../planning`, `../jiraplanner` — the BTU/Jira planning handlers registered alongside this package
- `../jira`, `../confluence`, `../github`, `../workspace` — external system clients
