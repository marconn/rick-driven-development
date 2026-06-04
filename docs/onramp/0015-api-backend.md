# 0015 — API backend adapter (optional)

- **Track:** Backend · **SP:** 5
- **Status:** Optional · **Depends on:** 0002 · **Blocks:** —
- **Spec:** [Section 10 — rejected alternative + additive API backend](../persona-extensibility-and-dispatch-redesign.md#10-rejected-alternative-migrate-to-the-claude-agent-sdk)

## Context

The system drives models by shelling out to CLI subprocesses and parsing stdout —
fragile, and token accounting is unavailable on several backends. Adding an
**API-based** adapter (official Go client SDK) behind the existing `Backend`
interface gives a non-CLI path with real token accounting, slotting next to the
CLI backends in rotation. This is **additive, not a migration** — the multi-vendor
adapter and single-binary deploy are unchanged.

Not scheduled; listed for completeness.

## Scope

- **In:** a new backend implementing `Backend` (incl. `Capabilities()` from 0002)
  via the official Go client SDK; opt-in via the binary-path/rotation config.
- **Out:** removing or changing the CLI backends; adopting an external agent
  framework.

## Files

- `internal/backend/api_<vendor>.go` (new) + test.
- `internal/backend/factory.go` — register/select when configured.

## Implementation notes

- Implement `Run(ctx, Request) (*Response, error)` mapping `Request` →
  client-SDK call; populate `Response` incl. real token counts and stop reason.
- `Capabilities()`: tool support, token accounting true, etc.
- Honor the same timeouts/cancellation contract as the CLI backends.

## Acceptance criteria

- [ ] The adapter runs a request and returns output + real token counts + stop
      reason.
- [ ] Slots into rotation alongside CLI backends; opt-in only.
- [ ] CLI backends unaffected.
- [ ] `make check` green.

## Tests

- Adapter unit test against a faked client.
- Rotation includes/excludes it per config.

## Rollback

Opt-in; remove from rotation config or revert the commit.
