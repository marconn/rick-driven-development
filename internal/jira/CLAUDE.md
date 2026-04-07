# package jira

Thin REST client for the Jira Cloud API v3 with ADF formatting helpers, used by every Jira-touching MCP tool, handler, and workflow in Rick.

## Files
- `client.go` — entire package: `Client`, auth, all REST methods, ADF helpers, types
- `client_test.go` — table-driven tests for HTTP methods (httptest server)
- `issue_test.go` — table-driven tests for Issue parsing, ADF conversion, field extraction

## Key types
- `Client` — basic-auth HTTP wrapper, holds `baseURL`, `email`, `token`, default `project`, `teamID`
- `Issue` — typed view (Summary, Description, Status, Labels, Components, AC custom fields, Microservice)
- `RawIssue` — `map[string]json.RawMessage` for arbitrary custom fields
- `SearchResult` / `SearchIssue` — JQL response shape
- `EpicChildIssue` — flattened epic child (key, summary, status, assignee, labels, points, type)
- `IssueLink` — issue link with direction (`inward`/`outward`) and human label
- `PRLink` — GitHub PR linked via dev-status API (id, url, status, source repo)
- `CreateOption` — functional options: `WithProject`, `WithEpicLink`, `WithStoryPoints`, `WithLabels`, `WithComponents`, `WithPriority`, `WithTeam`

## Capabilities
- **Auth**: `NewClientFromEnv()` reads `JIRA_URL`/`JIRA_EMAIL`/`JIRA_TOKEN`, returns `nil` (not error) when unset for graceful degradation; `NewClient(...)` for tests; `WithProject`/`WithTeamID` builders
- **Read**: `FetchIssue` (typed), `FetchRawIssue`/`GetIssue` (raw fields), `Search` (JQL via POST `/rest/api/3/search/jql` — GET endpoint deprecated by Atlassian), `FetchEpicChildren` (JQL `"Epic Link" = KEY`)
- **Write**: `CreateIssue` (any type, with options), `CreateEpic`, `CreateTask`, `UpdateField` (single field PUT), `AddLabel` (additive update without clobbering), `SetMicroservice` (writes `customfield_11538` with `repo:<name>` label fallback)
- **Transition**: `TransitionIssue` — looks up transitions, matches target status name case-insensitively, then POSTs the transition ID
- **Comment**: `AddComment` — wraps body in minimal ADF doc
- **Links**: `LinkIssues` (Blocks shortcut), `LinkIssuesWithType` (any link type), `FetchIssueLinks`, `DeleteIssueLink`
- **PR links**: `FetchPRLinks` calls `/rest/dev-status/latest/issue/detail` (requires GitHub-for-Jira integration); resolves issue numeric ID first

## Patterns
- **ADF formatting**: `MarkdownToADF(text)` converts `**bold**`, `- `/`* ` bullets, `## ` headings, paragraph breaks into Atlassian Document Format. `parseInlineMarks` handles inline `**bold**` runs. Used by all create/update paths so descriptions accept Markdown
- **ADF parsing**: `ADFToPlainText(any)` and `ExtractTextField(json.RawMessage)` walk ADF nodes recursively, extracting `text` leaves; pass-through for plain strings
- **Custom field IDs** (Huli/Team Rocket Jira instance, hardcoded):
  - `customfield_10004` — Story Points
  - `customfield_10035` / `customfield_10036` — Acceptance Criteria (two locations)
  - `customfield_10200` — Epic Link (parent)
  - `customfield_10201` — Epic Name (required when creating Epic)
  - `customfield_11533` — Team (set via `JIRA_TEAM_ID` env or `WithTeam`)
  - `customfield_11538` — Microservice select (maps to repo dir under `RICK_REPOS_PATH`)
  - QA Steps default `customfield_10037` lives in the qa-jira-writer handler, not here
- **Env vars**: `JIRA_URL`, `JIRA_EMAIL`, `JIRA_TOKEN` (required); `JIRA_PROJECT` (default `PROJ`); `JIRA_TEAM_ID` (numeric, optional)
- **Error wrapping**: every method wraps with `jira:` or operation name; HTTP non-2xx returns `jira API returned %d: %s` with body excerpt
- **No retries / no rate limiting** — caller is responsible

## Related
- `../mcp/tools_jira.go` — 10 `rick_jira_*` MCP tools wrap this client
- `../mcp/tools_wave.go` — wave planning uses `FetchEpicChildren` + `FetchIssueLinks`
- `../handler/` — `jira-context` (jira-dev), `jira-task-creator` (plan-jira), `task-creator`, `qa-context`/`qa-jira-writer` (jira-qa-steps) all instantiate via `NewClientFromEnv`
- Root `CLAUDE.md` documents the workflows and env var requirements
