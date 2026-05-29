package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Claude shells out to the Claude CLI binary.
type Claude struct {
	binaryPath   string
	stallTimeout time.Duration // 0 = no idle watchdog (default)
	// progressTimeout arms the completion-progress watchdog: the subprocess is
	// killed if it produces no assistant text delta within the window even
	// while raw stdout stays active. 0 = disabled (default). Set from
	// RICK_BACKEND_PROGRESS_TIMEOUT via the factory.
	progressTimeout time.Duration
}

// NewClaude creates a Claude backend. binaryPath is the path to the `claude` CLI binary.
func NewClaude(binaryPath string) *Claude {
	return &Claude{binaryPath: binaryPath}
}

func (c *Claude) Name() string { return "claude" }

// buildArgs returns CLI arguments and, when the user prompt exceeds maxArgSize,
// the prompt content to pipe via stdin (avoiding OS ARG_MAX limits).
func (c *Claude) buildArgs(req Request) (args []string, stdinPrompt string) {
	args = append(args, "-p")

	// Skip system prompt when resuming — the original session already has it.
	if req.SessionID == "" {
		args = append(args, "--system-prompt", req.SystemPrompt)
	}

	switch req.SessionID {
	case "":
		// New session, nothing to add.
	case "latest":
		args = append(args, "--continue")
	default:
		args = append(args, "--resume", req.SessionID)
	}

	// Always use stream-json for structured parsing of the output.
	args = append(args, "--output-format", "stream-json", "--verbose", "--include-partial-messages")

	// Force a fixed extended-thinking budget so the model emits visible
	// stream activity early instead of going silent for an unbounded
	// internal-reasoning window. Workaround for
	// anthropics/claude-code#20127: in CLI v2.1.8+, stream-json silently
	// drops thinking blocks, so a model that decides to think before
	// emitting any other stdout looks like a wedged subprocess. Pinning a
	// finite budget bounds the silent window regardless of the effort
	// level the caller picks — even at --effort xhigh/max the silent
	// window stays capped because --max-thinking-tokens is the hard
	// ceiling. --effort is the documented companion knob; persona-specific
	// values come from internal/handler personaEffort. Empty Effort keeps
	// the historical default "high" so handlers without an override see
	// no behavior change.
	effort := req.Effort
	if effort == "" {
		effort = "high"
	}
	args = append(args, "--max-thinking-tokens", "31999", "--effort", effort)

	if req.Yolo {
		args = append(args, "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions")
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.MCPConfig != "" {
		args = append(args, "--mcp-config", req.MCPConfig)
	}
	if req.UserPrompt != "" {
		if len(req.UserPrompt) > maxArgSize {
			stdinPrompt = req.UserPrompt
		} else {
			args = append(args, req.UserPrompt)
		}
	}

	return args, stdinPrompt
}

// filterEnv returns env without entries starting with any of the given keys.
func filterEnv(env []string, keys ...string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		skip := false
		for _, k := range keys {
			if strings.HasPrefix(e, k+"=") {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, e)
		}
	}
	return out
}

