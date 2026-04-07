# agent/frontend/src/utils

Pure helper modules for the rick-agent Wails desktop app's Svelte 5 frontend.

## Modules

- `markdown.ts` — Markdown rendering pipeline using `marked` + `highlight.js` + `mermaid`.
  - Configures `marked` with GFM and line breaks enabled.
  - Custom `renderer.code` hook:
    - Routes ` ```mermaid ` fenced blocks into `<div class="mermaid" id="mermaid-N">` placeholders (counter-based unique IDs) for deferred rendering.
    - Otherwise highlights with `hljs.highlight(lang)` when the language is known, else `hljs.highlightAuto`. Wraps in `<pre><code class="hljs language-...">`.
  - Initializes `mermaid` once at module load: `startOnLoad: false`, `theme: 'default'`, `securityLevel: 'loose'`, `fontFamily: "'Inter', sans-serif"` (matches the Typora-inspired UI).
  - Imports `highlight.js/styles/github.css` for syntax highlighting styles (matches design system).
  - **Exports**:
    - `renderMarkdown(text: string): string` — synchronous parse (`marked.parse` with `async: false`); returns `''` for empty input. Output is raw HTML — caller is responsible for `{@html}` injection.
    - `renderMermaidIn(container: HTMLElement): Promise<void>` — finds all `.mermaid:not([data-processed])` nodes inside `container` and runs `mermaid.run({ nodes })`. No-op if no unprocessed nodes. Call after `renderMarkdown` HTML is mounted in the DOM.

## Patterns

- Pure ES modules — no Svelte imports, no stores, no Wails bindings.
- Module-level singletons: `mermaid.initialize` and `marked.use({ renderer })` run once at import time.
- `mermaidCounter` is module-scoped state for unique DOM IDs across the app session.
- `renderMarkdown` is sync; `renderMermaidIn` is async because `mermaid.run` parses + renders SVG.
- Two-step render flow: first inject HTML from `renderMarkdown`, then call `renderMermaidIn` on the mounted container to upgrade mermaid placeholders into SVG diagrams.

## Related

- `../lib` — Svelte components that consume `renderMarkdown` / `renderMermaidIn` (chat message rendering).
- `../stores` — Svelte stores for app state (not touched from this directory).
- `highlight.js`, `marked`, `mermaid` — npm dependencies declared in `agent/frontend/package.json`.
