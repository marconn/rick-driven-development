# 0002 — `backend.Capabilities()` interface

- **Track:** Foundations · **SP:** 3
- **Status:** Ready · **Depends on:** — · **Blocks:** 0008, 0015
- **Spec:** [Section 3.3 — resolution pipeline & capability negotiation](../persona-extensibility-and-dispatch-redesign.md#33-resolution-pipeline--capability-negotiation)

## Review corrections (verified against code — override body where conflicting)

- **F5 — aggregate intersection is wrong for the pinning use case.** A pure
  intersection makes `RoundRobin([codex,opencode,claude]).Capabilities().MCP =
  false` even though claude is present — which would make 0008's "pin to the
  capable member" impossible. Expose **per-candidate** capabilities + a
  capability-**filtered selection** (pairs with 0001's `Select`), not only an
  aggregate. Keep an aggregate accessor only if a caller genuinely needs
  "all members support X."
- **F10 — cover all wrappers.** `limitedBackend` (`internal/backend/limited.go`)
  also implements `Backend`; it must delegate `Capabilities()` (and any new
  selection API) to its inner. Add wrapper-delegation tests.

## Context

The resolver (0008) must negotiate knowledge delivery against what a backend can
do (tool support, native system prompt, etc.). Today there is no capability
surface, so the handler sends tool-only `Request` fields (`MCPConfig`, `Effort`)
to every backend; backends that lack them silently ignore them. An explicit
capability matrix fixes the no-op footgun and unblocks knowledge negotiation.

## Scope

- **In:** add `Capabilities()` to the `Backend` interface; implement on every
  backend; `RoundRobin` delegates (intersection or per-call — see notes).
- **Out:** changing what backends actually do; using the capabilities (that's
  0008).

## Files

- `internal/backend/backend.go` — `Capabilities` struct + interface method.
- `internal/backend/{claude,gemini,codex,opencode,antigravity}.go` — implement.
- `internal/backend/round_robin.go` — delegate.

## Implementation notes

```go
type Capabilities struct {
    MCP             bool // retrieve via tool calls (progressive disclosure)
    SystemPrompt    bool // native --system-prompt flag
    SessionResume   bool
    TokenAccounting bool
    ReasoningEffort bool
}
// Backend gains: Capabilities() Capabilities
```

Matrix to encode (from the backend research):

| Backend | MCP | SystemPrompt | SessionResume | TokenAccounting | ReasoningEffort |
|---|---|---|---|---|---|
| claude | ✓ | ✓ | ✓ | ✓ | ✓ |
| gemini | ✗ | ✗ | ✓ | ✗ | ✗ |
| codex | ✗ | ✗ | ✓ | ✓ | ✗ |
| opencode | ✗ | ✗ | ✓ | ✗ | ✗ |
| antigravity | ✗ | ✗ | ✓ | ✗ | ✗ |

- `RoundRobin.Capabilities()`: return the **intersection** (conservative —
  callers can't assume a capability a rotation member lacks). 0008 uses this to
  decide pin-vs-degrade for `required` knowledge.

## Acceptance criteria

- [ ] Every backend implements `Capabilities()`; values match the matrix.
- [ ] `RoundRobin.Capabilities()` returns the intersection of its members.
- [ ] No behavior change (nothing consumes it yet).
- [ ] `make check` green.

## Tests

- Table test asserting each backend's matrix row.
- `RoundRobin` intersection test (e.g. claude+codex ⇒ MCP=false).

## Rollback

Additive interface method; revert the commit.