func (c *Claude) Run(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()
	args, stdinPrompt := c.buildArgs(req)

	// Idle watchdog: kills the subprocess fast when it stops producing any
	// output. Complements the caller's wall-clock timeout — a wedged CLI now
	// dies in ~stallTimeout instead of tying up a concurrency slot for the
	// full backend-timeout budget.
	watchCtx, progress, stopWatch := WithIdleTimeout(ctx, c.stallTimeout)
	defer stopWatch()

	// Completion-progress watchdog (default-off): stacked on top of the byte
	// idle watchdog as a longer, looser net. Its progress() is driven ONLY by
	// assistant text deltas (wired into the extractor below), not raw bytes, so
	// it catches a CLI that is alive and chattering tool-use frames but never
	// converging to an answer — the failure the byte watchdog is deliberately
	// blind to (it counts any stdout byte as progress). progCtx is the exec
	// context; on parent cancellation context.Cause propagates, so the error
	// block below disambiguates progress vs. idle vs. wall-clock by cause.
	progCtx, textProgress, stopProgress := WithProgressTimeout(watchCtx, c.progressTimeout)
	defer stopProgress()

	cmd := exec.CommandContext(progCtx, c.binaryPath, args...)
	cmd.Dir = req.WorkDir
	// Kill the whole process group on ctx cancel, not just the direct
	// child — claude CLI forks a node subprocess that would otherwise
	// survive SIGKILL and keep stdio pipes open, blocking cmd.Wait.
	configureProcessGroup(cmd, defaultKillGraceDelay)

	// Clear CLAUDECODE env var so the subprocess doesn't refuse to start
	// when Rick is invoked from inside a Claude Code session.
	cmd.Env = filterEnv(os.Environ(), "CLAUDECODE")

	// Capture output via stream parser.
	var captured bytes.Buffer
	var inner io.Writer = &captured
	if req.Output != nil {
		inner = io.MultiWriter(&captured, req.Output)
	}

	// Wire progress on raw stdout — any byte the CLI emits counts. Earlier
	// revisions wired progress into the extractor and required a non-empty
	// content_block_delta.text_delta or a terminal event, intending to ignore
	// "protocol noise". In Claude CLI 2.1.x this misclassifies real work:
	// while a tool_use is executing (Bash / Edit / Task / MCP), the parent
	// stdout emits content_block_start, input_json_delta, content_block_stop,
	// system.task_started/notification, and user/tool_result events — none of
	// which the extractor counted. A multi-minute tool run therefore looked
	// identical to a wedged subprocess and got SIGKILL'd before its first
	// text_delta. The CLI's own internal stream watchdog
	// (CLAUDE_CODE_STREAM_TIMEOUT, 45s default; full 5min abort+retry since
	// 2.1.105) is the authoritative liveness signal — our outer watchdog is
	// just the safety net that fires when the CLI itself has hung. Counting
	// every stdout byte gives the CLI room to drive its own retries.
	extractor := NewClaudePrintExtractor()
	// The byte watchdog (progress) taps raw stdout below. The completion
	// watchdog (textProgress) taps the extractor: it fires only when a line
	// yields a non-empty assistant text delta — the one signal that the model
	// is producing answer content rather than tool-use protocol noise. A
	// tool-loop wedge keeps raw stdout (and the byte watchdog) alive but never
	// trips this, so the longer progressTimeout eventually kills it. Wrapping
	// the extractor (not duplicating parse logic) keeps token/stop-reason
	// accounting in one place.
	baseExtract := extractor.ExtractFn()
	extractFn := func(line []byte) (string, bool) {
		text, ok := baseExtract(line)
		if ok && text != "" {
			textProgress()
		}
		return text, ok
	}
	sw := NewStreamWriter(inner, extractFn, WithResultCheck(ClaudeCheckResult))
	cmd.Stdout = newProgressWriter(sw, progress)

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if stdinPrompt != "" {
		cmd.Stdin = strings.NewReader(stdinPrompt)
	}
	// When no stdin prompt is being piped, wire /dev/null explicitly so the
	// claude CLI's startup stdin probe (~3s wait for input) sees EOF
	// immediately instead of hanging on an inherited/ambiguous fd under
	// systemd. See wireNullStdin in backend.go.
	closeStdin := wireNullStdin(cmd)
	defer closeStdin()

	if err := cmd.Run(); err != nil {
		_ = sw.Close()
		stderr := captureErrorTail(stderrBuf.String(), captured.String())
		elapsed := time.Since(start)
		// Completion-progress watchdog fired: the CLI streamed bytes but no
		// answer text within the window. Checked before ErrIdleTimeout because
		// progCtx is the exec context — when the byte watchdog fires on the
		// parent (watchCtx), context.Cause(progCtx) propagates ErrIdleTimeout,
		// so this branch is reached only for a genuine progress timeout.
		if errors.Is(context.Cause(progCtx), ErrProgressTimeout) {
			return nil, &BackendError{
				Backend:  "claude",
				Inner:    fmt.Errorf("%w (window=%s)", ErrProgressTimeout, c.progressTimeout),
				Duration: elapsed,
				Stderr:   stderr,
			}
		}
		if errors.Is(context.Cause(watchCtx), ErrIdleTimeout) {
			return nil, &BackendError{
				Backend:  "claude",
				Inner:    fmt.Errorf("%w (stall=%s)", ErrIdleTimeout, c.stallTimeout),
				Duration: elapsed,
				Stderr:   stderr,
			}
		}
		// Prefer the context error when the deadline tripped — otherwise the
		// caller sees "signal: killed", which is the symptom (we SIGKILL'd
		// the child) not the root cause (we timed out).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, &BackendError{
				Backend:  "claude",
				Inner:    ctxErr,
				Duration: elapsed,
				Stderr:   stderr,
			}
		}
		return nil, &BackendError{
			Backend:  "claude",
			Inner:    err,
			Duration: elapsed,
			Stderr:   stderr,
		}
	}
	_ = sw.Close()

	return &Response{
		Output:     captured.String(),
		StopReason: sw.StopReason(),
		Duration:   time.Since(start),
		TokensUsed: extractor.TokensUsed(),
	}, nil
}
