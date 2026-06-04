# 0013 — Retire the legacy readiness path

- **Track:** Dispatch · **SP:** 5
- **Status:** Blocked · **Depends on:** 0012 · **Blocks:** —
- **Spec:** [Section 4 — backward compatibility & migration](../persona-extensibility-and-dispatch-redesign.md#4-backward-compatibility--migration); [Section 5 — rollout & rollback](../persona-extensibility-and-dispatch-redesign.md#5-rollout--rollback)

## Review corrections (verified — override body where conflicting)

- **F7 — rollback contradiction.** The epic promises seconds-fast rollback "at
  any point," but deleting legacy makes rollback a code revert. Resolve one of
  two ways: (a) keep legacy **dormant** (compiled, flag-reachable) one extra
  release before deletion, preserving seconds-fast rollback; or (b) explicitly
  scope the seconds-fast guarantee to *pre-retirement* and document that after
  this task rollback is a revert+redeploy. Pick (a) unless there's a strong
  reason; update the README rollback claim to match.

## Context

Once the projection has run `active` cleanly for a full release cycle, remove the
legacy full-chain replay so there is a single readiness source of truth. Separate
PR from the flip (0012) so the removal is independently reviewable and revertable.

## Scope

- **In:** delete the legacy replay path and its now-dead branches; simplify the
  flag to on/off (or remove `shadow`); update docs.
- **Out:** any behavior change beyond removing the dead path.

## Files

- `internal/engine/workflow_resolver.go` — remove the legacy readiness function.
- `internal/engine/persona_runner.go` — remove the shadow comparison.
- `docs/architecture.md` + this epic's Spec — update to reflect the projection as
  the sole readiness source.

## Entry gate (release gate)

- `active` clean for a full release cycle (no readiness-attributed incidents).

## Acceptance criteria

- [ ] Legacy readiness path removed; projection is the sole source.
- [ ] No dead code / unused branches remain.
- [ ] Docs updated (architecture + Spec).
- [ ] `make check` green (incl. `-race`).

## Tests

- Full dispatch suite passes against the projection-only path.

## Rollback

Revert the removal PR (the projection + flag remain intact underneath).
