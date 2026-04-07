# internal/

All Rick Go packages live here. No public API — `internal/` is enforced by Go's visibility rules. Each subpackage has its own `CLAUDE.md`.

## Core (event sourcing + dispatch)
- [`event/`](event/CLAUDE.md) — envelope + event type catalog
- [`eventstore/`](eventstore/CLAUDE.md) — SQLite-backed Store, optimistic concurrency, tag index
- [`eventbus/`](eventbus/CLAUDE.md) — in-process bus, 7 middleware
- [`engine/`](engine/CLAUDE.md) — WorkflowAggregate, PersonaRunner, WorkflowDef/Graph, Sentinel
- [`projection/`](projection/CLAUDE.md) — read models (status/tokens/phases/verdicts)

## Handlers + personas
- [`handler/`](handler/CLAUDE.md) — Handler interface + every concrete persona handler
- [`persona/`](persona/CLAUDE.md) — PromptBuilder + embedded prompt/phase markdown
- [`backend/`](backend/CLAUDE.md) — Claude/Gemini CLI subprocess wrappers

## External integrations
- [`jira/`](jira/CLAUDE.md) — Atlassian Jira REST client
- [`confluence/`](confluence/CLAUDE.md) — Confluence REST client
- [`github/`](github/CLAUDE.md) — GitHub PR/CI client
- [`adf/`](adf/CLAUDE.md) — Markdown to Atlassian Document Format converter
- [`source/`](source/CLAUDE.md) — `gh:`/`jira:`/`file:` source string parser

## Workflow + tooling surface
- [`mcp/`](mcp/CLAUDE.md) — JSON-RPC 2.0 MCP server (46 tools)
- [`grpchandler/`](grpchandler/CLAUDE.md) — bidi gRPC stream for external handlers (+ [`grpchandler/proto/`](grpchandler/proto/CLAUDE.md))
- [`cli/`](cli/CLAUDE.md) — cobra command tree (note: `run.go` is DEPRECATED)
- [`workspace/`](workspace/CLAUDE.md) — isolated git clones under `$RICK_REPOS_PATH`

## Background services / planning
- [`jirapoller/`](jirapoller/CLAUDE.md) — JQL polling loop, dedups via pluginstore
- [`jiraplanner/`](jiraplanner/CLAUDE.md) — plan-jira / task-creator handlers
- [`planning/`](planning/CLAUDE.md) — plan-btu workflow handlers
- [`estimation/`](estimation/CLAUDE.md) — Fibonacci calibration store
- [`pluginstore/`](pluginstore/CLAUDE.md) — KV store for plugins / poller dedup state

## Observability
- [`observe/`](observe/CLAUDE.md) — in-memory metrics recorder (note: not yet wired into production)

## Cross-cutting conventions
- Functional options: `WithName`, `WithLogger`, `WithTimeout`
- Sentinel errors: `ErrConcurrencyConflict`, `ErrHandlerNotFound`, `ErrIncomplete`
- Errors wrapped with package context: `fmt.Errorf("engine: load aggregate: %w", err)`
- Tests use in-memory SQLite (`:memory:`)
- Handlers return events; never persist or publish — caller owns atomicity
