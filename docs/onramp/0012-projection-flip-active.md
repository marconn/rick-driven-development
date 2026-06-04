# 0012 — Flip projection to active

- **Track:** Dispatch · **SP:** 2
- **Status:** Blocked · **Depends on:** 0011 · **Blocks:** 0013
- **Spec:** [Section 5 — rollout & rollback](../persona-extensibility-and-dispatch-redesign.md#5-rollout--rollback)

## Context

After shadow mode (0011) shows the projection agrees with the legacy path across
real traffic, promote it: `active` acts on the projection while the legacy path is
retained as the rollback target.

## Scope

- **In:** the `active` mode (act on projection; legacy retained as fallback);
  document the soak window and the divergence==0 entry gate.
- **Out:** removing legacy (0013).

## Files

- `internal/engine/persona_runner.go` / `workflow_resolver.go` — honor `active`.

## Entry gate (not code — a release gate)

- `JoinDivergence` == 0 across real traffic for a documented soak window, **or**
  every divergence explained and fixed.
- The apply-lag liveness alert (0010) is live.

## Acceptance criteria

- [ ] `active`: dispatch acts on the projection; legacy no longer authoritative
      but still present.
- [ ] No new strands after flip; the previously-silent stall class now surfaces as
      an observable state and pages.
- [ ] Rollback to `off`/`shadow` is seconds-fast and verified.
- [ ] `make check` green.

## Tests

- `active`-mode dispatch test (projection authoritative).
- Rollback test (`active` → `off` returns to legacy behavior).

## Rollback

`RICK_DISPATCH_PROJECTION=shadow` (or `off`) ⇒ legacy authoritative, seconds.
