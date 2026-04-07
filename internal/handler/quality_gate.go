package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// qualityCheck pairs a logical check name (used in summaries and debug filenames)
// with the actual command args passed to `stack run` after the workspace path.
// Tests need a "setup → run" wrapper (e.g. `./run.sh up` before `./run.sh test`)
// because each `stack run` is a one-shot VM — services started in a separate
// invocation would be torn down with their VM. Compound shell commands ensure
// setup and execution share the same VM.
type qualityCheck struct {
	name    string
	command []string
}

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
	debugDir string // directory for full debug output; empty = no debug files
	logger   *slog.Logger
}

// NewQualityGate creates a QualityGateHandler with the canonical name "quality-gate".
// Set RICK_QUALITY_GATE_DEBUG_DIR to persist full untruncated output for inspection.
func NewQualityGate(d Deps) *QualityGateHandler {
	h := &QualityGateHandler{
		store:    d.Store,
		name:     "quality-gate",
		stackBin: "stack",
		timeout:  300,
		debugDir: os.Getenv("RICK_QUALITY_GATE_DEBUG_DIR"),
		logger:   slog.Default(),
	}
	return h
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
	// Track kept stacks so we can destroy them at the end — VMs must not
	// survive across iterations; a failed gate means a fresh VM on retry.
	//
	// `test` is wrapped in `bash -c "./run.sh up && ./run.sh test"` because
	// many repos (e.g. hulihealth-web) require services to be running before
	// tests can exec into them. Each `stack run` is a one-shot VM, so up and
	// test must share the same invocation.
	var issues []event.Issue
	var failSummaries []string
	var keptStacks []string

	checks := []qualityCheck{
		{name: "lint", command: []string{"./run.sh", "lint"}},
		{name: "test", command: []string{"bash", "-c", "./run.sh up && ./run.sh test"}},
	}

	for _, check := range checks {
		result, runErr := h.runCheck(ctx, wsPath, check)
		if result.Kept && result.Stack != "" {
			keptStacks = append(keptStacks, result.Stack)
		}
		if runErr != nil {
			// Stack-level errors (no compose file, repo not found, stack binary
			// missing) — the repo doesn't support stack-based quality checks.
			if result.isStackError() {
				return h.passVerdict(fmt.Sprintf("stack unavailable (%s), skipping quality checks", result.Code)), nil
			}

			// Save full raw output to debug file for operator inspection.
			debugRef := h.saveDebugOutput(env.CorrelationID, check.name, result.Output)

			// Filter Docker noise before truncation so actual errors survive.
			cleaned := filterDockerNoise(result.Output)
			desc := fmt.Sprintf("./run.sh %s failed:\n%s", check.name, truncateOutput(cleaned, 2000))
			if debugRef != "" {
				desc += "\n\n[full output: " + debugRef + "]"
			}

			issues = append(issues, event.Issue{
				Severity:    "major",
				Category:    "correctness",
				Description: desc,
			})
			failSummaries = append(failSummaries, fmt.Sprintf("%s failed", check.name))
		}
	}

	// Always destroy kept VMs so the next iteration starts from a clean slate.
	h.destroyKeptStacks(ctx, keptStacks)

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

// runCheck executes `stack run <wsPath> <check.command...> --json --timeout <n>`
// to run the quality check inside an isolated Multipass VM. The command is
// supplied by the caller so that compound shell invocations (e.g.
// `bash -c "./run.sh up && ./run.sh test"`) can share a single one-shot VM.
func (h *QualityGateHandler) runCheck(ctx context.Context, wsPath string, check qualityCheck) (stackRunResult, error) {
	args := []string{"run", wsPath}
	args = append(args, check.command...)
	args = append(args, "--json", "--timeout", fmt.Sprintf("%d", h.timeout))
	cmd := exec.CommandContext(ctx, h.stackBin, args...)

	// Separate stdout (JSON envelopes) from stderr (VM lifecycle noise) so
	// that Docker image pulls and VM creation messages don't corrupt the
	// JSON parse or consume the truncation budget.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	// stack run --json emits NDJSON (one JSON object per line): create, run,
	// destroy. We need the "run" envelope specifically — it contains the
	// inner command's exit code and captured output.
	result, parseOK := parseStackNDJSON(stdout.Bytes())
	if !parseOK {
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

// parseStackNDJSON scans NDJSON lines from stack run --json and returns the
// "run" action envelope. Falls back to the last parseable envelope if no "run"
// action is found. Returns false if no JSON could be parsed at all.
func parseStackNDJSON(data []byte) (stackRunResult, bool) {
	// Fast path: try single-JSON parse (works for tests and simple output).
	var single stackRunResult
	if err := json.Unmarshal(data, &single); err == nil {
		return single, true
	}

	// NDJSON: scan line by line, strip ANSI, find the "run" action envelope.
	var last stackRunResult
	found := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := ansiRe.ReplaceAllString(strings.TrimSpace(scanner.Text()), "")
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var r stackRunResult
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		found = true
		last = r
		if r.Action == "run" {
			return r, true
		}
	}
	return last, found
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

// destroyKeptStacks runs `stack destroy` for each VM that was kept on failure.
// Best-effort — errors are logged but don't affect the verdict.
func (h *QualityGateHandler) destroyKeptStacks(ctx context.Context, stacks []string) {
	for _, name := range stacks {
		h.logger.Info("quality-gate: destroying kept VM", "stack", name)
		cmd := exec.CommandContext(ctx, h.stackBin, "destroy", name, "--force")
		if out, err := cmd.CombinedOutput(); err != nil {
			h.logger.Warn("quality-gate: failed to destroy VM",
				"stack", name, "err", err, "output", string(out))
		}
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

// dockerNoiseRe matches lines that are pure Docker Compose / image pull
// lifecycle noise — container start/stop, network creation, layer
// download progress. These carry no diagnostic value and drown out the
// actual lint/test errors.
var dockerNoiseRe = regexp.MustCompile(
	`(?i)` +
		`(^Container \S+ (Creating|Created|Starting|Started|Stopping|Stopped|Removing|Removed|Waiting|Healthy|Running)$)` +
		`|(^Network \S+ (Creating|Created|Removing|Removed)$)` +
		`|(: Pulling fs layer$)` +
		`|(: (Verifying Checksum|Download complete|Pull complete|Extracting|Waiting)$)` +
		`|(: Pulling from )` +
		`|(^Digest: sha256:)` +
		`|(^Status: Downloaded newer image for )` +
		`|(^Unable to find image .+ locally$)` +
		`|(^[0-9a-f]{12}: )`,
)

// filterDockerNoise removes Docker Compose lifecycle and image-pull lines
// from stack output so that truncation preserves actual error content.
func filterDockerNoise(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if dockerNoiseRe.MatchString(trimmed) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// saveDebugOutput persists the full untruncated output to a file for operator
// inspection. Returns the file path or empty string if debug is disabled.
func (h *QualityGateHandler) saveDebugOutput(correlationID, check, output string) string {
	if h.debugDir == "" {
		return ""
	}
	if err := os.MkdirAll(h.debugDir, 0o755); err != nil {
		h.logger.Warn("quality-gate: failed to create debug dir", "dir", h.debugDir, "err", err)
		return ""
	}
	// Use short correlation prefix for readability.
	shortID := correlationID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	name := fmt.Sprintf("qg-%s-%s.log", shortID, check)
	path := filepath.Join(h.debugDir, name)
	if err := os.WriteFile(path, []byte(output), 0o644); err != nil {
		h.logger.Warn("quality-gate: failed to write debug output", "path", path, "err", err)
		return ""
	}
	h.logger.Info("quality-gate: debug output saved", "path", path, "check", check, "bytes", len(output))
	return path
}
