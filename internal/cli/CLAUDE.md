# package cli

Cobra command tree for the `rick` binary — wires the engine, persona runner, projections, MCP server, and gRPC service together for each subcommand.

## Files
- `root.go` — `New()` builds the root cobra command and registers all subcommands.
- `run.go` — **DEPRECATED**. Direct in-process workflow execution. Do NOT add features here. All workflow execution must go through `rick serve` + MCP + agent UI. Still hosts shared helpers `selectWorkflowDef()` (workflow def selector with `RICK_DISABLE_QUALITY_GATE` strip) and `unmarshalPayload()`.
- `serve.go` — Long-running daemon. Boots store, bus, backend, handler registry, engine, PersonaRunner with `CompositeDispatcher` (local + gRPC stream), all four projections, `NotificationBroker`, gRPC `PersonaService`, optional GitHub/Jira pollers, and the MCP HTTP server. Defaults `--yolo=true`. Honors `RICK_LOG_LEVEL=debug`.
- `mcp.go` — MCP-over-stdio mode for Claude Desktop / Cursor. Subset of serve (no gRPC, no optional services). Logs to stderr at warn level since stdout is reserved for protocol.
- `deps.go` — Shared dep constructors: `openEstimationStore`, `loadMicroserviceMap`, `newGitHubClient`, `openPluginStore`, `startOptionalServices` (GitHub reporter, CI poller, Jira poller — all gated by env vars).
- `mcpclient.go` — `mcpCall()` JSON-RPC client used by pause/resume/cancel/guide to route through the running server. `replayAggregate()` shared aggregate replay helper. `defaultMCPURL = http://localhost:8077/mcp`.
- `events.go` — `rick events` reader. Per-aggregate or `--correlation` cross-aggregate listing with type-aware `eventSummary()` rendering.
- `status.go` — `rick status` replays an aggregate and prints status, prompt, completed personas, feedback iteration counts.
- `find.go` — `rick find <key> <value>` tag-based workflow lookup via `store.LoadByTag` (ticket, repo, repo_branch, source, workflow_id).
- `cancel.go` — `rick cancel`. MCP-first with direct-DB fallback when server unreachable.
- `pause.go` — `rick pause`, `rick resume`, `rick guide` (operator guidance + auto-resume). All MCP-first with direct-DB fallback.
- `plan.go` — `rick plan` triggers `plan-btu` via gRPC `InjectEvent` against a running server (requires `--page`, optional `--ticket`).
- `cli_test.go` — package tests.

## Commands
- `rick run` — DEPRECATED in-process workflow runner.
- `rick serve` — primary execution mode; HTTP MCP + gRPC service.
- `rick mcp` — stdio MCP server for desktop clients.
- `rick plan` — kick off a `plan-btu` workflow against a running server.
- `rick events <id>` — list events (`-c` for cross-correlation).
- `rick status <id>` — aggregate state via replay.
- `rick find <key> <value>` — tag lookup.
- `rick cancel|pause|resume <id>` — workflow lifecycle control.
- `rick guide <id> <text>` — inject `OperatorGuidance` (auto-resumes if paused).

## Patterns
- Each command builds its own `slog.Logger` to stderr; serve respects `RICK_LOG_LEVEL`.
- Lifecycle commands try MCP first (`mcpCall`) and fall back to direct `eventstore` writes — keeps projections in sync when the server is up.
- `selectWorkflowDef()` is the canonical name → `engine.WorkflowDef` mapper. Adding a new built-in workflow requires a new case here AND the registration loop in `serve.go` / `mcp.go` (see memory `feedback_selectworkflowdef_sync`).
- Direct-DB lifecycle writes use optimistic concurrency via `agg.Version+1` and `WithSource("cli:<verb>")`.
- Helpers `replayAggregate`, `truncate`, `unmarshalPayload` are package-shared.

## Related
- `../engine` — `Engine`, `PersonaRunner`, `WorkflowDef`, `LocalDispatcher`, aggregate.
- `../mcp` — MCP server consumed by `serve.go` and `mcp.go`.
- `../grpchandler` — `Server`, `StreamDispatcher`, `CompositeDispatcher`, `NotificationBroker`, `EventInjector`, `Client` (used by `plan.go`).
- `../eventstore`, `../eventbus`, `../event` — persistence and choreography primitives.
- `../handler` — handler registry + `Deps` injection point (`RegisterAll`).
- `../projection` — workflow status, token usage, phase timeline, verdict projections.
- `../backend`, `../persona` — AI backend factory and persona registry/prompt builder.
- `../jira`, `../confluence`, `../github`, `../jirapoller`, `../estimation`, `../planning`, `../pluginstore` — wired through `deps.go` and `startOptionalServices`.
- `../source` — `Resolver` used by deprecated `run.go` to fetch source content.
