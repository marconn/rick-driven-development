# cmd/rick

Binary entry point for the `rick` CLI — a thin `main` that delegates to the cobra command tree in `internal/cli`.

## Files
- `main.go` — package main entry (15 lines)

## Build
- `go build -o rick ./cmd/rick`
- Deploy convention: `go build -o ~/.local/bin/rick ./cmd/rick` (NOT `go install`) so the systemd user service picks it up
- Always restart `rick-server` systemd unit after rebuilding, otherwise changes don't take effect

## What it does
- Calls `cli.New()` to construct the root cobra command (with all subcommands wired)
- Calls `.Execute()` on it
- On error: prints to stderr and exits with code 1

## Notes
- Zero business logic here — all command wiring lives in `internal/cli`
- No version vars, no flag parsing, no init() — keep it that way
- The `rick run` subcommand is deprecated; primary execution mode is `rick serve` (MCP + gRPC) driven by the agent UI

## Related
- `../../internal/cli` — cobra command tree (root command, subcommands, flag wiring)
- `../../CLAUDE.md` — repo-level architecture and conventions
