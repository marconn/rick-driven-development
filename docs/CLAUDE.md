# docs/

- Long-form architecture and integration deep dives for the Rick event-driven orchestrator. Use as reference when the root `CLAUDE.md` short-form summary is insufficient.

## Documents

- `architecture.md` — System overview with Mermaid diagrams. Covers engine vs. PersonaRunner split, default workflow event flow, trigger config (events + AfterPersonas), feedback loop state machine, component responsibilities, workflow definitions, before-hooks, dispatch queue priorities, tag-based correlation lookup, and gRPC service discovery.
- `event-bus-integration.md` — How to integrate with the event bus. Covers the `event.Envelope` shape, full event catalog, real event traces from the test suite, subscription patterns (`Subscribe` / `SubscribeAll`), subscriber guarantees, dispatch queue, tag lookup, middleware stack, persona context sharing, `ChannelBus` vs `OutboxBus`, and a side-system quickstart.

## When to consult

- Designing a new persona handler or external gRPC handler → `architecture.md` (Trigger Configuration, Component Responsibilities, Workflow Definitions).
- Building a projection, side system, or external subscriber → `event-bus-integration.md` (How to Subscribe, Subscriber Guarantees, Quick Start).
- Debugging dispatch order, priorities, or join-gate behavior → both docs (Dispatch Queue sections).
- Understanding feedback retry semantics or `FeedbackGenerated` flow → `architecture.md` (Feedback Loop State Machine).
- Looking up a specific event type's payload or producers/consumers → `event-bus-integration.md` (Event Catalog).
- Adding tag-based workflow lookup from external systems → either doc (Tag-Based Correlation Lookup).

## Authoring conventions

- GitHub-flavored Markdown, `##` for top-level sections (no `#` beyond the title).
- Mermaid diagrams (`graph TB`, sequence, state) for architecture flows; ASCII boxes acceptable for simpler component layouts.
- Fenced code blocks tagged with language (`go`, `mermaid`, `text`).
- Real event traces preferred over invented examples — pull from the test suite when adding catalog entries.
- Keep diagrams in sync with the runtime: update both docs when handler topology, dispatch logic, or event types change.

## Related

- Root `../CLAUDE.md` — canonical short-form architecture; the source of truth for build/test/lint commands and high-level workflow DAG summaries. These docs are deep dives that complement, not replace, it.
- `internal/engine/`, `internal/grpchandler/`, `internal/eventbus/` — code referenced throughout both docs.
