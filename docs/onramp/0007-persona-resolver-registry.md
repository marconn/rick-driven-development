# 0007 — PersonaResolver + dual-source registry

- **Track:** Persona registry · **SP:** 8 *(split during grooming if needed: resolver vs registry-loading)*
- **Status:** Blocked · **Depends on:** 0006 · **Blocks:** 0008, 0009
- **Spec:** [Section 3.1 — three-layer model](../persona-extensibility-and-dispatch-redesign.md#31-the-three-layer-persona-model); [Section 4 — backward compatibility & migration](../persona-extensibility-and-dispatch-redesign.md#4-backward-compatibility--migration)

## Review corrections (verified against code — override body where conflicting)

- **F8 — `AIHandler` is not the only persona consumer.** `PRConsolidator` calls
  `registry.LoadSystemPrompt` directly and pins its own claude backend
  (`internal/handler/pr_consolidator.go`). Wiring only `AIHandler` means
  manifests/knowledge silently won't apply there. Either route both through a
  **shared persona-resolution path**, or explicitly scope `pr-consolidator` out
  of manifests for Phase 2 and say so. Audit for any other direct
  `LoadSystemPrompt` callers and treat them the same way.

## Context

Compose a persona's system prompt from its manifest (identity + ordered skill
fragments) and load manifest personas alongside the existing code-registered ones
**without recompile**. This is the L1 capability (prompt composition for existing
handlers). Handler-binding/safety fields continue to come from Go construction —
unchanged.

## Scope

- **In:** a resolver that composes `system prompt = identity + skill fragments`;
  a dual-source registry (code + manifest, manifest wins on name collision);
  wiring `AIHandler` to resolve via it.
- **Out:** knowledge resolution/negotiation (0008); creating *new* handlers from
  manifests (0014, L2); any dispatch/readiness change (hard boundary).

## Files

- `internal/persona/resolver.go` (new) + test.
- `internal/persona/persona.go` — extend `Registry` to load from
  `RICK_PERSONA_MANIFESTS_DIR` and merge (manifest wins).
- `internal/handler/ai.go` — call the resolver instead of bare
  `LoadSystemPrompt`. Handler-binding fields from `aiCfg` in
  `internal/handler/handlers.go` stay as-is.

## Implementation notes

- Resolve skill refs in declared order; a missing ref is a load-time error for
  that persona (not a silent skip).
- Flag `RICK_PERSONA_MANIFESTS_DIR` unset ⇒ code-only path, byte-for-byte current
  behavior.
- **Hard boundary:** no edits to `internal/engine/workflow_resolver.go` or
  `persona_runner.go` dispatch logic. The PR diff must contain none.

## Acceptance criteria

- [ ] A code-registered persona overridden by a same-name manifest uses the
      manifest (override test).
- [ ] Composed prompt = identity followed by skill fragments in declared order.
- [ ] Missing skill ref fails that persona loudly at load.
- [ ] Flag unset ⇒ current behavior unchanged (existing persona tests pass).
- [ ] PR diff touches no dispatch/readiness code.
- [ ] `make check` green.

## Tests

- Composition order test; override-precedence test; missing-ref failure test;
  flag-unset equivalence test.

## Rollback

Unset `RICK_PERSONA_MANIFESTS_DIR` ⇒ code-registered personas. Revertable.
