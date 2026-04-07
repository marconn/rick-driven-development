# package jirapoller

Polls Jira via JQL on a fixed interval and publishes `WorkflowRequested` events for unseen tickets, triggering a Rick workflow per issue.

## Files
- `poller.go` — single-file package: Config, Poller, polling loop, issue processing, field/label extraction, prompt builder

## Key types
- `Config` — `JQL` (required), `PollInterval` (default 60s), `MaxResults` (default 50), `WorkflowID` (default `pr-review`), `FieldMappings map[string]string`, `Logger`
- `Poller` — holds `*jira.Client`, `*pluginstore.Store`, `eventbus.Bus`, logger
- `issueInfo` — extracted `Repo`, `Branch`, `PRURL`, `PRNumber` per ticket
- `NewPoller(jiraClient, pstore, bus, cfg) *Poller` — constructor; applies `cfg.defaults()`
- `(*Poller).Run(ctx)` — blocks until ctx cancelled, polls immediately then on `time.Ticker`

## Behavior
- Calls `jira.Client.Search(JQL, MaxResults)` per cycle, iterates `result.Issues`
- For each issue: `pluginstore.IsProcessed(key)` dedup check; skips if already seen
- Fetches full issue via `jira.FetchRawIssue` and extracts repo/branch/PR info
- Skips (warn-logs) tickets with no resolvable repo
- Generates new `uuid` correlation ID, builds Markdown prompt with summary/description/repo/branch/PR
- Publishes `event.WorkflowRequested` with `WorkflowRequestedPayload{Prompt, WorkflowID, Source: "jira:KEY", Repo, Ticket, BaseBranch}`, source `jira-poller`
- Persists to pluginstore via `SaveTicket` with status `running` AFTER publish — dedup state survives restarts
- Errors per-issue are logged but do not abort the cycle

## Patterns
- Field resolution priority: `FieldMappings` (e.g. `customfield_10100`) → label fallback (`repo:owner/name`, `branch:name`) → `branch` defaults to `main`
- `extractPRNumber` parses trailing path segment of PR URL via `fmt.Sscanf`
- No env vars read directly here — caller (cmd wiring) injects `jira.Client` (which uses `JIRA_URL`/`JIRA_EMAIL`/`JIRA_TOKEN`) and `Config`
- State persistence: `pluginstore.Store` (SQLite-backed), keyed by Jira issue key — single source of truth for "already processed"
- No internal state on `Poller` — restart-safe; all dedup lives in pluginstore

## Related
- `../jira` — REST client (`Search`, `FetchRawIssue`, `ExtractTextField`, `RawIssue`, `SearchIssue`)
- `../pluginstore` — `Store.IsProcessed`, `Store.SaveTicket`, `Ticket` struct
- `../event` — `WorkflowRequested` type, `WorkflowRequestedPayload`, `MustMarshal`, `New`, `WithCorrelation`, `WithSource`
- `../eventbus` — `Bus.Publish` (event ingress; Engine downstream consumes `WorkflowRequested`)
- `rick-jira-poller.service` — systemd unit that runs this poller as a long-lived process
- Triggered workflow defaults to `pr-review` (see root `CLAUDE.md` for DAG)
