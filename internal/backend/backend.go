package backend

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Backend is the interface for AI LLM provider drivers.
// Implementations shell out to CLI binaries (claude, gemini) and capture
// the full response for event-driven handler consumption.
type Backend interface {
	// Name returns the backend identifier (e.g., "claude", "gemini").
	Name() string

	// Run executes an AI request and returns the captured response.
	// The full LLM output is captured regardless of whether streaming
	// is enabled via Request.Output.
	Run(ctx context.Context, req Request) (*Response, error)
}

// Selector is implemented by backends that pick a concrete inner backend per
// call (RoundRobin). Resolving the concrete backend before Run lets callers
// attribute the events emitted before dispatch (AIRequestSent /
// AIRequestStarted) to the CLI that actually executes, not a composite
// rotation name.
type Selector interface {
	// Select returns the backend a subsequent Run with the same ctx would
	// dispatch to. It may advance rotation state (see RoundRobin.Select), so
	// the caller MUST invoke the returned backend rather than calling Run on
	// the Selector afterwards.
	Select(ctx context.Context) Backend
}

// Resolve returns the concrete backend b would dispatch to for ctx. When b is a
// Selector (RoundRobin), it returns the selected inner backend; otherwise b
// itself. Callers invoke the returned backend's Run directly so that the
// per-call backend identity is known before any pre-dispatch event is emitted.
//
// In the review-rotation construction the rotation members are the
// concurrency-limited wrappers (RoundRobin over limitedBackend), so the
// returned backend still enforces its limiter on Run — Resolve never bypasses
// concurrency control.
func Resolve(ctx context.Context, b Backend) Backend {
	if s, ok := b.(Selector); ok {
		return s.Select(ctx)
	}
	return b
}

// Request configures an AI backend execution.
type Request struct {
	SystemPrompt string    // LLM system prompt (persona instructions).
	UserPrompt   string    // User/task prompt.
	Model        string    // Optional model override (e.g., "claude-haiku-4-5-20251001").
	WorkDir      string    // Working directory for backend execution.
	Yolo         bool      // Skip permission checks (Claude: --dangerously-skip-permissions, Gemini: --yolo).
	MCPConfig    string    // JSON MCP server config (passed via --mcp-config for Claude).
	SessionID    string    // "latest" to continue most recent, or a specific session ID to resume.
	Output       io.Writer // Optional: stream extracted text here in real-time (tee'd with capture buffer).
	// Effort sets the Claude CLI --effort reasoning level. Valid values
	// follow the Claude CLI (low / medium / high / xhigh / max). Empty
	// keeps the historical default ("high"). Ignored by Gemini and Codex —
	// neither CLI exposes an equivalent knob; per-persona reasoning tuning
	// only takes effect on the claude backend.
	Effort string
}

// Response captures the result of an AI backend execution.
type Response struct {
	Output     string        // Full captured text output from the LLM.
	StopReason string        // "end_turn", "max_tokens", etc. Empty if not captured.
	Duration   time.Duration // Wall clock execution duration.
	// TokensUsed is the total token count for the request (input + output +
	// cache_creation_input + cache_read_input). Sourced from the authoritative
	// "result" event in the stream-json output; falls back to the last-seen
	// message_start + message_delta counters if no result arrives. Zero when
	// the backend does not report usage (e.g., Gemini — handled separately).
	TokensUsed int
	// SessionID is the CLI session/thread id the run produced, for a later
	// resume (codex `thread.started.thread_id`, claude `session_id`). Empty
	// when the backend exposes none (gemini, antigravity at integration time).
	// Callers persist it (AIResponsePayload.SessionID) and feed it back via
	// Request.SessionID to continue the same session.
	SessionID string
}

// maxArgSize is the threshold above which prompts are piped via stdin
// instead of passed as CLI arguments, to avoid OS ARG_MAX limits.
// Set conservatively at 128KB (ARG_MAX is ~2MB on Linux, but we share
// the budget with other args, env vars, and the binary path).
const maxArgSize = 128 << 10

// MaxStderrCapture caps how many trailing bytes of subprocess stderr a
// BackendError carries. Event store payloads are JSON-marshalled into
// SQLite rows — capping at 4KB keeps per-failure overhead bounded while
// still giving operators enough context to diagnose a crash.
const MaxStderrCapture = 4 << 10

// stdoutFallbackPrefix marks a BackendError.Stderr value that was populated
// from a subprocess's stdout tail rather than its stderr stream. Claude CLI
// is the primary case: on non-zero exit it emits the error payload as a
// JSON event on stdout and leaves stderr empty, which historically
// produced a bare "exit status 1" with no operator-visible cause
// (hulilabs/huli#802 pr-data failure, 2026-04-24). Downstream consumers
// (aggregate, projection, UI) should treat anything with this prefix as
// "last-resort diagnostic tail", not as "authoritative stderr".
const stdoutFallbackPrefix = "[no stderr; stdout tail]\n"

