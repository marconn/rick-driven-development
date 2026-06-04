# package persona

Identity layer for AI personas: name constants, the system-prompt `Registry`, and `PromptBuilder` that renders per-phase user prompts from accumulated workflow context.

## Layout
- `*.go` (this dir) — `Registry`, `PromptBuilder`, both `embed.FS` entrypoints, persona name constants
- `phases/` — per-phase user-prompt `text/template` markdown -> see `phases/CLAUDE.md`
- `prompts/` — per-persona system-prompt markdown (identity text) -> see `prompts/CLAUDE.md`

## Files (top level)
- `persona.go` — name constants (`Researcher`, `Architect`, `Developer`, `Reviewer`, `QA`, `QAAnalyzer`, `Committer`, `Workspace`, `ContextSnapshot`, `FeedbackAnalyzer`, `PRConsolidator`), `PhasePersona` map (phase verb -> persona name), `Persona` struct, `Registry`, `DefaultRegistry()`, `LoadSystemPrompt()`. Holds `//go:embed prompts/*.md` -> `promptFS`.
- `prompt.go` — `PromptContext`, `PromptBuilder`, phase template rendering. Holds `//go:embed phases/*.md` -> `phaseFS`. Also defines internal `promptData` (template field names) and `formatEnrichments()`.
- `resolver.go` — **dual-source resolution** (data-driven personas, L1). `LoadManifestDir(root)` discovers `<root>/personas/<name>/SKILL.md` + `<root>/skills/<name>/SKILL.md` (operator-local `RICK_PERSONA_MANIFESTS_DIR`), parsing each via `manifest.go` and returning a `ManifestSource` plus per-manifest errors (a bad manifest fails only itself). `ManifestSource.ComposeSystemPrompt(name)` builds `system prompt = identity + ordered skill fragments`; identity is an `identity:` file ref (resolved relative to the manifest dir) or the inline body; a missing skill ref is a **load error** (never a silent skip). The `Registry` holds an optional `*ManifestSource` (`LoadManifests(root, logger)`); `LoadSystemPrompt` consults it **first** — manifest composition WINS over the embedded/code prompt and over the custom-dir override, so an operator overrides/recomposes a persona with no recompile. This is the single shared resolution path: AIHandler, PRConsolidator, and the `rick_consult`/`rick_run` MCP jobs all go through `LoadSystemPrompt`, so manifests apply uniformly without per-call-site wiring (F8). Unset env ⇒ byte-for-byte the embedded-only path. Wired in `cli/serve.go` + `cli/mcp.go`. (Knowledge negotiation against backend capability is **0008**, not yet wired.)
- `manifest.go` — **manifest schema + validator** (data-driven personas). Parses a `SKILL.md` (YAML frontmatter + markdown body) into a typed `PersonaManifest` (`schema_version`, `name`, `description`, `identity` file-ref-or-inline-body, `skills[]`, `knowledge[]{pack, load, criticality}`) or `SkillManifest` (`name`, `description`, `tools[]`, body). `ParsePersonaManifest` / `ParseSkillManifest` enforce: a closed schema (`KnownFields(true)` rejects unknown keys), `schema_version == CurrentManifestSchemaVersion`, required fields, and `load`/`criticality` enums. **Safety boundary:** `forbiddenKeys` (`yolo`, `plaintext`, `verdict_bearing`, `backend`, `timeout`, `target`, `phase`, `effort`) are rejected **by name** — a persona manifest owns prompt composition only; runtime/safety knobs stay code-owned in `handler/handlers.go`. Each manifest fails only itself (returns an error, never panics / never affects the process). **Parse + validate only** — composition (system prompt = identity + skill fragments), the dual-source registry, and knowledge negotiation are the resolver's job (not yet wired).
- `persona_test.go` — unit tests; not load-bearing for callers.

## Key types
- `Persona{Name, Description}` — minimal identity record stored in the registry.
- `Registry` — thread-safe (`sync.RWMutex`) map of personas with optional `customDir` override. Methods: `Register`, `Get`, `Names`, `SetCustomDir`, `LoadSystemPrompt(name)` (custom dir first, then embedded `prompts/<name>.md`). `DefaultRegistry()` pre-registers all 11 built-ins.
- `PromptBuilder` — stateless, optional `customDir`. Methods: `SetCustomDir(dir)`, `Build(phase, ctx) (string, error)`. Looks up `<dir>/phases/<phase>.md` first, falls back to embedded.
- `PromptContext` — accumulator filled by handlers from the event store: `Task`, `Source`, `Outputs` (phase -> text), `Feedback`, `Iteration`, `Ticket`, `BaseBranch`, `WorkspacePath`, `Codebase`, `Schema`, `GitContext`, `Enrichments`.

## Prompt assembly
- Handlers build a `PromptContext` by walking prior `PersonaCompleted` / `AIResponseReceived` events for the correlation (via `eventstore.Store.LoadByCorrelation`).
- `PromptBuilder.Build(phase, ctx)` loads the phase template (custom dir override -> embedded `phases/<phase>.md`), parses it as `text/template`, and executes against an internal `promptData` struct that mirrors v1's field names (`.Source`, `.Research`, `.Architecture`, `.Develop`, `.Feedback`, `.PreviousDevelop`, `.FeedbackAnalysis`, `.Codebase`, `.Schema`, `.GitContext`, `.Enrichments`, `.Ticket`, `.BaseBranch`).
- Feedback loops: when `ctx.Feedback != ""`, `PreviousDevelop` is populated from `Outputs["develop"]` so the develop template can show the prior attempt alongside the new feedback.
- The system prompt comes separately from `Registry.LoadSystemPrompt(personaName)`; the backend layer stitches the two together (system prompt + rendered user prompt) before invoking the LLM CLI.

## Related
- `../backend` — consumer; combines system prompt + built phase prompt and shells out to claude/gemini.
- `../handler` — AI handlers call `PromptBuilder.Build` inside `Handle()` (and `Hint()` where applicable).
- `../event`, `../eventstore` — source of `PromptContext` fields (prior outputs, enrichments, ticket metadata).
- `../engine` — `WorkflowDef.PhaseMap` maps phase verbs to handler names; `PhasePersona` here maps phase verbs to persona identity.
