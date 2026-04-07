# package mcp

JSON-RPC 2.0 Model Context Protocol server exposing Rick's workflow, job, workspace, Jira, wave, observability, and Confluence capabilities to MCP clients (Claude Desktop, Cursor, the agent UI) over stdio and HTTP.

## Files

- `server.go` — `Server`, `Tool`, `ToolDefinition`, `Deps`. JSON-RPC envelope types, `initialize` / `tools/list` / `tools/call` / `ping` dispatch, stdio `Serve(ctx, r, w)` loop with newline-delimited JSON framing (10 MiB buffer).
- `http.go` — `Server.ServeHTTP(ctx, addr)`. POST `/mcp` for JSON-RPC, GET `/mcp` for health/info, permissive CORS for the Wails webview. HTTP clients skip the `initialize` handshake (auto-marked initialized).
- `job.go` — `JobManager`, `Job`, `JobStatus`, `trackedJob`. In-memory async job registry with a background reaper (`maxJobAge = 2h`, `reaperTick = 5m`). Owns the lifecycle of `rick_consult` / `rick_run` subprocesses.
- `tools.go` — Workflow tool group (16): `rick_run_workflow`, status/list/events, token/timeline/verdict reads, persona output, dead letters, cancel/pause/resume, inject guidance, BTU planning, hint approve/reject. Defines shared `Deps`, the `register(Tool)` helper, `registerBuiltinTools()` (calls every group), and PR-URL/PR-source helper regexes (`extractPRURL`, `resolvePRBranch`).
- `tools_jobs.go` — Direct AI execution tools (6): `rick_consult`, `rick_run`, `rick_job_status`, `rick_job_output`, `rick_job_cancel`, `rick_jobs`. Backed by `JobManager` + `backend.Backend` subprocesses. No events.
- `tools_workspace.go` — Workspace tools (3): `rick_workspace_setup`, `rick_workspace_cleanup`, `rick_workspace_list`. Operate on `$RICK_REPOS_PATH` with a `*-rick-ws-*` deletion guard.
- `tools_jira.go` — Jira tools (10): read/write/transition/comment, epic-issues, search, link/delete-link, set-microservice, pr-links. Requires `JIRA_URL` / `JIRA_EMAIL` / `JIRA_TOKEN` (resolved via `Deps.Jira`).
- `tools_wave.go` — Wave tools (4): `rick_wave_plan`, `rick_wave_launch`, `rick_wave_status`, `rick_wave_cleanup`. Topologically sorts an epic's children and batch-launches `jira-dev` workflows.
- `tools_observability.go` — Observability tools (6): `rick_search_workflows`, `rick_retry_workflow`, `rick_workflow_output`, `rick_diff`, `rick_create_pr`, `rick_project_sync`. Reads tags/projections, drives `gh pr create`, emits Mermaid diagrams.
- `tools_confluence.go` — Confluence tools (2): `rick_confluence_read`, `rick_confluence_write`. Requires `CONFLUENCE_URL` / `CONFLUENCE_EMAIL` / `CONFLUENCE_TOKEN` (resolved via `Deps.Confluence`).

Test files (`*_test.go`) cover protocol handshake, HTTP transport, job lifecycle, and per-tool behavior with mocked Jira/Confluence clients.

## Key types

- `Server` — holds `Deps`, the `tools` map, a logger, and a `JobManager`. Built once via `NewServer(deps, logger)`; tear down with `Close()` to stop the reaper.
- `Tool` / `ToolDefinition` — name, description, JSON Schema for input, and a `ToolHandler func(ctx, json.RawMessage) (any, error)` that returns a JSON-serializable result.
- `Deps` — injection bag: `eventstore.Store`, `eventbus.Bus`, `*engine.Engine`, four projections (`Workflows`, `Tokens`, `Timelines`, `Verdicts`), `SelectWorkflow`, `BackendName`, `WorkDir`, `Yolo`, plus `backend.Backend`, `*jira.Client`, `*confluence.Client` for Tier 1-5 tools.
- `JobManager` / `Job` / `JobStatus` — async job tracking with `running`/`completed`/`failed`/`cancelled` states and per-job cancel funcs.
- `jsonRPCRequest` / `jsonRPCResponse` / `jsonRPCError` — wire envelopes, exported only within the package.

## Patterns

- **Tool registration**: each `tools_*.go` file owns a `registerXxxTools()` method on `*Server`. `registerBuiltinTools()` in `tools.go` calls all groups during `NewServer`. Adding a group means adding a single call there.
- **JSON Schema inline**: schemas are written as `map[string]any` literals next to each `register(Tool{...})` call — no struct tags, no codegen. Required fields go in `"required": []string{...}`.
- **Synchronous workflow tools vs async jobs**: workflow tools publish events through `Deps.Engine` / `Deps.Bus` and return immediately with a correlation ID; clients poll via `rick_workflow_status`. Job tools (`rick_consult`, `rick_run`) spawn a `backend.Backend` subprocess in a goroutine, return a job ID, and clients poll `rick_job_status` / `rick_job_output`.
- **Env-gated tools**: Jira and Confluence tool groups expect `Deps.Jira` / `Deps.Confluence` to be non-nil. The wiring (`cmd/rick/serve.go`) only constructs these clients when the corresponding env vars are present; otherwise the tools are still registered but fail at call time with a clear error.
- **Result encoding**: handlers return any JSON-serializable value; `handleToolsCall` marshals it as pretty JSON inside a single `text` content block. Errors become `IsError: true` content blocks (not JSON-RPC errors) so MCP clients can render them.
- **Stdio framing**: line-delimited JSON, one request per line. HTTP framing is request-per-POST and bypasses the `initialize` handshake.

## Adding a new tool

1. Pick the right `tools_*.go` file (or add a new group + a `registerXxxTools()` method and call it from `registerBuiltinTools()` in `tools.go`).
2. Define the input args struct and (if useful) a result struct near the handler.
3. Inside `registerXxxTools()` call `s.register(Tool{Definition: ToolDefinition{Name, Description, InputSchema}, Handler: s.toolFoo})` — schema as `map[string]any`, with a `"required": []string{...}` slice if any args are mandatory.
4. Implement the handler `func (s *Server) toolFoo(ctx context.Context, args json.RawMessage) (any, error)`. Unmarshal into the args struct, talk to `s.deps.*`, return a JSON-serializable value or an error.
5. Add a test in the matching `*_test.go` (use the existing mock Jira/Confluence helpers if you need them) and update the tool count in the root `CLAUDE.md` and any agent-side surface (slash command list, autocomplete) if it should be operator-visible.

## Related

- `../engine` — workflow lifecycle, `RegisterWorkflow`, `RegisteredWorkflows`, the source of truth for `Deps.Engine`.
- `../eventstore`, `../eventbus`, `../event` — persistence and bus that workflow tools publish through.
- `../projection` — `WorkflowStatusProjection`, `TokenUsageProjection`, `PhaseTimelineProjection`, `VerdictProjection` feed the read-side tools.
- `../backend`, `../persona` — `rick_consult` / `rick_run` spawn `backend.Backend` subprocesses with `persona.PromptBuilder`-style prompts.
- `../jira`, `../confluence` — REST clients used by the env-gated tool groups.
- `../workspace` — git/workspace primitives behind the workspace and wave tools.
- `../grpchandler` — peer transport for external handlers; MCP is the human/operator surface, gRPC is the machine surface. They share `engine.Engine` but never call into each other.
