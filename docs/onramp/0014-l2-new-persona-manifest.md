# 0014 — New AI persona from a manifest (L2, stretch)

- **Track:** Persona registry · **SP:** 8 *(split during grooming)*
- **Status:** Stretch · **Depends on:** 0009 · **Blocks:** —
- **Spec:** [Section 3.1.1 — three levels of extensibility](../persona-extensibility-and-dispatch-redesign.md#311-three-levels-of-extensibility-scope-honesty); [Section 3.2.1 — handler-binding contract](../persona-extensibility-and-dispatch-redesign.md#321-handler-binding-contract-the-safety-boundary)

## Review corrections (verified against code — override body where conflicting)

- **F11 — this does not add a genuinely new participant.** The handler registry
  rejects duplicate names and `Replace` only swaps the same name
  (`internal/handler/registry.go`); with no DAG node, a new handler is never
  dispatched. So L2 as written = **"replace/rebind an existing node from a
  manifest."** A truly new participant requires **workflow-topology manifests**
  (L3), which are out of scope. Either rename this task to "manifest-rebind an
  existing node" or fold it into the deferred L3 track. Adjust the acceptance
  criteria accordingly (it cannot claim "brand-new persona at an existing node
  with no recompile").

## Context

L1 (0006–0009) composes prompts for **existing** handlers. L2 stands up a *new*
AI persona purely from a manifest, reusing the generic AI handler, slotted into an
existing workflow node. It needs a **trusted handler-binding contract** to supply
the safety/runtime fields a new handler requires — which is exactly the surface
kept code-owned in L1, so this must not regress that boundary.

**Gate:** do not start until L1 (especially 0009) has run clean in production for
a sustained window.

## Scope

- **In:** a trusted handler-binding declaration (operator-local, validated more
  strictly than persona manifests) supplying runtime/safety fields; generic
  construction of an AI handler from (persona manifest + binding); registration
  against an existing workflow node.
- **Out:** new deterministic handlers; new workflow graph topology (L3 — separate
  track); editing dispatch/readiness logic.

## Files

- `internal/persona/` — handler-binding type + strict validator.
- `internal/handler/handlers.go` — generic AI-handler construction from manifest +
  binding.
- Registration wiring (`internal/cli/serve.go` / `mcp.go`) for the new participant
  at an existing node.

## Implementation notes

- The binding owns: effort, verdict-bearing, backend, timeout, target persona,
  template, permission-skipping, plain-text. It is **not** a persona manifest and
  is validated/trusted differently.
- Reuse the existing uniform AI-handler construction path; do not fork it.

## Acceptance criteria

- [ ] A brand-new AI persona defined by (manifest + binding) runs at an existing
      workflow node with **no recompile**.
- [ ] Binding validation rejects unsafe/incomplete configurations loudly.
- [ ] The L1 safety boundary is intact (persona manifests still cannot set safety
      knobs).
- [ ] `make check` green.

## Tests

- End-to-end: a fixture persona+binding dispatches and completes at a node.
- Binding validation negative tests.

## Rollback

Feature-flag the binding loader; unset ⇒ L1-only behavior.
