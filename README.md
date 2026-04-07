# Rick

> **Alpha** — Rick is under active development. APIs, workflow definitions, and the agent UI may change without notice. Use at your own risk.

Rick is an event-sourced AI workflow orchestrator built in Go. It coordinates multiple AI personas (researcher, architect, developer, reviewer, QA, etc.) through DAG-based pipelines to automate software engineering tasks end-to-end — from Jira ticket to committed code.

## How It Works

All state changes are immutable events stored in SQLite. Execution topology is defined declaratively in workflow DAGs — handlers are stateless workers that know nothing about when they fire. The engine dispatches based on the graph, enforces iteration limits, and manages feedback loops automatically.

```
Jira Ticket / Prompt / PR
        |
    WorkflowStarted
        |
   [ workspace ] --> [ context-snapshot ] --> [ developer ]
                                                   |
                                            +------+------+
                                            |             |
                                        [reviewer]      [qa]
                                            |             |
                                         [quality-gate]
                                               |
                                          [committer]
                                               |
                                        WorkflowCompleted
```

The `jira-dev` workflow extends this with `jira-context`, `researcher`, and `architect` phases before the developer.

When a reviewer or QA persona renders a failing verdict, the engine emits `FeedbackGenerated`, re-triggering the developer in a retry loop — until the code passes or the iteration limit is reached.

## Features

- **DAG-based workflow orchestration** — Declare handler dependencies as a directed acyclic graph. The engine computes subscriptions, join conditions, and dispatch order automatically.
- **Event sourcing** — Every state change is an immutable event in SQLite. Full audit trail, deterministic replay, and aggregate-based decision making.
- **Built-in workflows** — `workspace-dev`, `jira-dev`, `pr-review`, `pr-feedback`, `ci-fix`, `plan-btu`, `plan-jira`, `task-creator`, `jira-qa-steps`, and more.
- **Feedback loops** — Failing review verdicts re-trigger the developer automatically. Max iterations and escalation-on-limit keep loops bounded.
- **Hint system** — Two-phase dispatch: handlers can emit a lightweight pre-check (`Hint`) for human review before full execution. Auto-approve above a confidence threshold, or pause for operator input.
- **MCP server** — 46 tools across workflow management, jobs, workspaces, Jira, Confluence, wave orchestration, and observability. Works with Claude Desktop, Cursor, or any MCP-compatible client.
- **gRPC service discovery** — External systems register as handlers via bidirectional streaming. The stream lifecycle is the service discovery — open registers, close deregisters. Reconnecting client with exponential backoff included.
- **Dynamic workflow registration** — External systems can compose custom workflows from any combination of local and gRPC-connected handlers at runtime, no code changes needed.
- **Agent UI** — Desktop application (Wails + Svelte 5) with chat, workflow dashboard, and real-time event stream. Backed by a Gemini ADK operator that calls Rick's MCP tools.
- **Quality gate** — Runs lint and tests in a VM-isolated environment before committing. Disable with `RICK_DISABLE_QUALITY_GATE=1` on machines without VM support.
- **Jira & Confluence integration** — Read tickets, create epics and tasks, transition issues, write plans to Confluence, generate QA steps from PR diffs.
- **Projections** — Real-time read models for workflow status, token usage, phase timeline, and verdicts.

## Architecture

```
cmd/rick/             CLI entrypoint (serve, run)
internal/
  engine/             Workflow aggregate, PersonaRunner (DAG dispatcher), sentinel
  eventstore/         SQLite event store with WAL and optimistic concurrency
  eventbus/           Pub/sub bus with middleware pipeline
  handler/            Handler registry and built-in persona handlers
  backend/            AI backend (Claude, Gemini) via CLI subprocess
  persona/            Prompt builder with embedded templates
  grpchandler/        gRPC server, client, stream dispatcher, notification broker
  mcp/                MCP server (JSON-RPC 2.0 over stdio/HTTP)
  projection/         Read-model projections (status, tokens, phases, verdicts)
agent/                Desktop UI (Wails v2 + Svelte 5)
```

**Key design principles:**
- Handlers return events, never persist or publish directly — the caller owns atomicity.
- Engine handles lifecycle only (started, completed, failed, cancelled). Zero dispatch logic.
- PersonaRunner is the sole dispatcher. Safety guards: self-trigger prevention, chain depth limiting, width limiting, event dedup, join-gate dedup, graceful drain.
- All code lives in `internal/` — no public API exports.

## Quick Start

### Prerequisites

- Go 1.24+
- An AI backend: [Claude CLI](https://docs.anthropic.com/en/docs/claude-cli) or [Gemini CLI](https://github.com/google-gemini/gemini-cli)

### Build & Run

```bash
# Build the binary
go build -o rick ./cmd/rick

# Start the server (HTTP + gRPC)
rick serve --addr :58077 --grpc-addr :59077 --db rick.db --backend claude

# Or with Gemini
rick serve --addr :58077 --grpc-addr :59077 --db rick.db --backend gemini
```

### Run a Workflow

Workflows are triggered via the MCP interface. Connect any MCP-compatible client (Claude Desktop, Cursor, or the agent UI) to `http://localhost:58077` and use tools like:

- `rick_run_workflow` — Start a workflow (`dag=workspace-dev`, `dag=jira-dev`, etc.)
- `rick_workflow_status` — Check progress
- `rick_list_workflows` — List all workflows
- `rick_cancel_workflow` — Cancel a running workflow

### Agent UI

```bash
cd agent && wails build
./build/bin/rick-agent
```

Or install the `.deb` package from `deploy/`.

## Environment Variables

| Variable | Description |
|----------|-------------|
| `RICK_DISABLE_QUALITY_GATE` | Skip VM-based quality checks. Committer depends on `[reviewer, qa]` directly. |
| `JIRA_URL` | Jira instance URL |
| `JIRA_EMAIL` | Jira authentication email |
| `JIRA_TOKEN` | Jira API token |
| `CONFLUENCE_URL` | Confluence instance URL |
| `CONFLUENCE_EMAIL` | Confluence authentication email |
| `CONFLUENCE_TOKEN` | Confluence API token |
| `RICK_REPOS_PATH` | Base path for local repository checkouts |

Set these in `~/.config/rick/env` or your shell environment. Keep this file secure (`chmod 600`) — it contains API tokens.

## Development

```bash
# Run all tests
go test ./...

# Run tests with race detector
go test -race ./...

# Lint
golangci-lint run

# Pre-commit (always run both)
golangci-lint run && go test ./...
```

## Extending Rick

External systems can connect as handlers via gRPC:

```go
client := grpchandler.NewClient(conn, grpchandler.ClientConfig{
    Name:          "my-handler",
    EventTypes:    []string{"persona.completed"},
    AfterPersonas: []string{"developer"},
    Handler: func(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
        // Process event, return results
        return nil, nil
    },
})
client.Run(ctx) // blocks, reconnects automatically
```

See [CLAUDE.md](CLAUDE.md) for the full external system integration guide.

## License

Proprietary. All rights reserved.
