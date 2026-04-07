# package github

GitHub REST API client and event-driven adapters for fetching PR feedback, posting workflow results, and polling CI status — used by `pr-feedback` / `pr-review` workflows and the `rick_create_pr` MCP tool.

## Files
- `client.go` — Pure-Go HTTP client for GitHub REST v3 (no `gh` CLI shell-out). Pagination, custom Accept headers, bearer auth.
- `fetcher.go` — `FetcherHandler` (`handler.Handler`) — DAG-dispatched persona that fetches PR reviews/comments/diff and emits `ContextEnrichment` for downstream personas.
- `reporter.go` — `Reporter` — bus subscriber that posts a consolidated PR comment when a workflow reaches a terminal state.
- `ci_poller.go` — `CIPoller` — background goroutine that polls GitHub Actions check runs after a successful workflow and triggers a `ci-fix` workflow on failure.

## Key types
- `Client` — REST client. Constructors: `NewClient(token)`, `NewClientWithBase(baseURL, token)` (Enterprise).
- `PullRequest`, `PRRef`, `PRRepoRef`, `PRHead` — PR metadata models.
- `Review`, `ReviewComment`, `PRComment`, `User` — review payload models.
- `CheckRun`, `CheckRunsResponse` — Actions check run models.
- `FetcherHandler` — implements `Name()="github-pr-fetcher"`, `Subscribes()=nil` (DAG-dispatched), `Handle()` reads source from `WorkflowRequested`/`WorkflowStarted` then calls `FetchPRFeedback`.
- `Reporter` — `Start(bus)` subscribes to `WorkflowCompleted`/`WorkflowFailed`; `WithCIPoller(p)` chains CI polling after successful runs.
- `CIPoller` / `CIPollerConfig` — defaults: 15s interval, 10m timeout, `ci-fix` workflow, max 2 retries.

## Patterns
- **HTTP-only, no `gh` CLI subprocess** in this package — auth via bearer token (`Authorization: Bearer <token>`), `Accept: application/vnd.github+json`, `X-GitHub-Api-Version: 2022-11-28`. (`rick_create_pr` MCP tool shells out to `gh` separately — not in this package.)
- Token comes from caller; no env-var lookup inside the package. Wiring code reads `GITHUB_TOKEN` etc.
- Pagination: `getPaginated()` walks the `Link: rel="next"` header until exhausted.
- Diff fetch: `GetPRDiff` overrides Accept with `application/vnd.github.diff` (raw unified diff text).
- PR reference parsing (`parsePRRef`, `ParsePRURL`): supports `gh:owner/repo#123` and `https://github.com/owner/repo/pull/123` (also matches inside surrounding text). Falls back to `pluginstore.Ticket` lookup by correlation ID when source has no PR ref.
- `Reporter.handleTerminal` is non-fatal on every error path — never dead-letters on GitHub API failure (logs and returns nil).
- `CIPoller` uses `pluginstore.GetCIAttemptCount`/`IncrementCIAttempt` to cap retry chains and constructs a new correlation ID per attempt (`<orig>-cifix-<n>`).
- All errors wrapped with `github: <op>: %w` per repo convention.
- Diff summarization in `formatPRFeedback` reports file count + ±line totals only (avoids flooding LLM context).

## Related
- `../mcp` — `rick_create_pr` tool (uses `gh` CLI directly, not this package).
- `../persona/phases` — `pr-architect`, `pr-reviewer`, `pr-qa`, `pr-consolidator`, `pr-jira-context` consume the `ContextEnrichment` events emitted here.
- `../workspace` — provisions the clone the personas operate on.
- `../pluginstore` — `Ticket` rows hold the `TicketID → repo + PR number + correlation` mapping used by Reporter and CIPoller fallbacks.
- `../eventbus`, `../eventstore`, `../event` — bus/store wiring and `ContextEnrichmentPayload` / `WorkflowRequestedPayload` types.
