# agent/frontend/src/lib

Shared Svelte 5 components for the rick-agent desktop UI — chat, workflow dashboard, event stream, and modal panels styled with Typora-inspired light theme.

## Components

### Layout / chrome
- `NavBar.svelte` — top tab bar (chat/workflows/events) with health indicator and connection status
- `StatusBar.svelte` — secondary status row showing model, thinking spinner, connected dot
- `DeadLetterBanner.svelte` — red warning banner with dead letter count

### Chat
- `Chat.svelte` — message list container with auto-scroll, empty-state placeholder
- `Message.svelte` — single message renderer (user / assistant / tool / error roles), Markdown + Mermaid via `../utils/markdown`
- `Input.svelte` — textarea with slash-command autocomplete popup, dispatches `onSend`
- `ToolCall.svelte` — pill showing tool name with calling spinner or completed checkmark

### Workflow dashboard
- `WorkflowDashboard.svelte` — workflow list with polling lifecycle (`startPolling('full')`)
- `WorkflowCard.svelte` — collapsible card binding selected workflow detail/phases/tokens/verdicts
- `WorkflowDetail.svelte` — expanded detail view: persona grid, token bars, verdict summary
- `WorkflowActions.svelte` — pause/resume/cancel/inject-guidance buttons
- `PhaseTimeline.svelte` — horizontal bar chart of phases with hint/running/completed/failed colors
- `VerdictPanel.svelte` — sortable verdict list (fail-first), expand-on-click
- `OutcomeBanner.svelte` — banner with status/duration/token summary
- `PersonaOutputPanel.svelte` — fullscreen modal showing persona output (Markdown rendered)
- `HintReviewPanel.svelte` — modal for reviewing pending `HintEmitted` items, approve with optional guidance or reject with reason

### Events tab
- `EventStream.svelte` — outer container, manages `eventsStore` polling lifecycle
- `EventFilterBar.svelte` — category dropdown (lifecycle/persona/ai/feedback/...), workflow filter, autoscroll toggle
- `EventList.svelte` — virtualized scrolling list with jump-to-latest button
- `EventRow.svelte` — single row, expand-on-click, color-coded type via `eventColorClass`

## Conventions
- Svelte 5 runes: `$state`, `$derived`, `$derived.by`, `$effect`, `$props` (no legacy stores)
- Props destructured from `$props()` with TypeScript types
- Tailwind utility classes — design tokens: `bg-gray-50`, `border-gray-200`, `text-gray-400/500/700`, status colors `emerald` (ok), `red` (fail), `blue` (running), `amber` (paused), `teal` (hints)
- `font-mono` for event types and tool names; base text size `text-base`/`text-lg`
- Action handlers are async functions on the relevant store; components stay presentational
- Modals use `fixed inset-0 z-50` with Escape key handler, never custom focus traps

## Related
- `../stores/chat.svelte` — chat messages, slash commands, model, connection state
- `../stores/dashboard.svelte` — workflows, detail, phases, tokens, verdicts, hints, persona outputs
- `../stores/events.svelte` — event polling, filters, autoscroll, color mapping
- `../utils/markdown` — `renderMarkdown` + `renderMermaidIn` (github.css for code, Mermaid diagrams)
- `../../wailsjs/go/main` — generated Wails bindings for Go backend calls
