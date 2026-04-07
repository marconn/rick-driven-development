# agent/frontend/src/stores

Svelte 5 runes-based reactive stores for the rick-agent Wails desktop app — chat history, workflow dashboard polling, and event stream feed, all backed by Wails Go bindings.

## Stores

- `chat.svelte.ts` — chat state (`messages`, `thinking`, `connected`, `model`) and `sendMessage()`. Wires Wails `EventsOn('agent:event' | 'agent:connected' | 'agent:error')` to update the message list. Houses ALL slash command definitions in `buildCommands()` (clear, help, status, reconnect, config, workflows, deadletters, model, remember, memories, forget, cancel, pause, resume, events, tokens, phases, verdicts, approve, reject, guide). Slash commands are intercepted in `sendMessage()` and never reach the LLM. Exports `getCommandMeta()` for autocomplete and `chatStore` getter façade.
- `dashboard.svelte.ts` — workflow dashboard state: `workflows` (sorted: paused→running→failed→cancelled→completed), `selectedWorkflowId`, `workflowDetail`, `phases`, `tokens`, `verdicts`, `personaOutputs`, `outputModalPersona`, `deadLetterCount`, `serverReachable`, `activeCount`/`failedCount`/`pausedCount` ($derived). Polling: `startPolling('full' | 'minimal')` (3s/10s) drives `fetchWorkflows` + `fetchDeadLetters`; `startDetailPolling(id)` runs `fetchDetail` (parallel `WorkflowStatus` + `PhaseTimeline` + `TokenUsageForWorkflow` + `WorkflowVerdicts`) every 3s. Action mutators: `pauseWorkflow`, `cancelWorkflow`, `resumeWorkflow`, `approveHint`, `rejectHint`, `injectGuidance`, `fetchPersonaOutput`, `closeOutputModal`.
- `events.svelte.ts` — event stream feed: `events` (capped at 500), `filters` ({category, correlationId, search}), `autoScroll`, `lastSeenId`. `filteredEvents` is `$derived` via `applyFilters`. `startPolling()` polls `ListEvents('', 500)` every 2s and dedupes by ID. Helpers: `eventCategory()` (type→category mapping), exported `eventColorClass()` (Tailwind text color per category), `setFilter`, `setAutoScroll`, `clearEvents`.

## Patterns

- **Svelte 5 runes only** — no `writable()`. Module-level `let x = $state(...)` + `$derived(...)`. Each store exports a const object with `get` accessors so consumers see reactive values without losing encapsulation.
- **Wails bindings** — every backend call goes through `window.go.main.App.<Method>` with `// @ts-ignore` (no generated types imported here). `chat.svelte.ts` defines `requireBinding(method)` to guard against missing bindings.
- **Wails events** — `window.runtime.EventsOn(...)` is used only in `chat.svelte.ts` for streaming agent output. `dashboard` and `events` rely on interval polling.
- **No localStorage** — nothing is persisted client-side. Memories live in Go (`~/.config/rick/memories.json` via `SaveMemory`/`ListMemories`/`DeleteMemory`). Chat history is in-memory only.
- **ID resolution** — `chat.svelte.ts:resolveWorkflowID()` expands short prefixes against `ListWorkflows()` so slash commands accept truncated IDs.
- **Consumers** — `chatStore` used by `App.svelte`, `lib/Chat.svelte`, `lib/Input.svelte`, `lib/Message.svelte`, `lib/StatusBar.svelte`, `lib/NavBar.svelte`. `dashboardStore` used by `lib/WorkflowDashboard.svelte`, `WorkflowDetail`, `WorkflowCard`, `WorkflowActions`, `PhaseTimeline`, `VerdictPanel`, `PersonaOutputPanel`, `HintReviewPanel`, `OutcomeBanner`. `eventsStore` used by `lib/EventStream.svelte`, `EventList`, `EventRow`, `EventFilterBar`.
- **Lifecycle** — `chat.svelte.ts` self-initializes `initEvents()` on module load (defers until `DOMContentLoaded`). Dashboard/events stores require explicit `startPolling()` from the consuming component (typically in `onMount`/`$effect`) and `stopPolling()` on teardown.

## Related

- `../lib/` — all Svelte components consuming these stores
- `../wailsjs/go/main/` — generated Wails bindings (`App.ListWorkflows`, `App.SendMessage`, `App.SaveMemory`, etc.); referenced via `window.go.main.App` rather than direct import
- `../wailsjs/runtime/` — Wails `EventsOn` runtime, accessed as `window.runtime`
- `../App.svelte` — top-level shell that mounts Chat / Workflows / Events tabs
