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

// Antigravity shells out to Google's Antigravity CLI binary (`agy`).
//
// Argv shape (verified against `agy --help`, v1.0.3):
//   - `-p` / `--print`        : single-shot prompt, non-interactive
//   - `--continue` / `-c`     : continue the most recent conversation
//   - `--conversation <id>`   : resume a specific conversation by ID
//   - `--dangerously-skip-permissions` : auto-approve tool requests
//   - `--print-timeout <dur>` : caps print-mode wait (CLI default 5m,
//     extended here to 30m so the outer rick wall-clock budget — 20m
//     developer / 15m review — is the binding deadline, not the CLI's
//     internal cutoff)
//
// Unsupported (no flag exists on the CLI):
//   - model override → agy has NO `-m` / `--model` flag (both are rejected
//     with "flags provided but not defined", exit 2, before any work).
//     The model is chosen out-of-band from the logged-in Antigravity
//     desktop session. Request.Model is therefore ignored here rather than
//     forwarded — forwarding it crashed every call that set a model (any
//     RICK_MODEL operator, any rick_consult/rick_run model arg). Silent
//     ignore (vs error) is deliberate: a single global RICK_MODEL must not
//     take antigravity out of a mixed review-backend rotation.
//   - separate `--system-prompt` → folded into the prompt body inside a
//     <system_instructions> wrapper, matching the Gemini driver.
//   - structured `--output-format stream-json` → stdout is captured as
//     plain text. StopReason and TokensUsed stay empty; per-call token
//     accounting requires future CLI support.
//   - `--mcp-config` → MCPConfig on the Request is ignored. If/when agy
//     adds an MCP flag, wire it here.
type Antigravity struct {
	binaryPath   string
	stallTimeout time.Duration // 0 = no idle watchdog (default)
}

// NewAntigravity creates an Antigravity backend. binaryPath is the path to
// the `agy` CLI binary.
func NewAntigravity(binaryPath string) *Antigravity {
	return &Antigravity{binaryPath: binaryPath}
}

func (a *Antigravity) Name() string { return "antigravity" }

// Capabilities: antigravity resumes conversations (--continue / --conversation).
// It has no system-prompt flag (folded into a <system_instructions> wrapper),
// no MCP tool retrieval, captures stdout as plain text (no token usage), and
// has no reasoning-effort knob.
func (a *Antigravity) Capabilities() Capabilities {
	return Capabilities{SessionResume: true}
}

func (a *Antigravity) combinePrompt(systemPrompt, userPrompt string) string {
	return fmt.Sprintf("<system_instructions>\n%s\n</system_instructions>\n\n%s", systemPrompt, userPrompt)
}

// printTimeout is the value passed to `--print-timeout`. Chosen larger than
// either rick wall-clock cap (developer 20m, review 15m) so the outer
// context deadline wins; agy's own print watchdog should never be the
// reason a Run fails.
const antigravityPrintTimeout = 30 * time.Minute

// buildArgs returns CLI arguments and, when the prompt exceeds maxArgSize,
// the prompt content to pipe via stdin (avoiding OS ARG_MAX limits).
func (a *Antigravity) buildArgs(req Request) (args []string, stdinPrompt string) {
	// Session handling first so the prompt-only path stays simple. agy uses
	// `--conversation <id>` for a specific session and `--continue` for
	// "most recent" — matching our SessionID convention ("latest" → continue,
	// any other non-empty value → conversation).
	switch req.SessionID {
	case "":
		// new session — no resume flag
	case "latest":
		args = append(args, "--continue")
	default:
		args = append(args, "--conversation", req.SessionID)
	}

	// When resuming, the original session already has the system prompt
	// loaded — sending it again would prepend a duplicate.
	var combined string
	if req.SessionID != "" {
		combined = req.UserPrompt
	} else {
		combined = a.combinePrompt(req.SystemPrompt, req.UserPrompt)
	}

	if len(combined) > maxArgSize {
		stdinPrompt = combined
		args = append(args, "-p")
	} else {
		args = append(args, "-p", combined)
	}

	if req.Yolo {
		args = append(args, "--dangerously-skip-permissions")
	}
	// NOTE: Request.Model is intentionally NOT forwarded — agy has no model
	// flag and rejects `-m`/`--model` outright (see type doc). The model is
	// fixed by the logged-in Antigravity session.

	// Push the CLI's internal print watchdog past our outer wall-clock cap
	// so context cancellation is what actually surfaces on a stall, not a
	// CLI-side 5-minute cutoff.
	args = append(args, "--print-timeout", antigravityPrintTimeout.String())

	return args, stdinPrompt
}

func (a *Antigravity) Run(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()
	args, stdinPrompt := a.buildArgs(req)

	watchCtx, progress, stopWatch := WithIdleTimeout(ctx, a.stallTimeout)
	defer stopWatch()

	cmd := exec.CommandContext(watchCtx, a.binaryPath, args...)
	cmd.Dir = req.WorkDir
	configureProcessGroup(cmd, defaultKillGraceDelay)

	// agy emits plain text on stdout in --print mode (no documented NDJSON
	// envelope), so capture raw bytes directly — no StreamWriter / extractor
	// in the pipeline.
	var captured bytes.Buffer
	var sink io.Writer = &captured
	if req.Output != nil {
		sink = io.MultiWriter(&captured, req.Output)
	}
	cmd.Stdout = newProgressWriter(sink, progress)

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if stdinPrompt != "" {
		cmd.Stdin = strings.NewReader(stdinPrompt)
	}
	// Explicit /dev/null for non-piped invocations — see wireNullStdin doc.
	closeStdin := wireNullStdin(cmd)
	defer closeStdin()

	if err := cmd.Run(); err != nil {
		stderr := captureErrorTail(stderrBuf.String(), captured.String())
		elapsed := time.Since(start)
		if errors.Is(context.Cause(watchCtx), ErrIdleTimeout) {
			return nil, &BackendError{
				Backend:  "antigravity",
				Inner:    fmt.Errorf("%w (stall=%s)", ErrIdleTimeout, a.stallTimeout),
				Duration: elapsed,
				Stderr:   stderr,
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, &BackendError{
				Backend:  "antigravity",
				Inner:    ctxErr,
				Duration: elapsed,
				Stderr:   stderr,
			}
		}
		return nil, &BackendError{
			Backend:  "antigravity",
			Inner:    err,
			Duration: elapsed,
			Stderr:   stderr,
		}
	}

	return &Response{
		Output:   captured.String(),
		Duration: time.Since(start),
	}, nil
}
