# agent (rick-agent desktop app)

Wails v2 + Svelte 5 desktop application that gives operators a chat + dashboard UI for Rick, backed by a Gemini ADK operator that calls rick-server's MCP tools.

## Layout
- `main.go` — Wails bootstrap. Embeds `frontend/dist`, configures window (900x700, "Rick Operator"), wires `OnStartup`/`OnShutdown`, binds `App` to JS.
- `app.go` — `App` struct + Wails bindings exposed to the frontend (chat, config, memories, reconnect). Also defines the `Event` envelope streamed to the UI via `runtime.EventsEmit("agent:event", ...)`.
- `dashboard.go` — Wails bindings for the Workflows + Events tabs. Thin JSON marshalling layer over `MCPClient` calls.
- `operator.go` — Gemini ADK operator. Owns the system instruction (Rick Sanchez persona + DAG selection rules + tool catalog), MCP toolset wiring, session lifecycle, tool-round cap (`maxToolRoundsPerTurn=25`), periodic core-rules re-injection, memory injection on version change.
- `mcpclient.go` — Minimal JSON-RPC 2.0 HTTP client used by `dashboard.go` to call MCP tools directly (bypasses ADK for query bindings).
- `memory.go` — `MemoryStore` persisting operator memories to `~/.config/rick/memories.json` (Add/List/Delete/Search/FormatForPrompt + monotonic version counter).
- `config.go` — `Config` + `DefaultConfig`. Loads `~/.config/rick/env`, resolves `RICK_SERVER_URL`, `RICK_MODEL`, `GOOGLE_API_KEY` / `GOOGLE_GENAI_API_KEY`.
- `wails.json` — Wails project config: name `rick-agent`, output binary `rick-agent`, frontend npm commands, build tag `webkit2_41`.
- `go.mod` / `go.sum` — Separate module `github.com/marconn/rick-agent` (Go 1.25). Pulls in `wails/v2`, `google.golang.org/adk`, `genai`, `modelcontextprotocol/go-sdk`.
- `*_test.go` — Unit tests for app, dashboard, operator, mcpclient, memory, config.
- `frontend/` — Svelte 5 UI. See `frontend/CLAUDE.md`.
- `build/` — gitignored Wails build output (`build/bin/rick-agent`).
- `rick-agent` — local dev build artifact (do not ship; use the .deb).

## Key Wails bindings (`App` exported methods)

Chat / connection:
- `SendMessage(text)` — async; runs the operator and streams `agent:event` events (`tool_call`, `tool_result`, `response`, `error`, `done`).
- `GetConfig()` → `Config` (API key omitted).
- `CheckConnection()` → bool — pings rick-server.
- `Reconnect()` → error string — re-discovers MCP tools.
- `ClearContext()` → error string — resets ADK session, keeps tools.

Memory:
- `SaveMemory(content, category)` → `*Memory`
- `ListMemories()` → `[]Memory`
- `DeleteMemory(id)` → bool (accepts ID prefix)
- `SearchMemories(query)` → `[]Memory`

Workflow inspection (delegates to MCP tools):
- `ListWorkflows()`, `WorkflowStatus(id)`, `PhaseTimeline(id)`, `TokenUsageForWorkflow(id)`, `WorkflowVerdicts(id)`, `PersonaOutput(id, persona)`
- `ListEvents(workflowID, limit)`, `ListDeadLetters()`

Workflow control:
- `PauseWorkflow(id, reason)`, `CancelWorkflow(id, reason)`, `ResumeWorkflow(id, reason)`
- `ApproveHint(id, persona, guidance)`, `RejectHint(id, persona, reason, action)`
- `InjectGuidance(id, content, target)` (auto-resumes by default)

## Architecture
- Svelte UI → Wails JS bindings → `App` Go methods → either (a) Gemini ADK operator with MCP toolset for chat, or (b) `MCPClient` direct HTTP for dashboard queries → rick-server MCP HTTP at `RICK_SERVER_URL` (default `http://localhost:8077/mcp`).
- The agent UI ONLY accesses Rick through MCP — no direct event store or bus access.
- Operator events stream to the frontend via Wails `runtime.EventsEmit("agent:event", Event{...})`.

## Build / package / install
- Dev: `wails dev` from this directory.
- Build: `wails build` → `build/bin/rick-agent` (do not deploy this manually).
- Package: from repo root, `make package` → `../deploy/rick-agent_<version>_amd64.deb`.
- Install: `make install-agent` (installs the .deb to `/usr/bin/rick-agent`).
- IMPORTANT: rick-agent binary should ONLY come from the .deb at `/usr/bin/rick-agent`. Never copy the build artifact to `~/.local/bin/rick-agent` — that path is reserved for `rick` (the server CLI).

## Env vars (loaded from `~/.config/rick/env`)
- `RICK_SERVER_URL` — MCP HTTP endpoint (default `http://localhost:8077/mcp`).
- `RICK_MODEL` — Gemini model name (default `gemini-2.5-pro`). Detection of `flash` shrinks the thinking budget from 10240 to 4096.
- `GOOGLE_API_KEY` / `GOOGLE_GENAI_API_KEY` — required for the Gemini backend.

## Slash commands
Slash commands are handled client-side in the Svelte frontend — they never reach the operator (zero token cost). Categories:
- Chat: `/clear`, `/help`, `/status`, `/reconnect`, `/config`
- Inspection: `/workflows`, `/deadletters`, `/events`, `/tokens`, `/phases`, `/verdicts`
- Control: `/cancel`, `/pause`, `/resume`
- Hints: `/approve`, `/reject`
- Memory: `/remember [category] <text>`, `/memories`, `/forget <id>`
- Other: `/model`, `/guide <id> <message>`

See `frontend/CLAUDE.md` for the implementation details.

## Related
- `frontend/` — Svelte 5 UI, three-tab layout (Chat, Workflows, Events).
- `../internal/mcp/` — the MCP tool implementations the operator and dashboard call.
- `../deploy/rick-agent.{desktop,svg}` — packaging assets for the .deb.
- `../Makefile` — `package` and `install-agent` targets.
