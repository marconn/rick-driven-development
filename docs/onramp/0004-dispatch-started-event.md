# 0004 — `DispatchStarted` event (additive)

- **Track:** Telemetry · **SP:** 2
- **Status:** Ready · **Depends on:** — · **Blocks:** 0003
- **Spec:** [Section 3.5.1 — event contract: durable vs ephemeral](../persona-extensibility-and-dispatch-redesign.md#351-event-contract-what-is-durable-vs-ephemeral-review-correction)

## Review corrections (verified — override body where conflicting)

- **F16 — keep it off the synchronous dispatch hot path.** A synchronous store
  append at the top of `executeDispatch` adds I/O latency that stalls fast
  deterministic handlers (`workspace`, `quality-gate`). Emit best-effort /
  asynchronously (or only for non-AI handlers), and measure the added latency.
  Observability must never slow or fail a dispatch.

## Context

AI handlers emit `AIRequestStarted`, so their execution start is observable.
Non-AI (deterministic) handlers have no "started" signal, so dwell/duration
telemetry (0003) can't measure them. Add a single additive `DispatchStarted`
event emitted when the runner begins executing a handler.

**This is observability only — it must add no control flow to dispatch.** It does
not touch readiness/join logic (the hard boundary).

## Scope

- **In:** new event type; emit at the top of `executeDispatch`.
- **Out:** any readiness/join change; consuming the event (that's 0003).

## Files

- `internal/event/` — new `DispatchStarted` type + payload
  (`Persona`, `TriggerEvent`, `TriggerID`, `ChainDepth`).
- `internal/engine/persona_runner.go` — emit in `executeDispatch`, before
  `r.dispatcher.Dispatch(...)`. Append-only; no branching on it.

## Implementation notes

- Mirror the existing `AIRequestStarted` payload shape for consistency.
- Emit for **all** handlers (AI and non-AI); 0003 can dedup against
  `AIRequestStarted` if needed, or prefer `DispatchStarted` uniformly.
- Review this PR specifically for "no new control flow in `executeDispatch`."

## Acceptance criteria

- [ ] `DispatchStarted` emitted once per handler execution, carrying persona +
      trigger + chain depth.
- [ ] No change to dispatch ordering, guards, or join behavior (assert against
      existing dispatch tests — all still pass unchanged).
- [ ] `make check` green (incl. `-race`).

## Tests

- Assert a dispatched handler produces exactly one `DispatchStarted` with correct
  fields.
- Existing persona-runner/dispatch test suite passes byte-for-byte (no behavior
  drift).

## Rollback

Additive emit; revert the commit.
