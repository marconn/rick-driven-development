# 0008 — Knowledge layer (tool-based retrieval, criticality)

- **Track:** Persona registry · **SP:** 8 *(split during grooming if needed: resolution vs negotiation)*
- **Status:** Blocked · **Depends on:** 0001, 0002, 0007 · **Blocks:** 0009
- **Spec:** [Section 3.4 — knowledge delivery (deferred eager)](../persona-extensibility-and-dispatch-redesign.md#34-knowledge-delivery-for-non-claude-backends--deferred); [Section 3.4.1 — knowledge criticality](../persona-extensibility-and-dispatch-redesign.md#341-knowledge-criticality--closing-the-silent-operation-mode-gap)

## Review corrections (verified against code — override body where conflicting)

- **F9 — `MCPConfig` is not plumbed today.** `AIHandler.Run` does **not** set
  `backend.Request.MCPConfig` (`ai.go:269`) — there is nothing "already passed"
  to fold into. This task must **thread** resolver output into
  `backend.Request.MCPConfig` and define ownership/merge semantics. This is new
  plumbing, not an extension.
- **F4/F5 — depend on the selection API.** `required`-knowledge pinning needs the
  capability-filtered `Select` from 0001/0002 to choose a capable concrete
  backend *before* `Run`; negotiating against the `RoundRobin` wrapper's
  aggregate capabilities is insufficient. Add 0001 (selection API) as a
  dependency alongside 0002 and 0007.

## Context

Attach per-repo **knowledge** packs to personas so the same identity can change
operating mode per repo/domain. Phase 1 delivers knowledge only on tool-capable
backends (progressive disclosure via a retrieval tool). For backends without tool
support, behavior is governed by **criticality**: `required` pins to a capable
backend or fails; `optional` degrades and emits a knowledge-gap signal. Eager
inlining is deferred until the gap signal (0003/0005) quantifies it.

## Scope

- **In:** per-repo knowledge pack resolution; a retrieval tool for capable
  backends; criticality policy (required pins/fails, optional degrades + signal).
- **Out:** eager inlining / RAG (deferred); the resolver/registry (0007).

## Files

- `internal/persona/knowledge.go` (new) + test.
- `internal/persona/resolver.go` — build the knowledge plan; negotiate via
  `backend.Capabilities()` (0002).
- `internal/handler/ai.go` — fold the retrieval tool into the `MCPConfig` already
  passed to capable backends.
- `internal/event/` — the `knowledge_unavailable` diagnostic.

## Implementation notes

- Pack resolution: `RICK_KNOWLEDGE_DIR` → `$XDG_CONFIG_HOME/rick/knowledge` →
  `$HOME/.config/rick/knowledge`, keyed `<owner>/<repo>` (mirrors the per-repo
  quality-manifest model).
- Capable backend ⇒ expose packs as a `retrieve_knowledge` tool.
- Not capable + `required` ⇒ intersect the rotation set
  (`RICK_REVIEW_BACKENDS`) with capable backends; empty ⇒ **fail dispatch** with a
  clear error. Warn at startup when a rotated reviewer declares `required`
  knowledge (it trades rotation for knowledge).
- Not capable + `optional` ⇒ run degraded + emit `knowledge_unavailable`.
- Flag `RICK_KNOWLEDGE_DIR` unset ⇒ no knowledge layer.

## Acceptance criteria

- [ ] Capable backend retrieves declared packs via the tool.
- [ ] `required` on a non-capable-only rotation fails dispatch with a clear
      message; mixed rotation pins to the capable member.
- [ ] `optional` on a non-capable backend runs and emits `knowledge_unavailable`.
- [ ] Flag unset ⇒ no behavior change.
- [ ] `make check` green.

## Tests

- Negotiation table test across capability × criticality.
- Empty-intersection failure test; pin-to-capable test.
- `knowledge_unavailable` emission test.

## Rollback

Unset `RICK_KNOWLEDGE_DIR`. Revertable.
