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
	// stream activity early instead of going silent for >2m of internal
	// reasoning that the watchdog can't see. Workaround for
	// anthropics/claude-code#20127: in CLI v2.1.8+, stream-json silently
	// drops thinking blocks, so a model that decides to think before
	// generating any text_delta looks identical to a wedged subprocess to
	// our idle watchdog (internal/backend/idle_watchdog.go). Pinning a
	// finite budget bounds the silent window — the model has to commit to
	// emitting something within the cap rather than drifting on adaptive
	// thinking. 31999 keeps us one token under the 32K Anthropic ceiling.
	// --effort is the belt-and-suspenders companion: --max-thinking-tokens
	// is a hidden/undocumented flag (absent from `claude --help` on 2.1.117)
	// and could disappear in a future CLI release, while --effort is
	// documented and maps to the same reasoning-budget concept. Pinning
	// "high" keeps us on the upper end of the documented scale without
	// going to "xhigh"/"max" which would make the silent-thinking window
	// worse than adaptive.
	args = append(args, "--max-thinking-tokens", "31999", "--effort", "high")

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

	cmd := exec.CommandContext(watchCtx, c.binaryPath, args...)
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

	// Thread progress into the extractor rather than wrapping raw stdout.
	// Claude CLI emits stream_event envelopes, tool_use blocks, and keep-alive
	// frames that would reset the idle watchdog without ever producing text if
	// we used newProgressWriter on the raw byte stream. By wiring progress
	// into the extractor, only genuine text deltas (content_block_delta →
	// text_delta) and terminal result/message_delta events count — protocol
	// noise never resets the timer.
	extractor := NewClaudePrintExtractor(WithProgress(progress))
	sw := NewStreamWriter(inner, extractor.ExtractFn(), WithResultCheck(ClaudeCheckResult))
	cmd.Stdout = sw

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
