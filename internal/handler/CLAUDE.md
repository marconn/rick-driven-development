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

### Per-persona Claude `--effort`

`handlers.go` defines a `personaEffort` map that overrides the Claude CLI `--effort` reasoning level per handler name. The value flows: map → `AIHandlerConfig.Effort` → `backend.Request.Effort` → `claude.buildArgs` → `--effort <value>`. Empty/unmapped names fall through to `claude.go`'s `"high"` default, so adding a handler can never silently regress its reasoning budget. The map is Claude-specific — Gemini and Codex have no equivalent flag and ignore `Request.Effort`; if `qa`/`reviewer` run on the review backend (gemini by default) the effort field is set but no-ops, and only kicks in when the review rotation includes claude (`RICK_REVIEW_BACKENDS=claude`).

Current settings:

| Handler | Effort | Why |
|---|---|---|
| `architect` | `max` | planning cost dominates token cost — wrong plan blows the workflow |
| `researcher` | `xhigh` | same shape as architect, slightly less depth required |
| `qa` / `reviewer` | `high` | verdict-bearing reviewers must catch defects developer iterations miss |
| `developer` | `medium` | bounded per-iteration thinking; the feedback loop drives correctness |
| `committer` | `low` | mechanical step (commit message + push); no analysis needed |

All other handlers (the 12 `pr-*` category reviewers, `feedback-analyzer`, `pr-replier`, `pr-consolidator`, `qa-analyzer`, `develop-only`/`develop`-aliased handlers) currently inherit the `high` default. Touch the map when tuning, not the handler registrations.

## Handler implementations (grouped by workflow)

### workspace-dev / jira-dev / ci-fix / develop-only
- `ai.go` — `AIHandler` (base for all AI personas: loads context from event store, builds prompts via `persona.PromptBuilder`, calls backend, emits `AIRequestSent` + `AIResponseReceived`). `PlainText` config skips JSON extraction.
- `review.go` — `ReviewHandler` wraps `AIHandler` for `reviewer` and `qa`. Parses `VERDICT: PASS/FAIL` plus issues, emits `VerdictRendered{TargetPhase}` so the engine can drive the feedback loop.
- `committer.go` — `CommitterHandler` wraps `AIHandler`. Pre-checks the workspace via `git status`/divergence; if no changes exist, short-circuits with `VerdictRendered{fail, phase=develop}` to force a developer retry instead of silently completing.
- `workspace.go` — provisions a git workspace from `WorkflowRequested` (+ optional `context.enrichment` from `jira-context` / `github-context`). Uses correlationID-derived suffix on the workspace directory to prevent collisions. Errors out if `ticket` provided without `repo`, AND errors out when neither `ticket` nor `repo_branch` is set — the handler does NOT synthesize fallback branch names (the historical `rick/<corr8>` default was removed because it produced meaningless names on every PR). Operators supply the branch via `ticket=<key>` (jira-dev), `branch=<name>` at the MCP layer (workspace-dev / develop-only), or by routing through a DAG whose context handler emits a branch enrichment (`github-dev` → `issue-<N>`, `pr-*` → PR head branch, `ci-fix` → PR branch).
- `context_snapshot.go` — non-AI; walks the workspace filesystem and git log to capture ground-truth codebase state (file tree, key files, schemas, recent commits) within size budgets. Feeds the developer prompt.
- `quality_gate.go` — drives a per-repo quality contract (lint + test). Resolution order: operator-local `.yaml` manifest → legacy `run.sh` probe → legacy `Makefile check:` probe → advisory escalation. Output capture is via `stack run --json` (16 MiB scanner buffer; oversized envelopes surface as `parse_error`). Stripped from DAGs by `RICK_DISABLE_QUALITY_GATE`. Manifest schema + lookup details below.
- `quality_manifest.go` — manifest loader, repo-identity resolver (git origin URL → workspace-basename fallback), runtime gating helpers.
- Personas registered through `AIHandler`: `researcher`, `architect`, `developer`, `feedback-analyzer`. `reviewer`/`qa` via `ReviewHandler`. `committer` via `CommitterHandler`.

