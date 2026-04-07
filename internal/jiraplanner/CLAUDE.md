# package jiraplanner

Handlers and shared state for the `plan-jira` workflow (Confluence page → AI plan → Jira epic + tasks) and the standalone `task-creator` workflow (free-text prompt → Jira epic + tasks).

## Files
- `state.go` — `PlanningState` per-correlation store, `ProjectPlan`/`JiraTask`/`Risk`/`Dep` types, `ParseProjectPlan` (extracts JSON from LLM output via reverse-scan + fallback streaming decoder), `stripCodeFences`, `renderTemplate`, `truncateStr`
- `reader.go` — `PageReaderHandler` (`page-reader`): reads Confluence page and writes content into `PlanningState`; extracts page ID from `WorkflowRequested.Source` (`confluence:<id>` prefix or bare numeric)
- `manager.go` — `ManagerHandler` (`project-manager`): implements `Hinter` two-phase dispatch — `Hint()` runs the AI to generate the plan and emits `HintEmitted{confidence: 0.5}`; `Handle()` confirms after approval and emits `ContextEnrichment`. Falls back to inline generation if hint phase was skipped
- `creator.go` — `TaskCreatorHandler` (`jira-task-creator`, plan-jira) and `StandaloneCreatorHandler` (`task-creator`, standalone). Shared `createJiraIssues()` sorts tasks by priority, creates epic + tasks, links "Blocks" dependencies via `resolveDepKey` (exact match → substring fallback)
- `prompts.go` — Spanish-language system + user prompt templates: `ProjectManagerSystemPrompt`/`ProjectManagerUserPromptTemplate`, `TaskCreatorSystemPrompt`/`TaskCreatorUserPromptTemplate`. Both demand JSON-only responses
- `*_test.go` — unit tests for state/reader/creator (skipped here)

## Key types
- `PlanningState` — `sync.RWMutex` map of `correlationID → *WorkflowData`; `Get` is lazy-create, `Delete` for cleanup
- `WorkflowData` — per-workflow `PageID`, `PageTitle`, `PageContent`, `Plan` (own RWMutex)
- `ProjectPlan` — `Goal`, `EpicTitle`, `EpicDesc`, `Tasks[]`, `Risks[]`, `Dependencies[]`
- `JiraTask` — `Title`, `Description`, `Priority` (1=crit, 2=imp, 3=normal), `StoryPoints` (Fibonacci), `Tags`, `Dependencies` (titles, resolved post-create)

## Behavior
- No background polling, no scanner, no systemd-driven loop in this package — all four handlers are passive event handlers dispatched by `PersonaRunner` according to the workflow DAG (see root CLAUDE.md `plan-jira` and `task-creator` sections)
- `page-reader` and `project-manager` share `PlanningState` keyed by correlation ID rather than passing large payloads through events
- `project-manager` always pauses for operator review (confidence 0.5 < default `HintThreshold` 0.7) unless workflow def overrides
- Task dependency linking is best-effort: failures are logged, not fatal; missing matches silently dropped
- All AI prompts and Jira output are in Spanish ("Team Rocket")

## Patterns
- `Subscribes() []event.Type { return nil }` on every handler — dispatch is fully DAG-driven via `WorkflowDef.Graph`
- Errors wrapped with handler name: `fmt.Errorf("page-reader: %w", err)`
- `createJiraIssues` is the single Jira write path shared by both creator handlers (`sourceName` parameter distinguishes log/event source)
- Requires `CONFLUENCE_URL`/`CONFLUENCE_EMAIL`/`CONFLUENCE_TOKEN` (page-reader) and `JIRA_URL`/`JIRA_EMAIL`/`JIRA_TOKEN` (creators); missing config fails the handler with a clear error
- Idempotency: `createJiraIssues` is NOT idempotent — re-running creates duplicate epics/tasks. Workflow retries should be gated upstream

## Related
- `../confluence` — `Client.ReadPage`, `ExtractTextContent` (used by page-reader)
- `../jira` — `Client.CreateEpic`, `CreateTask`, `LinkIssues` (used by both creators)
- `../backend` — `Backend.Run` (LLM calls in manager + standalone creator)
- `../event` — `HintEmittedPayload`, `ContextEnrichmentPayload`, `WorkflowRequestedPayload`
- `../engine` — `PersonaRunner` dispatches these handlers per `WorkflowDef.Graph` for `plan-jira`/`task-creator`
- `../mcp` — `rick_run_workflow` with `dag=plan-jira` (source `confluence:<id>`) or `dag=task-creator` (with `prompt`)
- Root `CLAUDE.md` — workflow DAG diagrams and trigger reference
