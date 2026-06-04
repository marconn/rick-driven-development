# 0010 — Readiness projection (`WorkflowRuntimeState`)

- **Track:** Dispatch · **SP:** 8 *(split during grooming if needed: fold logic vs robustness)*
- **Status:** Blocked · **Depends on:** 0003 (data gate) · **Blocks:** 0011
- **Spec:** [Section 3.5 — dispatch projection](../persona-extensibility-and-dispatch-redesign.md#35-track-b--dispatch-projection); [Section 3.5.1 — durable vs ephemeral](../persona-extensibility-and-dispatch-redesign.md#351-event-contract-what-is-durable-vs-ephemeral-review-correction); [Section 3.5.2 — idempotency, poison pills, liveness](../persona-extensibility-and-dispatch-redesign.md#352-idempotency-poison-pills-and-updater-liveness-review-critical)

## Review corrections (verified against code — override body where conflicting)

- **F1 — snapshot the topology.** Readiness depends on the WorkflowDef
  (Graph/`RetriggeredBy`/`PartialReviewOnFailure`/consolidator), which is **not**
  in the durable log (`WorkflowStartedPayload` has only `Phases []string`).
  Persist a WorkflowDef/admission snapshot at `WorkflowStarted` and fold against
  it (same pattern the `WorkflowRetried` handler already uses for
  `InvalidatedPersonas`). Without this the projection is **not** reconstructable.
- **F2 — fold `WorkflowRetried`.** Add it to the input set; its
  `InvalidatedPersonas` clears completions after auto/rate-limit retry and
  barrier-sibling invalidation. Add a barrier-sibling-retry test.
- **F13 — `IsReady(requestingHandler, target)`.** Bypass is reader-relative
  (`isConsolidatorBypass`); store raw verdict/feedback state and apply bypass at
  read time. No static all-readers "blocked" phase.
- **F12 — define partial-review contract first.** Today resolver treats *any*
  failed persona as satisfied; aggregate absorbs *only* category reviewers.
  Decide the intended behavior (clarification task) before encoding it; add
  non-category failure tests.
- **F14 — quarantine at the runner/store boundary**, not only inside the
  projector (`catchUp` can fail before a projector sees the event).

## Context

Dispatch readiness is recomputed today by replaying the full event chain on every
attempt, with every new rule bolted onto one ~150-line function — the source of a
recurring silent-stall class. Replace it with a single-writer read model that is a
**pure fold over the existing durable events** (reconstructable, idempotent).
**Entry gate:** the dwell data (0003) must first identify the live stall class so
this redesign is data-driven.

This task builds the projection only; it is wired into dispatch in shadow mode by
0011.

## Scope

- **In:** the projection type + pure-fold apply; `IsReady(handler)` mirroring
  current readiness semantics; robustness (poison-pill quarantine, apply-lag
  liveness, rebuild-from-log).
- **Out:** wiring into the dispatcher (0011); deleting the legacy path (0013).

## Files

- `internal/projection/runtime_state.go` (new) + test.
- `internal/event/` — `ProjectionApplyFailed`, `JoinDivergence` diagnostics.

## Implementation notes

- `PersonaState{Phase, LastTriggerID, Verdict{Active,Advisory,Fingerprint},
  FeedbackCount, StaleSince}` keyed per correlation.
- Inputs are the **existing** durable events (workflow-started, persona-completed,
  persona-failed, verdict-rendered, feedback-generated). **No new readiness
  events.** `ready`/`running` are ephemeral and re-derived on restart.
- Apply is pure `(state, event) → state`, deduped by event ID/version ⇒
  replay-safe and rebuildable from the log.
- Reproduce current readiness carve-outs: advisory verdicts, consolidator bypass,
  partial-review absorption, stale handling — but as explicit transitions.
- Poison pill ⇒ skip + `ProjectionApplyFailed`, never panic the updater.
- Emit an apply-lag metric (event append → apply) for updater-liveness alerting.

## Acceptance criteria

- [ ] Projection reproduces legacy readiness across the full existing test corpus.
- [ ] Pure-fold replay is idempotent (counters/states don't drift on replay).
- [ ] Rebuild-from-log yields identical state (restart test).
- [ ] A malformed event is quarantined (diagnostic emitted, updater survives).
- [ ] Apply-lag metric emitted.
- [ ] `make check` green (incl. `-race`).

## Tests

- Equivalence vs legacy on existing dispatch fixtures.
- Idempotent-replay test; rebuild determinism test; poison-pill quarantine test.
- **Regression (write first):** the "stale-guard never clears" scenario — assert
  it surfaces as an observable `stale` state.

## Rollback

Projection unused until 0011 wires it behind a flag; revert the commit.