### pr-review
- `pr_workspace.go` — fires on `workflow.started.pr-review`; parses `gh:owner/repo#N` Source, fetches PR branch via `gh`, calls `workspace.SetupWorkspace` in isolated mode.
- `pr_jira_context.go` — extracts Jira key from PR title/body/branch via regex, fetches the issue, emits `ContextEnrichment`. Missing ticket is non-fatal.
- `pr_consolidator.go` — joins on every PR category-reviewer output (authoritative list: `prCategoryReviewerLabels` slice in this file), calls AI to emit a structured JSON review (summary, event, inline comments, unanchored findings), validates each inline comment against the live PR diff, then posts a single GitHub **pull request review** via `gh api POST repos/:o/:r/pulls/:n/reviews`. Inline comments that don't anchor cleanly fold into the review body. Falls back to `gh pr comment` only when the AI output isn't parseable JSON. Only handler in this DAG with an external side-effect.
- `pr_cleanup.go` — best-effort removal of the isolated workspace dir after consolidation.
- (`architect`, `reviewer`, `qa` themselves are the same `AIHandler`/`ReviewHandler` instances reused via DAG scoping.)

#### pr-review forensics signals (`review.go`)

`ReviewHandler` for `pr-category-review` handlers preserves three forensic signals on every invocation. These are write-only — the engine and consolidator ignore them. They exist for operator post-mortem.

- **`AIResponsePayload.OutputRaw`** (optional `json.RawMessage`) — the original LLM response captured **before** `groundPRCategoryReview` rewrites the canonical `Output` to the canned `"No grounded issues found in the changed lines for this review category."` string. Populated only when grounding actually mutated the output. Consumers MUST continue reading `Output`; `OutputRaw` is for SQL forensics (`json_extract(payload, '$.output_raw')`) and a future MCP tool.
- **`VerdictPayload.Source`** (`event.VerdictSource`) — classifies which parser path produced the verdict so operators can distinguish a real PASS from a defaulted PASS or a PASS demoted from FAIL by the grounding filter.
- **`VerdictGroundingSummary` event** (`internal/event` package) — emitted exactly once per `pr-category-review` invocation, recording how many issues the LLM produced, how many survived grounding (`GroundedCount`), how many of those were rescued via the file-scope path (`RescuedCount` — hallucinated line, but token found elsewhere in the file's changed lines; issue accepted with `Line=0` so the consolidator demotes it to an unanchored body bullet), and the `GroundingDropReason` taxonomy. `rescued_file_scope` appears in `DropReasons` for operator visibility even though it represents an accept. Empty-summary cases are intentional: the *absence* of a summary event would itself be a code-path bug.

##### Operator response table

| `verdict_source` value | What it means | Recommended operator action |
|---|---|---|
| `explicit_pass` | LLM emitted `VERDICT: PASS`. | Trust the verdict. |
| `explicit_fail` | LLM emitted `VERDICT: FAIL` and at least one issue survived grounding. | Read the inline findings; the consolidator already escalated. |
| `default_optimistic` | LLM emitted no `VERDICT:` line at all; `ParseVerdict` defaulted to PASS. | **Re-run that reviewer.** The LLM produced unparseable output — the verdict carries zero signal. Inspect `OutputRaw` to see what was actually returned. |
| `downgraded_no_grounded` | LLM emitted `VERDICT: FAIL` with N findings; all N were rejected by the diff-grounding filter. | Inspect `OutputRaw` to see the original findings, then check the matching `verdict.grounding.summary` for the `drop_reasons` breakdown. If `file_not_in_scope` dominates the LLM hallucinated; if `token_not_near_line` dominates the LLM cited valid lines but loose tokens — adjust the persona prompt or relax grounding. |
| `""` (unspecified) | Pre-PR event written before the field existed. | Ignore — back-compat zero value. |

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

## Quality-gate manifest

Quality-gate's lint/test commands are repo-specific. The contract is declared in an **operator-local** YAML file, NOT committed to the consumer repo (huli-api / ehr / huli / practice-api are owned by other teams and shouldn't carry rick-specific config).

### Lookup

