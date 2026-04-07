# cmd/

Binary entry points (Go convention).

## Binaries
- [`rick/`](rick/CLAUDE.md) — the `rick` CLI (cobra commands wired from `internal/cli`)

## Build
- `go build -o rick ./cmd/rick`
- Deploy convention: `go build -o ~/.local/bin/rick ./cmd/rick` (NOT `go install`) so systemd `rick-server.service` picks it up — restart the service after deploy

## Note
- The rick-agent desktop app lives in `../agent/`, not here, because it's a Wails project with its own go.mod-style layout
