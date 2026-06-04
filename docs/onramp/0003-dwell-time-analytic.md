# 0003 — Dwell-time projection

- **Track:** Telemetry · **SP:** 5
- **Status:** Blocked · **Depends on:** 0001, 0004 · **Blocks:** 0005, gates 0010
- **Spec:** [Section 6 — Observability](../persona-extensibility-and-dispatch-redesign.md#6-observability); [impl-plan Phase 1.1](../persona-extensibility-and-dispatch-impl-plan.md#phase-1--telemetry-parallel-prerequisite-for-phase-3)

## Review corrections (verified against code — override body where conflicting)

- **F3 — `DispatchDropped` is never published on the bus** (it is written to a
  separate aggregate; comment in `internal/engine/persona_runner.go`). A bus
  projection (`SubscribeAll`) cannot see live drops. Read the diagnostic
  **aggregate from the store** (or a store tailer with ordering guarantees), not
  the bus. *Or* publish the diagnostic event — decide explicitly.
- **F-agy5 — clock-start from `WorkflowStarted`/prior `PersonaCompleted`**, not
  `DispatchDropped`: a handler waiting on its *first* predecessor emits no drop
  event, so drop-based dwell undercounts the longest waits.
- **F17 — register in all entrypoints** (`serve.go` **and** `mcp.go`), or scope
  the epic explicitly to `rick serve`.

## Context

Dispatch stalls have been fixed reactively. Before redesigning dispatch
(the projection track), we need to know *which* stall class actually strands
workflows. This task builds a read-only projection over the **existing** event
log that measures how long handlers sit blocked, by `drop_reason`. It changes
**no dispatch logic**. This dataset is the **projection go/no-go gate.**

## Scope

- **In:** a projection that consumes `DispatchDropped` (already shipped) +
  `PersonaCompleted`/`PersonaFailed` + `DispatchStarted` (0004), and exposes
  `dispatch_dwell_seconds{state,persona}`.
- **Out:** changing dispatch; emitting new dispatch events.

## Files

- `internal/projection/dwell.go` (new) + `dwell_test.go`.
- `internal/cli/serve.go` — register in the projection runner
  (`projRunner.Register(...)`).
- Reference existing projections in `internal/projection/` for the pattern
  (status, token usage, timeline, verdicts).

## Implementation notes

- Dwell-in-blocked-state = time between a `DispatchDropped{drop_reason=
  join_unsatisfied|pending_feedback|...}` for a `(correlation, persona)` and the
  subsequent `PersonaCompleted`/`PersonaFailed` (or terminal) for that pair.
- Execution duration = `DispatchStarted` → `PersonaCompleted/Failed`.
- Use the resolved backend name from 0001 when bucketing review-phase handlers.
- Pure read model: rebuildable from the log; no writes back to the bus.

## Acceptance criteria

- [ ] Histogram `dispatch_dwell_seconds{state,persona}` populated from real
      traffic.
- [ ] A dashboard/SQL shows dwell distribution by `drop_reason` (document the SQL
      in the style of `reference_synchronous_feedback_observability`).
- [ ] Projection rebuildable from the event log (restart test).
- [ ] `make check` green.

## Tests

- Feed a synthetic correlation (dropped → later completed) and assert the
  computed dwell.
- Rebuild-from-log determinism test.

## Rollback

Read-only projection; drop the `serve.go` registration to disable.
