package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// QualityGateHandler runs project-level quality checks (lint, test) inside an
// isolated VM via `stack run --json`. The stack tool copies the workspace into a
// temporary Multipass VM, executes ./run.sh lint and ./run.sh test at /code,
// then tears down the VM. Fires after developer, emits VerdictRendered so the
// engine can feed failures back to the developer via the feedback loop.
type QualityGateHandler struct {
	store    eventstore.Store
	name     string
	stackBin string // path to stack binary, defaults to "stack"
	timeout  int    // stack run --timeout in seconds, defaults to 300
}

// NewQualityGate creates a QualityGateHandler with the canonical name "quality-gate".
func NewQualityGate(d Deps) *QualityGateHandler {
	return &QualityGateHandler{
		store:    d.Store,
		name:     "quality-gate",
		stackBin: "stack",
		timeout:  300,
	}
}

func (h *QualityGateHandler) Name() string             { return h.name }
func (h *QualityGateHandler) Phase() string             { return "quality-gate" }
func (h *QualityGateHandler) Subscribes() []event.Type { return nil }

// Handle runs ./run.sh lint and ./run.sh test inside an isolated VM via
// `stack run <workspace-path> ./run.sh <check> --json`.
// Returns VerdictRendered{pass} if both succeed, VerdictRendered{fail} with
// captured output if either fails.
func (h *QualityGateHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	wsPath, err := h.resolveWorkspacePath(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("quality-gate: resolve workspace: %w", err)
	}
	if wsPath == "" {
		return nil, fmt.Errorf("quality-gate: no workspace found in correlation chain — workflow requires a provisioned workspace")
	}

	runScript := filepath.Join(wsPath, "run.sh")
	if _, err := os.Stat(runScript); os.IsNotExist(err) {
		return h.passVerdict("no run.sh found, skipping quality checks"), nil
	}

	// Run lint first, then test. Collect all failures before reporting.
	var issues []event.Issue
	var failSummaries []string

	for _, check := range []string{"lint", "test"} {
		result, runErr := h.runCheck(ctx, wsPath, check)
		if runErr != nil {
			// Stack-level errors (no compose file, repo not found, stack binary
			// missing) — the repo doesn't support stack-based quality checks.
			if result.isStackError() {
				return h.passVerdict(fmt.Sprintf("stack unavailable (%s), skipping quality checks", result.Code)), nil
			}
			issues = append(issues, event.Issue{
				Severity:    "major",
				Category:    "correctness",
				Description: fmt.Sprintf("./run.sh %s failed:\n%s", check, truncateOutput(result.Output, 2000)),
			})
			failSummaries = append(failSummaries, fmt.Sprintf("%s failed", check))
		}
	}

	if len(issues) > 0 {
		return h.failVerdict(strings.Join(failSummaries, "; "), issues), nil
	}
	return h.passVerdict("lint and test passed"), nil
}

// stackRunResult holds the parsed JSON output from `stack run --json`.
type stackRunResult struct {
	Status   string `json:"status"`    // "success" or "error"
	Action   string `json:"action"`    // "run"
	ExitCode int    `json:"exit_code"` // inner command exit code (success envelope only)
	Output   string `json:"output"`    // captured stdout from the command
	Kept     bool   `json:"kept"`      // whether temp stack was kept on failure
	Stack    string `json:"stack"`     // temp stack name
	Code     string `json:"code"`      // error code (error envelope only)
	Message  string `json:"message"`   // error message (error envelope only)
}

// isStackError returns true for infrastructure-level failures (no compose file,
// repo not found, multipass errors) as opposed to inner command failures.
func (r *stackRunResult) isStackError() bool {
	if r.Status != "error" {
		return false
	}
	switch r.Code {
	case "no_compose_file", "repo_not_found", "multipass_not_installed", "multipass_error":
		return true
	}
	return false
}

// runCheck executes `stack run <wsPath> ./run.sh <check> --json --timeout <n>`
// to run the quality check inside an isolated Multipass VM.
func (h *QualityGateHandler) runCheck(ctx context.Context, wsPath, check string) (stackRunResult, error) {
	cmd := exec.CommandContext(ctx, h.stackBin, "run", wsPath,
		"./run.sh", check,
		"--json",
		"--timeout", fmt.Sprintf("%d", h.timeout),
	)

	// Separate stdout (JSON envelope) from stderr (VM lifecycle noise) so
	// that Docker image pulls and VM creation messages don't corrupt the
	// JSON parse or consume the truncation budget.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	var result stackRunResult
	if jsonErr := json.Unmarshal(stdout.Bytes(), &result); jsonErr != nil {
		// JSON parsing failed — fall back to raw stdout+stderr.
		result.Status = "error"
		result.Output = stdout.String() + stderr.String()
		result.Code = "parse_error"
	}

	if runErr != nil {
		return result, runErr
	}

	// stack run succeeded at infrastructure level — check inner command exit code.
	if result.ExitCode != 0 {
		return result, fmt.Errorf("command exited with code %d", result.ExitCode)
	}

	return result, nil
}

func (h *QualityGateHandler) passVerdict(summary string) []event.Envelope {
	return []event.Envelope{
		event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
			Phase:       "develop",
			SourcePhase: "quality-gate",
			Outcome:     event.VerdictPass,
			Summary:     summary,
		})).WithSource("handler:" + h.name),
	}
}

func (h *QualityGateHandler) failVerdict(summary string, issues []event.Issue) []event.Envelope {
	return []event.Envelope{
		event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
			Phase:       "develop",
			SourcePhase: "quality-gate",
			Outcome:     event.VerdictFail,
			Issues:      issues,
			Summary:     summary,
		})).WithSource("handler:" + h.name),
	}
}

// resolveWorkspacePath delegates to the shared helper in committer.go.
func (h *QualityGateHandler) resolveWorkspacePath(ctx context.Context, correlationID string) (string, error) {
	ws, err := resolveWorkspace(ctx, h.store, correlationID)
	return ws.Path, err
}

// ansiRe matches ANSI escape sequences and backspace-overwrite pairs (spinner chars).
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]|.\x08`)

// truncateOutput strips ANSI escape codes, then caps command output using a
// head+tail strategy to preserve both context and actionable errors.
// Lint errors appear at the tail; Go test failures can appear mid-output with
// a FAIL summary at the tail — keeping both ends covers both cases.
func truncateOutput(s string, maxLen int) string {
	s = ansiRe.ReplaceAllString(s, "")
	if len(s) <= maxLen {
		return s
	}
	headBudget := maxLen / 4     // 25% for context (what command ran)
	tailBudget := maxLen * 3 / 4 // 75% for actual errors
	return s[:headBudget] + "\n\n... (truncated) ...\n\n" + s[len(s)-tailBudget:]
}
