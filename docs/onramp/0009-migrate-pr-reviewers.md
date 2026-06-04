# 0009 — Migrate the 13 category reviewers to manifests

- **Track:** Persona registry · **SP:** 5
- **Status:** Blocked · **Depends on:** 0007, 0008 · **Blocks:** 0014
- **Spec:** [Section 4 — backward compatibility & migration](../persona-extensibility-and-dispatch-redesign.md#4-backward-compatibility--migration)

## Review corrections (verified — override body where conflicting)

- **F15 — keep embedded prompts fully intact through Phase 2.** Do **not** gut
  the embedded `pr-*.md` while migrating: unsetting `RICK_PERSONA_MANIFESTS_DIR`
  as rollback would otherwise fall back to structurally-broken prompts. Remove
  the duplicated boilerplate only in a **later cleanup task**, after manifests are
  permanently locked in (post-active). Until then, deduplication is logical (the
  manifest path uses skills) while the Go fallback stays whole.

## Context

The 13 category reviewers (`pr-security`, `pr-concurrency`, `pr-error-handling`,
`pr-observability`, `pr-api-contract`, `pr-idempotency`, `pr-testing`,
`pr-integration`, `pr-performance`, `pr-data`, `pr-hygiene`,
`pr-vendor-resilience`, `pr-docs-concordance`) copy-paste shared boilerplate.
They are the first and lowest-risk consumer of manifests (review-only, one shared
phase template). Migrating them proves composition and deduplicates the
boilerplate. Safety knobs (plain-text mode, verdict-bearing) stay in code.

## Scope

- **In:** author shared skills for the duplicated boilerplate; convert each
  category reviewer to a manifest referencing those skills + its narrow domain
  identity.
- **Out:** changing reviewer behavior/boundaries; touching safety knobs;
  non-reviewer personas.

## Files

- `internal/persona/skills/` — shared skills (e.g. diff-grounding,
  domain-boundary, the common output contract).
- `internal/persona/personas/pr-*/SKILL.md` — one manifest per reviewer.
- The existing embedded `internal/persona/prompts/pr-*.md` remain as the override
  fallback until equivalence is proven, then are removed.

## Implementation notes

- Extract only the *truly shared* boilerplate into skills; keep each reviewer's
  `## Your Domain (ONLY these)` and cross-boundary rules in its identity.
- Do **not** alter the safety configuration in `internal/handler/handlers.go`
  (plain-text reviewers stay plain-text).

## Acceptance criteria

- [ ] Each migrated reviewer's composed prompt is a **superset** of its prior
      embedded prompt's domain rules (golden-file compare).
- [ ] Shared boilerplate exists once (as skills), not 13 times.
- [ ] Safety config unchanged (test asserts plain-text reviewers still plain-text).
- [ ] One live `pr-review` run is clean and produces equivalent category output.
- [ ] `make check` green.

## Tests

- Per-reviewer golden equivalence test.
- Safety-config assertion test.

## Rollback

Unset `RICK_PERSONA_MANIFESTS_DIR` ⇒ embedded `pr-*.md` prompts. Revertable.
