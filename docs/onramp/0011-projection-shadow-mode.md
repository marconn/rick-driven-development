# 0011 — Projection shadow mode

- **Track:** Dispatch · **SP:** 5
- **Status:** Blocked · **Depends on:** 0010 · **Blocks:** 0012
- **Spec:** [Section 5 — rollout & rollback](../persona-extensibility-and-dispatch-redesign.md#5-rollout--rollback); [Section 3.5.2 — liveness](../persona-extensibility-and-dispatch-redesign.md#352-idempotency-poison-pills-and-updater-liveness-review-critical)

## Review corrections (verified against code — override body where conflicting)

- **F6 — do not compare in `wrap`.** The bus delivers typed subscribers before
  all-subscribers (`internal/eventbus/channel.go`), so the runner can evaluate
  readiness **before** the projection has applied the triggering event →
  constant false `JoinDivergence`. Compute divergence **inside the projection
  apply** (after the projection reaches the triggering event's watermark),
  comparing projection output vs a legacy evaluation at that same point — not on
  the dispatch hot path.

## Context

Trusting the projection on the hot path is the highest-risk change in the epic.
Ship it dark: in shadow mode, compute readiness from **both** the projection and
the legacy path, **act on legacy**, and emit a divergence diagnostic on mismatch.
This validates the projection (and surfaces updater stalls) for free before it is
ever trusted.

## Scope

- **In:** the `RICK_DISPATCH_PROJECTION=off|shadow|active` flag (**default
  `off`**); shadow comparison + `JoinDivergence` emission; legacy stays
  authoritative.
- **Out:** acting on the projection (0012); deleting legacy (0013).

## Files

- `internal/engine/persona_runner.go` — read-side comparison behind the flag.
- `internal/engine/workflow_resolver.go` — invoke both readiness sources for
  comparison (legacy remains the one acted on).

## Implementation notes

- Default `off` = strict current behavior (no projection compute, no diagnostics).
- `shadow` is an **intentional opt-in** with bounded compute + event-write cost;
  it is *not* current behavior — document that.
- On mismatch emit `JoinDivergence{persona, correlation, legacy, projection,
  reason}` for the dashboard (0003-style query).

## Acceptance criteria

- [ ] `off` (default): no projection compute, behavior identical to today.
- [ ] `shadow`: both computed, legacy acted on, `JoinDivergence` on mismatch.
- [ ] A divergence dashboard/query exists and reads zero on agreement.
- [ ] `make check` green (incl. `-race`).

## Tests

- Flag-matrix test (`off`/`shadow` behavior).
- Induced divergence emits `JoinDivergence`; agreement emits none.

## Rollback

`RICK_DISPATCH_PROJECTION=off` ⇒ legacy path, seconds.
