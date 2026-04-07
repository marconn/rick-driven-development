# package persona

Identity layer for AI personas: name constants, the system-prompt `Registry`, and `PromptBuilder` that renders per-phase user prompts from accumulated workflow context.

## Layout
- `*.go` (this dir) — `Registry`, `PromptBuilder`, both `embed.FS` entrypoints, persona name constants
- `phases/` — per-phase user-prompt `text/template` markdown -> see `phases/CLAUDE.md`
- `prompts/` — per-persona system-prompt markdown (identity text) -> see `prompts/CLAUDE.md`

## Files (top level)
- `persona.go` — name constants (`Researcher`, `Architect`, `Developer`, `Reviewer`, `QA`, `QAAnalyzer`, `Committer`, `Workspace`, `ContextSnapshot`, `FeedbackAnalyzer`, `PRConsolidator`), `PhasePersona` map (phase verb -> persona name), `Persona` struct, `Registry`, `DefaultRegistry()`, `LoadSystemPrompt()`. Holds `//go:embed prompts/*.md` -> `promptFS`.
- `prompt.go` — `PromptContext`, `PromptBuilder`, phase template rendering. Holds `//go:embed phases/*.md` -> `phaseFS`. Also defines internal `promptData` (template field names) and `formatEnrichments()`.
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
