# 0006 — Manifest schema + validator

- **Track:** Persona registry · **SP:** 5
- **Status:** Ready · **Depends on:** — · **Blocks:** 0007
- **Spec:** [Section 3.2 — on-disk layout](../persona-extensibility-and-dispatch-redesign.md#32-on-disk-layout-skillmd-format-adopted-as-a-schema); [Section 3.2.1 — handler-binding contract (safety boundary)](../persona-extensibility-and-dispatch-redesign.md#321-handler-binding-contract-the-safety-boundary)

## Context

A persona becomes a manifest (`SKILL.md`: YAML frontmatter + markdown body). This
task defines the parse + validate layer. The safety-critical rule: a persona
manifest owns **prompt composition only**. It must **not** be able to set
runtime/safety knobs (permission-skipping, plain-text mode, verdict-bearing,
backend, timeout, target persona, phase/template, effort) — those stay code-owned
to prevent an operator manifest from silently disabling a safety invariant.

## Scope

- **In:** manifest types for personas and skills; strict startup validator;
  `schema_version` enforcement; loud rejection of forbidden/unknown keys.
- **Out:** composing prompts (0007); knowledge resolution (0008); loading into the
  registry (0007).

## Files

- `internal/persona/manifest.go` (new) + `manifest_test.go`.

## Implementation notes

- Persona manifest fields: `schema_version`, `name`, `description`, `identity`
  (file ref or inline body), `skills[]`, `knowledge[]{pack, load, criticality}`.
- Skill manifest fields: `name`, `description`, `tools[]` (optional allowlist),
  body.
- **Forbidden keys** (validator rejects, fails that one persona, not the process):
  `yolo`, `plaintext`, `verdict_bearing`, `backend`, `timeout`, `target`,
  `phase`, `effort`.
- Enforce a known `schema_version`; unknown version → reject with a clear message.

## Acceptance criteria

- [ ] Valid persona + skill manifests parse into typed structs.
- [ ] A manifest with any forbidden key is rejected with a message naming the key.
- [ ] Unknown/missing `schema_version` is rejected.
- [ ] A malformed manifest fails only itself; the loader reports and continues.
- [ ] `make check` green.

## Tests

- Golden parse of a valid manifest.
- Negative test per forbidden key.
- `schema_version` missing/unknown rejection.

## Rollback

New package, unused until 0007 wires it; revert the commit.