Manifests live under `$RICK_QUALITY_MANIFESTS_DIR` (default `$XDG_CONFIG_HOME/rick/quality-manifests` then `$HOME/.config/rick/quality-manifests`). Repo identity is resolved from the workspace's git origin URL — `git@github.com:hulilabs/ehr.git` parses to `(owner=hulilabs, name=ehr)`. When the workspace has no origin, the basename minus `-rick-ws-<corrid>` is used (yields name only).

Two file paths are tried in order; the first found wins:

1. `<dir>/<owner>/<name>.yaml` — owner-scoped, preferred when both are known
2. `<dir>/<name>.yaml` — bare-name fallback (for the no-remote case or as a cross-org alias)

Absent manifest = fall through to legacy probing (`run.sh` / `Makefile check:`). A *malformed* manifest is NOT a fall-through — it produces an advisory `manifest_invalid` verdict so the operator must fix the file rather than silently get the heuristic.

### Schema

```yaml
runtime: stack          # stack | host (default: stack)
checks:
  - name: lint          # logical id; appears in summaries + qg-*-<name>.log
    label: "./run.sh lint"   # optional; defaults to space-joined command
    command: ["./run.sh", "lint"]
  - name: test
    command: ["bash", "-c", "./run.sh up && ./run.sh test"]
```

`command` is argv, not a shell line — wrap with `bash -c "..."` if you need shell features. Rick performs no expansion or quoting.

### Runtime kinds

- **`stack`** (default) — runs the command in a one-shot Multipass VM via `stack run --json --timeout <n> <wsPath> -- <command>`. The stack tool inlines stdout into the run envelope's `output` field and stderr (plus its own diagnostics) into `stderr`. Single envelopes can exceed 64 KiB easily — quality-gate uses a 16 MiB scanner buffer and surfaces `bufio.ErrTooLong` as `parse_error` rather than fall back to a partial parse.
- **`host`** — runs the command directly with `cwd=<workspace>`, no stack involved. Reserved for repos that cannot be stack-virtualized (e.g. monorepos with no docker-compose). Gated behind `RICK_ALLOW_HOST_RUNTIME=1` because a manifest is an arbitrary code execution vector — default-deny is the safety boundary. A missing executable produces an advisory `host_executable_missing` infra verdict, not a regression report.

### Common manifest shapes

- **Standard huli-style service** (`runtime: stack` + `./run.sh lint` + `./run.sh test`): the test target must call `up` itself, otherwise wrap with `bash -c "./run.sh up && ./run.sh test"` so a single VM hosts both setup and execution.
- **Go monorepo without docker-compose** (`runtime: host` + `make -C backend test-backend`): no stack, runs on the operator's host with their permissions; only place behind `RICK_ALLOW_HOST_RUNTIME=1`.
- **PHP repo with positional-arg test target**: `command: ["bash", "-c", "./run.sh up && ./run.sh test '' --testsuite=all"]` (or whatever runs the full suite — repo-team-specific).

### Failure modes

| Mode | Verdict | Operator action |
|---|---|---|
| Manifest absent + no probe match | advisory `no_run_sh` | Add a manifest or a `run.sh` / `make check` target |
| Manifest malformed | advisory `manifest_invalid` | Fix the YAML file |
| `runtime: host` + env unset | advisory `host_runtime_not_allowed` | Set `RICK_ALLOW_HOST_RUNTIME=1` in `~/.config/rick/env` |
| Stack envelope > 16 MiB | advisory `parse_error` | Genuine infra anomaly — investigate stack output |
| Host executable missing | advisory `host_executable_missing` | Install the binary or fix the manifest command |
| Inner command exited non-zero, stdout/stderr captured | regular fail (developer retriggers) | Read the verdict |

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
- `../backend` — `claude` / `gemini` / `codex` CLI subprocess wrappers; review-phase handlers use the `RoundRobin` rotation built via `backend.NewReviewBackend` (configurable through `RICK_REVIEW_BACKENDS`).
- `../persona` — `PromptBuilder` and persona registry for system prompts
- `../planning`, `../jiraplanner` — the BTU/Jira planning handlers registered alongside this package
- `../jira`, `../confluence`, `../github`, `../workspace` — external system clients
