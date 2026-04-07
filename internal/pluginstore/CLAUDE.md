# package pluginstore

Shared SQLite-backed state store for cross-plugin coordination — tracks processed Jira tickets, their workflow correlation IDs, PR linkage, and ci-fix attempt counters.

## Files
- `store.go` — `Store` struct, `New(path)` constructor, schema migrations, and all query methods.

## Key types
- `Store` — concrete struct wrapping `*sql.DB` (no interface). Opened via `New(path string) (*Store, error)`; `Close()` releases the connection.
- `Ticket` — row model: `TicketID`, `CorrelationID`, `Repo`, `Branch`, `PRURL`, `PRNumber`, `Summary`, `Status`, `CreatedAt`, `UpdatedAt`.

## API
Ticket dedup / lifecycle:
- `IsProcessed(ticketID string) (bool, error)` — existence check on `processed_tickets`.
- `SaveTicket(t Ticket) error` — upsert; on conflict updates `correlation_id`, `status`, `updated_at`.
- `UpdateTicketStatus(correlationID, status string) error` — status update keyed by correlation.
- `GetTicketByCorrelation(correlationID string) (*Ticket, error)` — lookup by workflow correlation; returns `nil, nil` on `sql.ErrNoRows`.

CI-fix attempt counter:
- `GetCIAttemptCount(ticketID string) int` — returns 0 on miss/error (swallowed).
- `IncrementCIAttempt(ticketID string) error` — upsert+increment in one statement.

## Patterns
- Backing store: SQLite via `modernc.org/sqlite` (pure-Go), opened with `_journal_mode=WAL&_busy_timeout=5000`. Separate database file from `eventstore` — pluginstore is its own DB path, not the event log.
- Schema: two tables, both ticket-keyed.
  - `processed_tickets` — primary dedup table; indexes on `correlation_id` and `status`.
  - `ci_attempts` — separate counter table so attempt bumps don't churn the ticket row.
- No namespacing per plugin and no TTL — schema is hardcoded for the Jira→workflow→PR→CI pipeline, not a generic KV store despite the package name.
- Errors wrapped with `pluginstore:` prefix; `GetCIAttemptCount` is the lone exception (returns 0 on any error).
- Timestamps stored as SQLite `datetime('now')` text and parsed with layout `"2006-01-02 15:04:05"` on read.

## Related
- `../jirapoller` — primary writer; calls `IsProcessed` for dedup and `SaveTicket` after starting a workflow.
- `../github` — `ci_poller.go`, `reporter.go`, `fetcher.go` read tickets via `GetTicketByCorrelation` and bump `IncrementCIAttempt` on CI failures.
- `../handler` (`handlers.go`) and `../cli` (`deps.go`) — wire the `*Store` into background services at startup.
- `../eventstore` — separate concern; this package does not share its DB or transactions. The two stores live side by side under `~/.local/share/rick/`.
- Historical context: handlers from the deleted `rick-plugins` repo were consolidated into this main repo (memory: `project_plugins_consolidated.md`); `pluginstore` predates that move and kept its name for the cross-service state it holds.
