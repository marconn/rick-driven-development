# package backend

Wraps `claude`, `gemini`, `codex`, `antigravity` (`agy`), and `opencode` CLI binaries as a uniform `Backend` interface. Claude/Gemini/Codex parse NDJSON `stream-json` output to capture text and stop reasons; antigravity captures stdout as plain text (no documented structured stream); opencode parses its `--format json` stream only for the session id + error event, then reads the authoritative output back via `opencode export` (its run-mode stdout has a flush race).

## Files
- `backend.go` — `Backend` interface, `Request`/`Response` types, `maxArgSize` (128KB) prompt-via-stdin threshold.
- `factory.go` — `New(name)` single-backend constructor; honors `RICK_CLAUDE_BIN`, `RICK_GEMINI_BIN`, `RICK_CODEX_BIN`, `RICK_ANTIGRAVITY_BIN`, `RICK_OPENCODE_BIN` env overrides. `Catalog` is the single source of truth for known backends (name + bin-env + default-bin); `ResolveBinary(name)` and `Names()` derive from it so the factory switch, valid-names error, and operator listings can't drift. Also exports `NewReviewBackend(names)` which returns a raw backend for len=1 and a `RoundRobin` for len≥2, plus `ParseReviewBackendsEnv` for `RICK_REVIEW_BACKENDS` handling. `DefaultReviewBackends` is `claude,gemini,codex` — antigravity and opencode must be opted in explicitly via `RICK_REVIEW_BACKENDS`.
- `opencode.go` — `Opencode` driver (sst/opencode CLI): `run --format json` with `-c`/`--session <id>` for resume, `--dangerously-skip-permissions` for yolo, prompt as the trailing positional after `--`. No `--system-prompt` flag → folded into the message with the `<system_instructions>` wrapper. **Model gating**: `-m` is forwarded only when `Request.Model` is provider-qualified (`provider/model`); a bare name is dropped (opencode rejects bare names with "Model not found"), so a global `RICK_MODEL` never crashes opencode out of the review rotation. **Output capture is a hybrid**: `opencode run --format json` has a stdout flush-on-dispose race (~1/3 of piped runs drop the assistant `text` event; flash-class models never emit one) and exits 0 even on an API error, so the stream is parsed only for the reliably-flushed `sessionID` (in `step_start`), any `error` event, and a best-effort live tee. The authoritative output is then read via `opencode export <sessionID>` (persisted session DB), taking the last assistant message's `text` parts. No stdin prompt channel exists, so there is no `maxArgSize` fallback — the prompt is always argv. `StopReason`/`TokensUsed` stay empty; `MCPConfig` ignored.
- `round_robin.go` — `RoundRobin` backend: atomic-counter rotation across N backends. Per-`Run` selection, not per-handler. `Name()` returns `round-robin(a,b,c)`. Known gap: `AIRequestSent`/`AIResponseReceived` record the composite name, not the chosen inner backend — per-call attribution requires subprocess logs today.
- `claude.go` — `Claude` driver: `buildArgs` for `-p`/`--system-prompt`/`--continue`/`--resume`/`--mcp-config`/`--dangerously-skip-permissions`; clears `CLAUDECODE` env to avoid nested-session refusal.
- `gemini.go` — `Gemini` driver: combines system + user prompt into `<system_instructions>` XML wrapper (gemini CLI has no system-prompt flag).
- `codex.go` — `Codex` driver: uses `exec` and `exec resume` subcommands with `--json`; wraps system prompt in XML tags like Gemini.
- `antigravity.go` — `Antigravity` driver (Google `agy` CLI): `-p` for single-shot, `--continue` / `--conversation <id>` for resume, `--dangerously-skip-permissions` for yolo, `--print-timeout 30m` to push the CLI's internal print watchdog past rick's outer wall-clock. No `--system-prompt` flag → folded into the prompt with the same `<system_instructions>` wrapper as Gemini. **No model flag**: agy v1.0.3 rejects both `-m` and `--model` ("flags provided but not defined", exit 2) — the model is fixed by the logged-in Antigravity session, so `Request.Model` is silently ignored (forwarding it crashed every model-bearing call). Stdout captured as plain text (no NDJSON envelope at integration time) so `StopReason` and `TokensUsed` stay empty. `MCPConfig` ignored.
- `stream.go` — `StreamWriter` (io.Writer) buffers + splits NDJSON lines, calls `ExtractFn` per line, optional `CheckResultFn` via `WithResultCheck`.
- `stream_claude.go` — `ExtractClaudeText` / `NewClaudePrintExtractor` / `ClaudeCheckResult`; handles both legacy flat events and `stream_event` envelope from `--include-partial-messages`.
- `stream_gemini.go` — `ExtractGeminiText` / `GeminiCheckResult` (gemini exposes no stop_reason yet, returns "").
- `stream_codex.go` — `NewCodexExtractor`; handles `item.completed` events for text extraction and `turn.completed` for token usage.
- `structured.go` — `ExtractJSON(output)`: pulls JSON from fenced code blocks, then falls back to scanning for first valid `{...}`/`[...]` in raw text.

## Key types
- `Backend` — interface: `Name()`, `Run(ctx, Request) (*Response, error)`.
- `Request` — `SystemPrompt`, `UserPrompt`, `Model`, `WorkDir`, `Yolo`, `MCPConfig`, `SessionID` (`""` new / `"latest"` continue / specific id resume), `Output` (optional tee for streaming), `Effort` (Claude CLI `--effort` reasoning level: `low`/`medium`/`high`/`xhigh`/`max`; empty falls back to `"high"`; ignored by Gemini and Codex — no equivalent flag).
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
