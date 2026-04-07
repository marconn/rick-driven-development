# agent/frontend/src

Svelte 5 source root for the rick-agent Wails v2 desktop app — three-tab UI (Chat, Workflows, Events) backed by Wails Go bindings to a Gemini ADK operator that calls Rick MCP tools.

## Layout
- `App.svelte` — root component; tab state (`chat | workflows | events`), `Ctrl+1/2/3` shortcuts, mounts NavBar + active tab view, kicks off minimal dashboard polling for the NavBar health pill
- `main.ts` — entry; imports `app.css`, mounts `App` into `#app` via Svelte 5 `mount()`
- `app.css` — Tailwind v4 import + Typora-inspired theme: 18px Inter base, thin scrollbars, `.markdown-content` prose styles, mermaid/code-block styling, slash-command autocomplete popup, animations (typing dots, status pulse, phase bars)
- `vite-env.d.ts` — Svelte + Vite client type references
- `lib/` — shared Svelte components (chat, workflow, events, layout) → see `lib/CLAUDE.md`
- `stores/` — Svelte 5 runes-based stores (chat, dashboard, events) → see `stores/CLAUDE.md`
- `utils/` — markdown rendering pipeline (marked + hljs + mermaid) → see `utils/CLAUDE.md`
- `wailsjs/` — auto-generated Wails Go bindings (`go/main/App`, `runtime/`) — DO NOT EDIT

## Conventions
- Svelte 5 with runes (`$state`, `$effect`); no legacy stores
- Tailwind utility classes; Inter 18px base; Typora-inspired light theme (white bg, generous line-height 1.75, minimal `border-gray-200`)
- Single high-contrast element across the UI: dark send button (`bg-gray-800`)
- Status colors: blue (running), emerald (completed), red (failed), amber (paused), teal (hints)
- Code highlighting via `github.css` (loaded in the markdown pipeline, see `utils/CLAUDE.md`)
- All Rick access flows through MCP tools via the operator — no direct event store / bus calls

## Related
- `../wailsjs` — second auto-generated bindings dir at the frontend root (yes, there are two `wailsjs/` dirs)
- `../../app.go` — Wails Go side; exposes operator + memory bindings consumed by `wailsjs/go/main/App`
- `~/.config/rick/env` — runtime config (`RICK_SERVER_URL`, `RICK_MODEL`, `GOOGLE_API_KEY`)