// captureErrorTail returns a best-effort diagnostic tail for a failed
// subprocess. Preference order:
//  1. trailing stderr bytes (up to MaxStderrCapture) — the authoritative
//     signal for most CLIs (gemini, codex),
//  2. trailing stdout bytes (claude emits errors to stdout JSON under
//     `--output-format stream-json`; stderr is usually empty),
//  3. empty string when both are empty (idle-timeout silent-stall case).
//
// Exported intentionally so driver implementations share one failure-tail
// policy and the stdout-fallback marker stays in sync.
func captureErrorTail(stderr, stdout string) string {
	if tail := tailBytes(stderr, MaxStderrCapture); tail != "" {
		return tail
	}
	if tail := tailBytes(stdout, MaxStderrCapture); tail != "" {
		return stdoutFallbackPrefix + tail
	}
	return ""
}

// BackendError wraps a driver failure with separately-accessible subprocess
// stderr and a stable classifiable Inner error. Drivers return this on any
// Run failure so upstream code (handlers, PersonaRunner) can:
//   - Use errors.Is(err, ErrIdleTimeout) to detect a wedged subprocess.
//   - Use errors.Is(err, context.DeadlineExceeded) to detect a wall-clock timeout.
//   - Use errors.Is(err, context.Canceled) to detect operator cancellation.
//   - Use errors.As(err, &backendErr) to pull the captured Stderr tail.
//
// Stderr may be empty when the subprocess was killed before producing any
// output (the idle-watchdog case). Duration reflects how long Run had been
// executing when it failed, so diagnostics do not have to re-derive it.
type BackendError struct {
	// Backend names the driver that failed (e.g., "claude", "gemini").
	Backend string
	// Inner is the underlying cause. Preserving it separately (rather than
	// just formatting into a string) keeps errors.Is working against
	// sentinel values (ErrIdleTimeout, context.DeadlineExceeded, etc.).
	Inner error
	// Duration is how long Run had been executing when it failed.
	Duration time.Duration
	// Stderr holds the captured tail of subprocess stderr, truncated to
	// MaxStderrCapture bytes. Empty when none was captured (silent stall).
	Stderr string
}

// Error renders a human-readable message. Format is stable: it preserves
// the "<backend>: <cause>" prefix established by prior versions so logs
// and existing substring matches keep working.
func (e *BackendError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", e.Backend, e.Inner.Error())
	if e.Duration > 0 {
		fmt.Fprintf(&b, " (after %s)", e.Duration.Round(time.Millisecond))
	}
	if stderr := strings.TrimSpace(e.Stderr); stderr != "" {
		// Keep message short — full stderr lives in the struct field.
		snippet := stderr
		if len(snippet) > 256 {
			snippet = snippet[len(snippet)-256:]
		}
		fmt.Fprintf(&b, ": %s", snippet)
	}
	return b.String()
}

// Unwrap returns the wrapped cause so errors.Is / errors.As traverse through.
func (e *BackendError) Unwrap() error { return e.Inner }

// wireNullStdin ensures cmd.Stdin is explicitly /dev/null when no stdin
// prompt is being piped. Go's exec package DOES open os.DevNull itself
// when cmd.Stdin is nil, so functionally this is a defensive belt-and-
// suspenders — but being explicit removes any ambiguity for CLIs that
// probe stdin at startup (confirmed for Claude CLI: ~3s stdin probe per
// H3 repro 2026-04-20) and matches the environment of direct terminal
// invocation, where shells pass /dev/null to backgrounded processes.
//
// Returns a cleanup func the caller MUST defer; the opened file descriptor
// must be closed after cmd.Run returns or it leaks. When os.Open fails
// (extremely rare: exhausted fds, exotic sandbox), we fall through and
// leave cmd.Stdin nil — Go's exec will then open /dev/null on its own,
// so behavior is never worse than before.
func wireNullStdin(cmd *exec.Cmd) func() {
	if cmd.Stdin != nil {
		return func() {} // caller is piping a real prompt via stdin
	}
	f, err := os.Open(os.DevNull)
	if err != nil {
		return func() {}
	}
	cmd.Stdin = f
	return func() { _ = f.Close() }
}

// tailBytes returns the trailing n bytes of s. When s is shorter than n,
// s is returned as-is. When truncated, a leading "...[truncated]..."
// marker is prepended so readers do not mistake the tail for the whole.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const marker = "...[truncated]...\n"
	if n <= len(marker) {
		return s[len(s)-n:]
	}
	return marker + s[len(s)-(n-len(marker)):]
}
