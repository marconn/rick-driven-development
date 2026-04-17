# package backend

Wraps `claude`, `gemini`, and `codex` CLI binaries as a uniform `Backend` interface, parsing their NDJSON `stream-json` output to capture text and stop reasons.

## Files
- `backend.go` — `Backend` interface, `Request`/`Response` types, `maxArgSize` (128KB) prompt-via-stdin threshold.
- `factory.go` — `New(name)` single-backend constructor; honors `RICK_CLAUDE_BIN`, `RICK_GEMINI_BIN`, `RICK_CODEX_BIN` env overrides. Also exports `NewReviewBackend(names)` which returns a raw backend for len=1 and a `RoundRobin` for len≥2, plus `ParseReviewBackendsEnv` for `RICK_REVIEW_BACKENDS` handling.
- `round_robin.go` — `RoundRobin` backend: atomic-counter rotation across N backends. Per-`Run` selection, not per-handler. `Name()` returns `round-robin(a,b,c)`. Known gap: `AIRequestSent`/`AIResponseReceived` record the composite name, not the chosen inner backend — per-call attribution requires subprocess logs today.
- `claude.go` — `Claude` driver: `buildArgs` for `-p`/`--system-prompt`/`--continue`/`--resume`/`--mcp-config`/`--dangerously-skip-permissions`; clears `CLAUDECODE` env to avoid nested-session refusal.
- `gemini.go` — `Gemini` driver: combines system + user prompt into `<system_instructions>` XML wrapper (gemini CLI has no system-prompt flag).
- `codex.go` — `Codex` driver: uses `exec` and `exec resume` subcommands with `--json`; wraps system prompt in XML tags like Gemini.
- `stream.go` — `StreamWriter` (io.Writer) buffers + splits NDJSON lines, calls `ExtractFn` per line, optional `CheckResultFn` via `WithResultCheck`.
- `stream_claude.go` — `ExtractClaudeText` / `NewClaudePrintExtractor` / `ClaudeCheckResult`; handles both legacy flat events and `stream_event` envelope from `--include-partial-messages`.
- `stream_gemini.go` — `ExtractGeminiText` / `GeminiCheckResult` (gemini exposes no stop_reason yet, returns "").
- `stream_codex.go` — `NewCodexExtractor`; handles `item.completed` events for text extraction and `turn.completed` for token usage.
- `structured.go` — `ExtractJSON(output)`: pulls JSON from fenced code blocks, then falls back to scanning for first valid `{...}`/`[...]` in raw text.

## Key types
- `Backend` — interface: `Name()`, `Run(ctx, Request) (*Response, error)`.
- `Request` — `SystemPrompt`, `UserPrompt`, `Model`, `WorkDir`, `Yolo`, `MCPConfig`, `SessionID` (`""` new / `"latest"` continue / specific id resume), `Output` (optional tee for streaming).
- `Response` — `Output` (full captured text), `StopReason`, `Duration`.
- `StreamWriter` — io.Writer that splits NDJSON, applies `ExtractFn` and optional `CheckResultFn`; `Close()` flushes trailing partial line.
- `ExtractFn` / `CheckResultFn` — per-line text extractor and stop-reason inspector.

## Patterns
- Prompts larger than `maxArgSize` (128KB) are piped via stdin instead of argv to avoid `ARG_MAX`. Otherwise stdin is left nil so subprocess MCP stdio transport isn't corrupted.
- Resuming a session (`SessionID != ""`) skips re-sending the system prompt — original session already has it.
- Output is always captured into a `bytes.Buffer`; if `Request.Output` is set, an `io.MultiWriter` tees extracted text to it for live streaming.
- `NewClaudePrintExtractor` is stateful: tracks `sawText` so the final `result` event's text is only emitted as a fallback when no incremental text deltas were observed (avoids duplication).
- `filterEnv` (claude.go) strips env vars by key prefix; only used to drop `CLAUDECODE`.
- `ExtractJSON` strategy order: fenced ```json block, then fenced ``` block, then first parseable JSON token in the raw text.

## Related
- `../persona` — `PromptBuilder` assembles the `Request.SystemPrompt` / `UserPrompt` that handlers feed into `Backend.Run`.
- `../handler` — persona handlers call `Backend.Run` and emit the captured `Response.Output` as event payloads.
