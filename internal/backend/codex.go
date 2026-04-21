package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Codex shells out to the Codex CLI binary.
type Codex struct {
	binaryPath   string
	stallTimeout time.Duration // 0 = no idle watchdog (default)
}

// NewCodex creates a Codex backend. binaryPath is the path to the `codex` CLI binary.
func NewCodex(binaryPath string) *Codex {
	return &Codex{binaryPath: binaryPath}
}

func (c *Codex) Name() string { return "codex" }

// buildArgs returns CLI arguments and, when the user prompt exceeds maxArgSize,
// the prompt content to pipe via stdin (avoiding OS ARG_MAX limits).
func (c *Codex) buildArgs(req Request) (args []string, stdinPrompt string) {
	// If resuming, use 'exec resume'. Otherwise use 'exec'.
	if req.SessionID != "" {
		args = append(args, "exec", "resume")
		if req.SessionID == "latest" {
			args = append(args, "--last")
		} else {
			args = append(args, req.SessionID)
		}
	} else {
		args = append(args, "exec")
	}

	// System prompt integration. Codex doesn't have a direct --system-prompt flag
	// in 'exec' mode that I saw, but we can prepend it.
	// Actually, let's check if it has a way to set system instructions.
	// If not, we'll wrap it like Gemini.

	// Always use --json for structured parsing of the output.
	args = append(args, "--json")

	if req.Yolo {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}

	// Prompt handling
	prompt := req.UserPrompt
	if req.SessionID == "" && req.SystemPrompt != "" {
		prompt = fmt.Sprintf("<system_instructions>\n%s\n</system_instructions>\n\n%s", req.SystemPrompt, req.UserPrompt)
	}

	if len(prompt) > maxArgSize {
		stdinPrompt = prompt
	} else if prompt != "" {
		args = append(args, prompt)
	}

	return args, stdinPrompt
}

func (c *Codex) Run(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()
	args, stdinPrompt := c.buildArgs(req)

	watchCtx, progress, stopWatch := WithIdleTimeout(ctx, c.stallTimeout)
	defer stopWatch()

	cmd := exec.CommandContext(watchCtx, c.binaryPath, args...)
	cmd.Dir = req.WorkDir
	configureProcessGroup(cmd, defaultKillGraceDelay)

	// Capture output via stream parser.
	var captured bytes.Buffer
	var inner io.Writer = &captured
	if req.Output != nil {
		inner = io.MultiWriter(&captured, req.Output)
	}

	extractor := NewCodexExtractor()
	sw := NewStreamWriter(inner, extractor.ExtractFn()) // CodexCheckResult could be added if needed
	cmd.Stdout = newProgressWriter(sw, progress)

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if stdinPrompt != "" {
		cmd.Stdin = strings.NewReader(stdinPrompt)
	}
	// Explicit /dev/null for non-piped invocations — see wireNullStdin doc.
	closeStdin := wireNullStdin(cmd)
	defer closeStdin()

	if err := cmd.Run(); err != nil {
		_ = sw.Close()
		stderr := tailBytes(stderrBuf.String(), MaxStderrCapture)
		elapsed := time.Since(start)
		if errors.Is(context.Cause(watchCtx), ErrIdleTimeout) {
			return nil, &BackendError{
				Backend:  "codex",
				Inner:    fmt.Errorf("%w (stall=%s)", ErrIdleTimeout, c.stallTimeout),
				Duration: elapsed,
				Stderr:   stderr,
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, &BackendError{
				Backend:  "codex",
				Inner:    ctxErr,
				Duration: elapsed,
				Stderr:   stderr,
			}
		}
		return nil, &BackendError{
			Backend:  "codex",
			Inner:    err,
			Duration: elapsed,
			Stderr:   stderr,
		}
	}
	_ = sw.Close()

	return &Response{
		Output:     captured.String(),
		Duration:   time.Since(start),
		TokensUsed: extractor.TokensUsed(),
	}, nil
}
