# package planning

Handlers and shared state for the `plan-btu` workflow: read a Confluence BTU, research target repos, generate a Spanish technical plan, estimate Fibonacci points, and write the result back to Confluence.

## Files
- `state.go` — `PlanningState` (per-correlation in-memory store), `WorkflowPlan`, `TechnicalPlan`/`Task`/`Risk`/`Dependency` types, `ParseTechnicalPlan` + `extractJSON` helpers
- `prompts.go` — embedded prompt templates (system + user) for researcher, architect, estimator, plus `ConfluenceHTMLTemplate`. All operator-facing prompts are in Spanish
- `reader.go` — `ReaderHandler` (`confluence-reader`): fetches the BTU page, extracts sections (`que es`, `como funciona`, `tipos de usuario`, `dispositivo`), detects `[microservice]` brackets
- `researcher.go` — `ResearcherHandler` (`codebase-researcher`): parallel AI research across repos with `Yolo: true`, AI-assisted repo inference fallback when BTU has no brackets, AI consolidation step
- `architect.go` — `ArchitectHandler` (`plan-architect`): two-phase `Hinter` — `Hint()` drafts plan, `Handle()` finalizes after `HintApproved`. Applies operator guidance via `applyAdjustments`. Emits `plan.generated`
- `estimator.go` — `EstimatorHandler` (`estimator`): two-phase `Hinter`. Loads calibration + similar estimates from `internal/estimation`, generates per-task points, persists batch on approval
- `writer.go` — `WriterHandler` (`confluence-writer`): builds Confluence storage HTML and updates the BTU page section under `PlanHeading` ("plan tecnico de implementacion"). Cleans up `PlanningState` after write
- `microservices.go` — `MicroserviceMap`: loads `AGENTS.md`/`CLAUDE.md` repo index or simple/vibe-format mapping files, resolves microservice names to paths under `RICK_REPOS_PATH`, exposes `PlatformContext()` for AI prompts

## Key types
- `PlanningState` / `WorkflowPlan` — mutex-guarded shared state, keyed by correlation ID, populated by handlers in DAG order
- `TechnicalPlan` — `{Summary, Tasks[], Microservices[], Risks[], Dependencies[], UserDeviceNotes}` (JSON tags)
- `Task` — `{Description, Microservice, Category(frontend|backend|infra), Files, Notes, Points, Justification}`
- `Risk` — `{Description, Probability(alta|media|baja), Impact, Mitigation}`
- `MicroserviceMap` — name → repo dir lookup with `Names()`, `RepoPath()`, `ResolveAll()`, `ListAvailable()`, `PlatformContext()`

## Functions
- `ParseTechnicalPlan(output)` — extracts trailing JSON, falls back to wrapping raw text as `Summary`
- `extractJSON(text)` — last-valid-JSON-object scan from end (AI usually emits JSON last) with forward-decode fallback
- `renderTemplate`, `splitParagraphs`, `escapeHTML`, `splitWords`, `stripCodeFences` — small string utilities
- `detectMicroservices(html)` — regex scan for `[name]` brackets, filters Spanish stopwords
- `extractTicketID(title)` — pulls `BTU-XXXX`/`ING-XXXX` from titles for estimation persistence
- `extractGuidance(env)` — pulls operator `guidance` field from event payload for plan adjustments

## Patterns
- **Shared state via `PlanningState`** instead of replaying events — handlers read/write `WorkflowPlan` under correlation ID, `confluence-writer` deletes on completion
- **Two-phase `Hinter` dispatch** for architect and estimator — `Hint()` generates draft + emits `HintEmitted{confidence: 0.5}`, `Handle()` finalizes after `HintApproved`. Both have fallback paths if hint phase is skipped
- **Mandatory JSON tail** — architect and estimator prompts append a "REQUISITO OBLIGATORIO" suffix forcing the model to end with a parseable JSON block
- **Spanish output** — all user-facing prompts and Confluence HTML are Spanish; system prompt explicitly forbids disclaimers about uncertain file paths
- **Tool-enabled AI runs** — researcher and architect set `Yolo: true` and `WorkDir` to a repo path so the backend can read files directly
- **Fibonacci scoring** — estimator system prompt encodes the 1/2/3/5/8/13/21/34 scale with day-equivalents and threshold questions
- **Confluence section update** — writer re-reads page for current version then calls `confluence.UpdatePageSection` with `PlanHeading` as anchor

## Related
- `../estimation` — `Store.CalibrationSummary`, `Store.SimilarEstimates`, `Store.SaveBatch`, `Estimate` struct
- `../confluence` — `Client.ReadPage`, `UpdatePageSection`, `ExtractTextContent`; `Page` type
- `../backend` — `Backend.Run`, `Request{SystemPrompt, UserPrompt, WorkDir, Yolo}`
- `../event` — `ContextEnrichment`, `HintEmitted`, `WorkflowRequested` event types and payloads
- `../eventstore` — `Store.LoadByCorrelation` (used by reader to find the page ID from `WorkflowRequested`)
- `../handler` — `Hinter` interface (architect/estimator); registration of these handlers happens in the engine wiring (cmd/serve)
- Workflow definition `plan-btu` is registered in `internal/engine/workflow_def.go`; see root CLAUDE.md for the DAG diagram and trigger contract
