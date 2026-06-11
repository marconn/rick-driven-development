# Bug: `jira-dev` workflows stuck in `requested`, never transition to `started`

**Reported:** 2026-05-06
**Severity:** P1 — DAG is unusable; ticket-driven dev pipeline blocked
**DAG affected:** `jira-dev` (other DAGs verified working in the same window)

## Symptom

A workflow created with `dag=jira-dev` is accepted by `mcp:run_workflow` and a `workflow.requested` event is persisted, but the engine never emits the corresponding `workflow.started.jira-dev` event. The workflow sits in `status=requested` indefinitely while sibling workflows on other DAGs progress normally.

## Reproduction

```
rick_run_workflow(
  prompt="<any>",
  dag="jira-dev",
  ticket="HULI-33782",
)
```

Returns `{ workflow_id: <uuid>, status: "started" }` (acknowledgement only). 7+ minutes later, `rick_workflow_inspect` (status panel) still reports:

```json
{
  "id": "da992d9c-6009-4f46-bee5-d0d010d678c3",
  "status": "requested",
  "workflow_id": "jira-dev",
  "version": 1,
  "tokens_used": 0,
  "completed_personas": {},
  "feedback_count": {}
}
```

`rick_list_events` for the workflow returns exactly one event:

```json
{
  "type": "workflow.requested",
  "version": 1,
  "timestamp": "2026-05-06T03:29:11Z",
  "source": "mcp:run_workflow"
}
```

There is no follow-up `workflow.started.jira-dev` from `engine:aggregate`. Compare to a healthy `pr-review` start (event id `1725a207-...`) where `workflow.started.pr-review` is emitted by `engine:aggregate` in the same second as the `workflow.requested`.

## Why this is engine-side, not a client problem

In the same time window:

| DAG | Workflow ID | Latest event timestamp |
|---|---|---|
| `github-dev` | `d37e48e0-3d49-498c-8c3f-ae6b611ab271` | 2026-05-06T03:32:45Z (`persona.tracked`) |
| `pr-feedback`, `ci-fix`, `github-dev` (×6 others) | various | actively progressing |

The engine, event bus, persona runner, and dispatcher are all healthy — events are being processed, just not for `jira-dev`.

There are **zero** other workflows in `requested` state across the whole tracker, so this is not queue contention.

## Population evidence — `jira-dev` is the common denominator

All `jira-dev` workflows currently in the tracker:

```
3  cancelled
1  completed
1  requested  ← this bug
0  running
0  paused
```

No `jira-dev` workflow is currently running. The pre-existing 3 cancellations may be earlier instances of the same defect — worth correlating their `workflow.requested` → `workflow.cancelled` event spans to see whether they ever emitted `workflow.started.jira-dev` or were cancelled out of `requested`.

## Suspected location

The `jira-dev` lifecycle starts at `WorkflowAggregate.Decide(env)` for `WorkflowRequested`, which should emit `WorkflowStarted{dag: "jira-dev"}` and trigger `PersonaRunner` to dispatch the root handler (`jira-context`, per `docs/CLAUDE.md` line 48).

Things to check:

1. Is `jira-dev` actually registered in the workflow registry at engine startup? A missing/typo'd registration would silently swallow the `WorkflowRequested` because the aggregate would have no DAG to start.
2. Does the dispatch path for `dag=jira-dev` differ from `workspace-dev`/`pr-review` in any way that could fail-silent (e.g. the `ticket`-resolution preflight that fetches the Jira ticket and resolves the repo from labels — if that handler errors before emitting `WorkflowStarted`, the workflow stays in `requested`)?
3. Is there a per-DAG concurrency cap that's been set to 0 or `nil`-checked incorrectly? `PersonaRunner` has self-trigger/chain-depth/width guards (default 10) per `CLAUDE.md` line 29 — verify none of these can return early without emitting `WorkflowStarted`.
4. Are there any silent panics in `engine:aggregate` for this aggregate ID? Recovery middleware should be catching them; check for `Recovery` middleware events on the aggregate.

## Recommended diagnostics for repro session

```bash
# After repro, dump the full event stream for the stuck aggregate:
rick events list --workflow-id <uuid> --limit 200

# Check the registry at startup:
grep -rn '"jira-dev"' internal/engine/ internal/grpchandler/

# Look for the Jira preflight that resolves repo from ticket labels —
# if this returns an error before emitting WorkflowStarted, the workflow
# silently stays in requested:
grep -rn 'jira.*context\|resolve.*repo' internal/handler/ internal/persona/
```

## Workaround for users

None reliable. `rick_workflow_control action=retry` cannot help — the workflow never started, so there is no failed phase to resume from. Users must `rick_workflow_control action=cancel` and either:

- Switch to `dag=workspace-dev` (loses ticket-context auto-fetch and repo resolution).
- Manually provision a workspace via `rick_workspace action=setup` and use `rick_run` instead.

## Why this matters

`jira-dev` is the documented default DAG for *any* ticket-driven development per the planning-workspace `CLAUDE.md` ("Iron Rule 1: Task updates must always be reflected on Jira tickets" + "Ticket → jira-dev" rule). With this DAG broken, the operator must either accept losing automatic ticket context, or fall back to manual workspace orchestration — defeating the point of the workflow tooling.

## Concrete repro artifact

Stuck workflow available for live inspection: `da992d9c-6009-4f46-bee5-d0d010d678c3` (`HULI-33782`, ticket-resolved repo `huli-api`). Do not cancel until diagnostics are captured.
