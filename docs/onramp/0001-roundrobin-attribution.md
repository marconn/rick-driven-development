# 0001 — RoundRobin backend attribution

- **Track:** Foundations · **SP:** 3
- **Status:** Ready · **Depends on:** — · **Blocks:** 0003
- **Spec:** [Section 10 — rejected-alternative note](../persona-extensibility-and-dispatch-redesign.md#10-rejected-alternative-migrate-to-the-claude-agent-sdk); [impl-plan Phase 0.1](../persona-extensibility-and-dispatch-impl-plan.md#phase-0--foundations--guardrails)

## Review corrections (verified against code — override body where conflicting)

- **F4 — attribution before `Run` is impossible without a selection API.**
  `AIRequestSent` (`ai.go:196`) and `AIRequestStarted` (`ai.go:254`) are emitted
  **before** `backend.Run`, but `RoundRobin` selects **inside** `Run`
  (`round_robin.go:54`), and `Response` has no backend field. Add an explicit
  `Select(ctx) Backend` (deterministic for the sticky/offset path reviewers use)
  and attribute all three events from it; `Run` reuses the same selection (guard
  the non-sticky atomic-counter path against double-advance). This `Select` API
  is also a dependency of task 0008 (required-knowledge pinning).

## Context

The rotation backend picks an inner backend per call, but its `Name()` returns
the composite `round-robin(codex,opencode,claude)`. The events `AIRequestSent` /
`AIResponseReceived` therefore record the composite, not the backend that
actually ran. Telemetry (0003) on review-phase handlers is unreadable until the
*chosen* backend is recorded. This is also a real observability gap today,
independent of this initiative.

## Scope

- **In:** surface the selected inner-backend name for each `RoundRobin.Run`, and
  record it on `AIRequestSent` / `AIResponseReceived`.
- **Out:** changing rotation/selection logic; sticky-key behavior.

## Files

- `internal/backend/round_robin.go` — expose the chosen backend per run.
- `internal/handler/ai.go` — the call site emitting `AIRequestSent` /
  `AIResponseReceived`; record the resolved backend name.
- (`internal/backend/sticky.go` — read-only reference for how the index is
  chosen.)

## Implementation notes

- `RoundRobin.Run` already selects an inner backend; return/propagate its
  `Name()` so the handler can attribute it. Prefer threading it through the
  `Response` (e.g. a `Backend` field) over a side channel — the handler emits the
  events from the `Response`.
- Keep `RoundRobin.Name()` returning the composite for registry/config use; the
  *attribution* is per-call, not the backend identity.

## Acceptance criteria

- [ ] A `pr-review` run using a rotation shows the concrete backend
      (e.g. `codex`) on each `AIRequestSent`/`AIResponseReceived`, not
      `round-robin(...)`.
- [ ] Non-rotated (single) backend behavior unchanged.
- [ ] `make check` green.

## Tests

- Unit test: a `RoundRobin` over two fakes, assert two successive `Run` calls
  attribute the two distinct inner names.
- Handler test: assert the emitted `AIResponseReceived` carries the resolved
  backend name.

## Rollback

Additive field + emit; revert the commit. No flag needed.
