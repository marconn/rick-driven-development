package backend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Claude shells out to the Claude CLI binary.
type Claude struct {
	binaryPath string
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

	cmd := exec.CommandContext(ctx, c.binaryPath, args...)
	cmd.Dir = req.WorkDir

	// Clear CLAUDECODE env var so the subprocess doesn't refuse to start
	// when Rick is invoked from inside a Claude Code session.
	cmd.Env = filterEnv(os.Environ(), "CLAUDECODE")

	// Capture output via stream parser.
	var captured bytes.Buffer
	var inner io.Writer = &captured
	if req.Output != nil {
		inner = io.MultiWriter(&captured, req.Output)
	}

	extractor := NewClaudePrintExtractor()
	sw := NewStreamWriter(inner, extractor.ExtractFn(), WithResultCheck(ClaudeCheckResult))
	cmd.Stdout = sw

	if stdinPrompt != "" {
		cmd.Stdin = strings.NewReader(stdinPrompt)
	}
	// Explicitly nil stdin when no stdin prompt — prevents subprocesses from
	// inheriting stdin, which would corrupt MCP's stdio transport.

	if err := cmd.Run(); err != nil {
		_ = sw.Close()
		// Prefer the context error when the deadline tripped — otherwise the
		// caller sees "claude: signal: killed", which is the symptom (we
		// SIGKILL'd the child) not the root cause (we timed out).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("claude: %w (after %s)", ctxErr, time.Since(start))
		}
		return nil, fmt.Errorf("claude: %w", err)
	}
	_ = sw.Close()

	return &Response{
		Output:     captured.String(),
		StopReason: sw.StopReason(),
		Duration:   time.Since(start),
		TokensUsed: extractor.TokensUsed(),
	}, nil
}
