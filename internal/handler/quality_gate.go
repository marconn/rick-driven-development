package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/github"
)

// qualityCheck pairs a logical check name (used in summaries and debug filenames)
// with the actual command args passed to `stack run` after the workspace path.
// Tests need a "setup → run" wrapper (e.g. `./run.sh up` before `./run.sh test`)
// because each `stack run` is a one-shot VM — services started in a separate
// invocation would be torn down with their VM. Compound shell commands ensure
// setup and execution share the same VM.
//
// label is the human-readable command form used in failure descriptions
// (e.g. "./run.sh test", "make check"). It is decoupled from `command`
// because the latter may include shell wrappers (bash -c "...") that look
// confusing in operator-facing output.
type qualityCheck struct {
	name    string
	label   string
	command []string
}

// runnerKind identifies which build runner the gate detected for a workspace.
// runnerNone means no driveable runner was found and the gate must escalate
// as advisory rather than execute anything.
type runnerKind string

const (
	runnerNone      runnerKind = ""
	runnerRunSh     runnerKind = "run.sh"
	runnerMakeCheck runnerKind = "make check"
	runnerManifest  runnerKind = ".rick/quality.yaml"
)

// qualityPlan bundles the runner identity, execution runtime, and the ordered
// list of checks. Produced by detectQualityPlan; consumed by Handle. Keeping
// runner and runtime distinct lets us tell "what we found" (manifest vs run.sh
// vs Makefile) apart from "how it executes" (stack VM vs host process tree).
type qualityPlan struct {
	runner  runnerKind
	runtime runtimeKind
	checks  []qualityCheck
}

// makefileCheckTargetRe matches a top-level `check:` rule in a Makefile.
// Tolerates optional whitespace before the colon and prerequisites/recipe
// after it. Static grep — false negatives are possible if `check:` lives in
// an `include`d file; those workspaces fall through to the advisory-pause
// path, preserving current behavior. We do not invoke `make` to probe
// because that would require running it inside the stack VM (which is the
// thing we are trying to decide whether to do at all).
var makefileCheckTargetRe = regexp.MustCompile(`(?m)^check[ \t]*:`)

// detectRunner picks the build runner for this workspace.
// Probe order: run.sh → Makefile with a `check:` target → none.
// Returns (runnerNone, nil) when neither is available; the caller emits
// an advisory verdict in that case.
func detectRunner(wsPath string) (runnerKind, []qualityCheck) {
	if _, err := os.Stat(filepath.Join(wsPath, "run.sh")); err == nil {
		// `test` is wrapped in `bash -c "./run.sh up && ./run.sh test"`
		// because many repos (e.g. hulihealth-web) require services to be
		// running before tests can exec into them. Each `stack run` is a
		// one-shot VM, so up and test must share one invocation.
		return runnerRunSh, []qualityCheck{
			{name: "lint", label: "./run.sh lint", command: []string{"./run.sh", "lint"}},
			{name: "test", label: "./run.sh test", command: []string{"bash", "-c", "./run.sh up && ./run.sh test"}},
		}
	}
	if makefileHasCheckTarget(wsPath) {
		// One unified target. Org convention (per pre-commit gate) is that
		// `make check` runs lint + test + race in a single invocation.
		// Splitting into separate make calls would change the contract and
		// double the number of stack VMs spawned per workflow.
		return runnerMakeCheck, []qualityCheck{
			{name: "check", label: "make check", command: []string{"make", "check"}},
		}
	}
	return runnerNone, nil
}

// makefileHasCheckTarget reports whether the workspace's Makefile declares
// a top-level `check:` rule. Returns false when the file is absent or
// unreadable — both are equivalent to "we can't drive this".
func makefileHasCheckTarget(wsPath string) bool {
	data, err := os.ReadFile(filepath.Join(wsPath, "Makefile"))
	if err != nil {
		return false
	}
	return makefileCheckTargetRe.Match(data)
}

// detectQualityPlan resolves the workspace's quality-check contract. Probe
// order is manifest-first, legacy-fallback:
//
//  1. operator-local manifest at <manifestsDir>/<owner>/<name>.yaml (or
//     <manifestsDir>/<name>.yaml fallback) → use it. The manifest is the
//     source of truth; it is NOT committed to the consumer repo.
//  2. else run.sh present → legacy heuristic (./run.sh lint + ./run.sh test
//     wrapped in `bash -c "./run.sh up && ./run.sh test"`), runtime=stack.
//  3. else Makefile with `check:` target → `make check`, runtime=stack.
//  4. else → no plan; caller emits advisory.
//
// Returns the plan plus a load error. The error is non-nil only when a
// manifest file was found but failed to parse/validate — that case must
// NOT silently fall back to probing because a misconfigured manifest is a
// rick-operator bug, not a missing-file scenario.
//
// manifestsDir empty disables manifest lookup entirely (effectively reverts
// to legacy probing). Tests use this to isolate the legacy-fallback path.
func detectQualityPlan(wsPath, manifestsDir string) (qualityPlan, error) {
	mf, err := loadLocalQualityManifest(wsPath, manifestsDir)
	if err != nil {
		return qualityPlan{}, err
	}
	if mf != nil {
		return qualityPlan{
			runner:  runnerManifest,
			runtime: mf.Runtime,
			checks:  checksFromManifest(mf),
		}, nil
	}
	runner, checks := detectRunner(wsPath)
	if runner == runnerNone {
		return qualityPlan{}, nil
	}
	// Legacy probing has always implied stack runtime — Multipass + docker-compose
	// is the assumption baked into ./run.sh up && ./run.sh test and into make check.
	// Repos that need host runtime must use a manifest to opt in.
	return qualityPlan{runner: runner, runtime: runtimeStack, checks: checks}, nil
}

// QualityGateHandler runs project-level quality checks (lint, test) inside an
// isolated VM via `stack run --json`. The stack tool copies the workspace into a
// temporary Multipass VM, executes ./run.sh lint and ./run.sh test at /code,
// then tears down the VM. Fires after developer, emits VerdictRendered so the
// engine can feed failures back to the developer via the feedback loop.
type QualityGateHandler struct {
	store        eventstore.Store
	name         string
	stackBin     string         // path to stack binary, defaults to "stack"
	timeout      int            // stack run --timeout in seconds, defaults to 180 (3 minutes)
	debugDir     string         // directory for full debug output; resolved by resolveQualityGateDebugDir
	manifestsDir string         // operator-local config dir for per-repo manifests; resolved by resolveQualityManifestsDir
	gh           *github.Client // optional — used to cross-check local fails against GitHub CI
	logger       *slog.Logger
}

// docsOnlyExts lists file suffixes that are purely documentation and carry
// no build/test impact. A PR whose modified-files set is a subset of these
// cannot have caused a runtime regression, so the gate short-circuits to pass.
// Kept deliberately narrow — we only skip when *every* modified path matches.
var docsOnlyExts = []string{".md", ".markdown", ".rst", ".txt"}

// docsOnlyPaths lists path prefixes/filenames that are structurally docs or
// metadata. Must-end-with-slash prefixes are matched via HasPrefix.
var docsOnlyPaths = []string{
	"docs/", ".github/ISSUE_TEMPLATE/", ".github/PULL_REQUEST_TEMPLATE/",
}

var docsOnlyFilenames = map[string]bool{
	"CODEOWNERS":    true,
	"LICENSE":       true,
	"AUTHORS":       true,
	"CONTRIBUTORS":  true,
}

// NewQualityGate creates a QualityGateHandler with the canonical name "quality-gate".
// Set RICK_QUALITY_GATE_DEBUG_DIR to override the default debug directory
// ($XDG_STATE_HOME/rick/quality-gate, falling back to $HOME/.local/state/rick/quality-gate).
// Debug files are always written on failure — the default location guarantees
// operators can recover the raw tool output even when the filtered verdict body
// collapses to an empty string.
func NewQualityGate(d Deps) *QualityGateHandler {
	h := &QualityGateHandler{
		store:        d.Store,
		name:         "quality-gate",
		stackBin:     "stack",
		timeout:      180,
		debugDir:     resolveQualityGateDebugDir(),
		manifestsDir: resolveQualityManifestsDir(),
		gh:           d.GitHub,
		logger:       slog.Default(),
	}
	return h
}

// resolveQualityGateDebugDir picks the default debug directory for
// quality-gate failure artifacts. Precedence: RICK_QUALITY_GATE_DEBUG_DIR →
// $XDG_STATE_HOME/rick/quality-gate → $HOME/.local/state/rick/quality-gate.
// Returns empty only if HOME is unset (practically impossible on a running host).
func resolveQualityGateDebugDir() string {
	if d := os.Getenv("RICK_QUALITY_GATE_DEBUG_DIR"); d != "" {
		return d
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "rick", "quality-gate")
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".local", "state", "rick", "quality-gate")
	}
	return ""
}

func (h *QualityGateHandler) Name() string             { return h.name }
func (h *QualityGateHandler) Subscribes() []event.Type { return nil }

// Handle runs ./run.sh lint and ./run.sh test inside an isolated VM via
// `stack run <workspace-path> ./run.sh <check> --json`.
// Returns VerdictRendered{pass} if both succeed, VerdictRendered{fail} with
// captured output if either fails.
//
// Two fast-passes run before invoking the VM: if the PR modifies only docs
// files we short-circuit to pass (nothing a test suite can regress on); if
// the VM run fails but GitHub CI is green on the same SHA we flip the verdict
// into an advisory failure that the engine escalates to the operator instead
// of looping the developer on what is almost certainly a local-env flake.
func (h *QualityGateHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	// Defensive default: tests that construct QualityGateHandler via struct
	// literal sometimes omit the logger field. Pre-existing log sites tolerated
	// this because no log call fired on the run.sh success path; the new
	// "runner detected" Info changed that. Fixing it here covers every log
	// site at once instead of patching individual test fixtures.
	if h.logger == nil {
		h.logger = slog.Default()
	}

	wsPath, err := h.resolveWorkspacePath(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("quality-gate: resolve workspace: %w", err)
	}
	if wsPath == "" {
		return nil, fmt.Errorf("quality-gate: no workspace found in correlation chain — workflow requires a provisioned workspace")
	}

	plan, err := detectQualityPlan(wsPath, h.manifestsDir)
	if err != nil {
		// A manifest file was found but malformed/invalid. Loud failure — do
		// NOT fall back to legacy probing. A broken manifest is a rick-operator
		// bug they must fix; silently probing would mask the typo and run
		// whatever heuristic happens to match, which is almost never what the
		// operator intended.
		return h.unverifiableVerdict(env.CorrelationID, wsPath, "manifest_invalid", "Rick's quality manifest could not be loaded: "+err.Error()), nil
	}
	if plan.runner == runnerNone {
		// We cannot drive this repo's build system. Escalate as advisory
		// rather than silently passing — the operator must verify the
		// developer's diff manually before approving the commit. The reason
		// text discriminates the three "no driver" sub-cases (no Makefile,
		// Makefile without `check:`, neither) so the operator's next step
		// is concrete, not a guess.
		return h.unverifiableVerdict(env.CorrelationID, wsPath, "no_run_sh", reasonNoRunner(wsPath)), nil
	}
	if plan.runtime == runtimeHost && !hostRuntimeAllowed() {
		// Repo declared host runtime but the operator's environment hasn't
		// opted in. Escalate as advisory: rick will not run host commands
		// from a manifest without explicit consent (RICK_ALLOW_HOST_RUNTIME=1)
		// because the manifest can be a vector for arbitrary code execution
		// on the host. Default-deny is the safety boundary.
		return h.unverifiableVerdict(env.CorrelationID, wsPath, "host_runtime_not_allowed",
			"Manifest declares runtime=host but RICK_ALLOW_HOST_RUNTIME=1 is not set. Host runtime runs commands directly on the operator's host (no stack VM isolation); set the env var explicitly to opt in."), nil
	}
	checks := plan.checks

	// Docs-only fast-pass: if every modified file is a .md/docs/* path,
	// runtime checks are structurally irrelevant. We still require non-empty
	// evidence that context-snapshot ran — an unset ModifiedFiles list means
	// "we don't know", not "no changes", and falls through to the full gate.
	if files, ok := h.modifiedFilesFromCorrelation(ctx, env.CorrelationID); ok && isDocsOnlyDiff(files) {
		h.logger.Info("quality-gate: docs-only diff, skipping runtime checks",
			"correlation", env.CorrelationID, "files", len(files))
		return h.passVerdict(fmt.Sprintf("docs-only diff (%d file(s)), skipping runtime checks", len(files))), nil
	}

	h.logger.Info("quality-gate: runner detected",
		slog.String("runner", string(plan.runner)),
		slog.String("runtime", string(plan.runtime)),
		slog.String("correlation", env.CorrelationID),
	)

	// Iterate the runner's checks. Collect all failures before reporting so
	// the developer sees the full picture in one feedback round (lint and
	// test both fail → one verdict naming both, not two iterations).
	// Track kept stacks so we can destroy them at the end — VMs must not
	// survive across iterations; a failed gate means a fresh VM on retry.
	var issues []event.Issue
	var failSummaries []string
	var keptStacks []string
	// rawDiagnosticsParts accumulates the unfiltered tail of each failing
	// check's stdout+stderr. Forwarded to VerdictPayload.RawDiagnostics so the
	// developer's iteration prompt can act on the unredacted failure stream
	// even when buildFailureDescription's filter trimmed Issue.Description.
	var rawDiagnosticsParts []string

	for _, check := range checks {
		result, runErr := h.runCheck(ctx, wsPath, check, plan.runtime)
		if result.Kept && result.Stack != "" {
			keptStacks = append(keptStacks, result.Stack)
		}
		if runErr != nil {
			// Stack-level errors (no compose file, repo not found, stack binary
			// missing) — the repo doesn't support stack-based quality checks.
			// Same gap as the missing-run.sh path: we cannot verify the diff,
			// so escalate as advisory rather than silently passing.
			if result.isStackError() {
				h.destroyKeptStacks(ctx, keptStacks)
				return h.unverifiableVerdict(env.CorrelationID, wsPath, "stack_"+result.Code, reasonStackUnavailable(result.Code, result.Message)), nil
			}

			// Always save full raw output to debug dir. Before this change the
			// debug artifact was env-gated, which meant the operator inspecting
			// a `./run.sh test failed:\n` empty-body verdict had no trail back
			// to the real stderr. The default dir is cheap — a few KB per run.
			fullOutput := mergeOutputAndStderr(result.Output, result.Stderr)
			debugRef := h.saveDebugOutput(env.CorrelationID, check.name, fullOutput)
			rawDiagnosticsParts = append(rawDiagnosticsParts, formatRawDiagnostics(check.name, fullOutput))

			// parse_error: stack exited but emitted no parseable JSON envelope.
			// 2026-04-29 incident: stack failed before reaching its run command;
			// cobra echoed `Error: command exited with code 1` to both streams;
			// runCheck's fallback used that as result.Output. A normal fail
			// verdict here would feed the developer cobra-echo garbage and burn
			// 3 iterations on a non-regression. Escalate as advisory instead so
			// the operator decides what to do next.
			if result.Code == "parse_error" {
				h.destroyKeptStacks(ctx, keptStacks)
				descLines := []string{
					fmt.Sprintf("%s failed: stack output unparseable (parse_error) — likely stack misconfiguration, multipass not ready, or a non-zero exit before stack emitted its JSON envelope.", check.label),
					"",
					"Captured raw bytes:",
					truncateOutput(fullOutput, 2000),
				}
				if debugRef != "" {
					descLines = append(descLines, "", "[full output: "+debugRef+"]")
				}
				return h.advisoryFailVerdict(
					fmt.Sprintf("%s parse_error — stack output unparseable, escalating to operator", check.name),
					[]event.Issue{{
						Severity:    "major",
						Category:    "infrastructure",
						Description: strings.Join(descLines, "\n"),
					}},
					strings.Join(rawDiagnosticsParts, "\n\n"),
				), nil
			}

			desc := buildFailureDescription(check.label, fullOutput, debugRef)
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

	if len(issues) == 0 {
		return h.passVerdict(passSummary(plan.runner)), nil
	}

	rawDiagnostics := strings.Join(rawDiagnosticsParts, "\n\n")

	// Cross-check GitHub CI on the same SHA before declaring a regression.
	// If upstream CI is green here, the likelihood of a real regression drops
	// sharply — most local fails at this point are environment flakes
	// (docker-compose timing, Solr reindex races, multipass cold starts).
	// Flip to advisory so the engine pauses for operator review instead of
	// re-spinning developer for 3M+ tokens on a non-regression.
	summary := strings.Join(failSummaries, "; ")
	if h.githubCIAllGreen(ctx, env.CorrelationID) {
		h.logger.Warn("quality-gate: local failed but GitHub CI is green on same SHA — emitting advisory",
			"correlation", env.CorrelationID)
		return h.advisoryFailVerdict(summary+" (GitHub CI green on same SHA — likely local-env flake)", issues, rawDiagnostics), nil
	}
	return h.failVerdict(summary, issues, rawDiagnostics), nil
}

// buildFailureDescription assembles the verdict description for a failed
// check. The commandLabel is the human-readable form (e.g. "./run.sh test"
// or "make check") rather than just the logical name, so the developer
// reads the actual command they would re-run to reproduce.
//
// Two failure modes must survive: (a) normal case — keep the tail of the
// cleaned output; (b) degenerate case — filter stripped everything, fall
// back to the raw unfiltered tail with a marker so the body is never empty.
// The reporter on 2026-04-22 hit (b): two identical `./run.sh test failed:\n`
// verdicts with zero body forced the developer to re-run the suite manually
// just to discover what failed.
func buildFailureDescription(commandLabel, rawOutput, debugRef string) string {
	const maxLen = 2000
	cleaned := strings.TrimSpace(filterDockerNoise(rawOutput))
	var body string
	switch {
	case cleaned != "":
		body = truncateOutput(cleaned, maxLen)
	case strings.TrimSpace(rawOutput) != "":
		// Filter collapsed to empty but raw output had something — use it.
		// This is the path that fixes the empty-body regression: the verdict
		// now always carries at least the tail of whatever the tool emitted.
		body = "[docker-noise filter stripped all lines; raw tail follows]\n" +
			truncateOutput(strings.TrimSpace(rawOutput), maxLen)
	default:
		// Genuinely no output at all. Name the failure explicitly so the
		// developer sees *something* more actionable than a bare newline.
		body = "[no output captured — command exited non-zero with empty stdout/stderr]"
	}
	desc := fmt.Sprintf("%s failed:\n%s", commandLabel, body)
	if debugRef != "" {
		desc += "\n\n[full output: " + debugRef + "]"
	}
	return desc
}

// modifiedFilesFromCorrelation loads the workflow's correlation chain and
// returns the most recent non-empty ModifiedFiles list from a ContextGit
// event. Returns (nil, false) when no context-git snapshot exists or its
// ModifiedFiles slice is empty — callers must treat that as "unknown", not
// "no changes", since an upstream handler may simply not have emitted yet.
func (h *QualityGateHandler) modifiedFilesFromCorrelation(ctx context.Context, correlationID string) ([]string, bool) {
	evts, err := h.store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return nil, false
	}
	var latest []string
	for _, e := range evts {
		if e.Type != event.ContextGit {
			continue
		}
		var p event.ContextGitPayload
		if jsonErr := json.Unmarshal(e.Payload, &p); jsonErr != nil {
			continue
		}
		if len(p.ModifiedFiles) > 0 {
			latest = p.ModifiedFiles
		}
	}
	if len(latest) == 0 {
		return nil, false
	}
	return latest, true
}

// isDocsOnlyDiff returns true iff every file in the list is structurally
// documentation (extension whitelist + known metadata filenames + docs/
// prefix). A single non-doc file disqualifies the PR — the whitelist is
// deliberately narrow because a false positive silently skips the gate.
// An empty or blank-only list returns false: "we don't know what changed"
// must never be treated as "nothing code-like changed".
func isDocsOnlyDiff(files []string) bool {
	seen := 0
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !isDocFile(f) {
			return false
		}
		seen++
	}
	return seen > 0
}

func isDocFile(path string) bool {
	base := filepath.Base(path)
	if docsOnlyFilenames[base] {
		return true
	}
	for _, prefix := range docsOnlyPaths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	ext := strings.ToLower(filepath.Ext(path))
	for _, allowed := range docsOnlyExts {
		if ext == allowed {
			return true
		}
	}
	return false
}

// githubCIAllGreen consults GitHub's check-runs API for the current PR's HEAD
// SHA and returns true only if at least one check completed and every
// completed check concluded success/skipped/neutral. Any failure, any
// queued/in-progress check, any API error — all treated as "not green", so
// we only short-circuit the retry loop when we have positive upstream signal.
func (h *QualityGateHandler) githubCIAllGreen(ctx context.Context, correlationID string) bool {
	if h.gh == nil {
		return false
	}
	src := h.sourceFromCorrelation(ctx, correlationID)
	if src == "" {
		return false
	}
	fullRepo, prNumberStr, err := parsePRSource(src)
	if err != nil {
		return false
	}
	parts := strings.SplitN(fullRepo, "/", 2)
	if len(parts) != 2 {
		return false
	}
	owner, repoName := parts[0], parts[1]
	prNumber, err := strconv.Atoi(prNumberStr)
	if err != nil {
		return false
	}
	head, err := h.gh.GetPRHead(ctx, owner, repoName, prNumber)
	if err != nil || head == nil || head.SHA == "" {
		h.logger.Debug("quality-gate: cross-check skipped — no PR head", "err", err)
		return false
	}
	resp, err := h.gh.GetCheckRuns(ctx, owner, repoName, head.SHA)
	if err != nil || resp == nil || resp.TotalCount == 0 {
		h.logger.Debug("quality-gate: cross-check skipped — no check runs", "sha", head.SHA, "err", err)
		return false
	}
	completed := 0
	for _, cr := range resp.CheckRuns {
		if cr.Status != "completed" {
			return false // any in-flight check → no upstream verdict yet
		}
		completed++
		switch cr.Conclusion {
		case "success", "skipped", "neutral":
			// ok
		default:
			return false
		}
	}
	return completed > 0
}

// sourceFromCorrelation returns the Source field of the originating
// WorkflowRequested event for this correlation. Empty string if we can't
// find one.
func (h *QualityGateHandler) sourceFromCorrelation(ctx context.Context, correlationID string) string {
	evts, err := h.store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return ""
	}
	for _, e := range evts {
		if e.Type != event.WorkflowRequested {
			continue
		}
		var p event.WorkflowRequestedPayload
		if jsonErr := json.Unmarshal(e.Payload, &p); jsonErr != nil {
			continue
		}
		return p.Source
	}
	return ""
}

// stackRunResult holds the parsed JSON output from `stack run --json`.
type stackRunResult struct {
	Status   string `json:"status"`    // "success" or "error"
	Action   string `json:"action"`    // "run"
	ExitCode int    `json:"exit_code"` // inner command exit code (success envelope only)
	Output   string `json:"output"`    // captured stdout from the command
	Stderr   string `json:"stderr"`    // captured stderr + stack diagnostic lines (stack ≥ contract v2)
	Kept     bool   `json:"kept"`      // whether temp stack was kept on failure
	Stack    string `json:"stack"`     // temp stack name
	Code     string `json:"code"`      // error code (error envelope only)
	Message  string `json:"message"`   // error message (error envelope only)
}

// isStackError returns true for infrastructure-level failures (no compose file,
// repo not found, multipass errors, missing host executable) as opposed to
// inner command failures. The host_* codes are produced by runHostCheck for
// the same operator-actionable class of errors as the multipass codes.
func (r *stackRunResult) isStackError() bool {
	if r.Status != "error" {
		return false
	}
	switch r.Code {
	case "no_compose_file", "repo_not_found", "multipass_not_installed", "multipass_error",
		"host_executable_missing", "host_command_invalid":
		return true
	}
	return false
}

// runCheck dispatches a single quality check to the runtime declared by the
// repo's plan (manifest or legacy probe). All runtimes return the same
// stackRunResult shape so the downstream pipeline (mergeOutputAndStderr,
// buildFailureDescription, debug-log persistence) is runtime-agnostic.
func (h *QualityGateHandler) runCheck(ctx context.Context, wsPath string, check qualityCheck, runtime runtimeKind) (stackRunResult, error) {
	if runtime == runtimeHost {
		return h.runHostCheck(ctx, wsPath, check)
	}
	return h.runStackCheck(ctx, wsPath, check)
}

// runStackCheck executes `stack run --json --timeout <n> <wsPath> -- <check.command...>`
// to run the quality check inside an isolated Multipass VM. The command is
// supplied by the caller so that compound shell invocations (e.g.
// `bash -c "./run.sh up && ./run.sh test"`) can share a single one-shot VM.
//
// The `--` separator is mandatory: without it cobra parses flag-like args in
// the inner command (e.g. `bash -c "…"`) and rejects them with
// `unknown shorthand flag: 'c' in -c`, short-circuiting before the VM is ever
// reached. Stack flags must appear before the separator so cobra consumes
// them rather than passing them through to the command.
func (h *QualityGateHandler) runStackCheck(ctx context.Context, wsPath string, check qualityCheck) (stackRunResult, error) {
	args := []string{"run", "--json", "--timeout", fmt.Sprintf("%d", h.timeout), wsPath, "--"}
	args = append(args, check.command...)
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

	// Fold rick's own captured subprocess stderr into result.Stderr. Stack
	// prints nothing to stderr in JSON mode, but this defends against future
	// leakage and against non-JSON fatal panics from stack itself. Belt +
	// suspenders: the 2026-04-22 and 2026-04-24 empty-body verdicts came from
	// exactly this hole — every layer captured the diagnostic text and every
	// layer dropped it before the verdict was built.
	if s := strings.TrimSpace(stderr.String()); s != "" {
		if result.Stderr != "" {
			result.Stderr = result.Stderr + "\n" + s
		} else {
			result.Stderr = s
		}
	}

	if runErr != nil {
		// Guarantee the caller always sees *something* diagnostic. If the JSON
		// envelope's output was empty but stderr has content, promote stderr
		// into output so buildFailureDescription's truncation and debug-save
		// paths carry actual signal.
		if strings.TrimSpace(result.Output) == "" && strings.TrimSpace(result.Stderr) != "" {
			result.Output = result.Stderr
		}
		return result, runErr
	}

	// stack run succeeded at infrastructure level — check inner command exit code.
	if result.ExitCode != 0 {
		if strings.TrimSpace(result.Output) == "" && strings.TrimSpace(result.Stderr) != "" {
			result.Output = result.Stderr
		}
		return result, fmt.Errorf("command exited with code %d", result.ExitCode)
	}

	return result, nil
}

// runHostCheck executes the check's command directly on the operator's host
// with cwd=workspace. No stack VM is spawned. Used for repos that cannot be
// stack-virtualized (e.g. Go monorepos with no docker-compose) and is gated
// by RICK_ALLOW_HOST_RUNTIME=1, enforced upstream by Handle.
//
// Result shape mirrors runStackCheck so the downstream pipeline is uniform:
// inner-command stdout maps to result.Output, inner-command stderr to
// result.Stderr, exit code to result.ExitCode. A missing executable maps to
// the dedicated host_executable_missing infra-error code so the operator
// gets an actionable advisory instead of a misleading "test failed" verdict.
func (h *QualityGateHandler) runHostCheck(ctx context.Context, wsPath string, check qualityCheck) (stackRunResult, error) {
	if len(check.command) == 0 {
		// Defensive: validateManifest already rejects this, but keep the guard
		// so a hand-constructed qualityCheck (e.g. in tests) can't panic.
		return stackRunResult{
			Status:  "error",
			Code:    "host_command_invalid",
			Message: "empty command argv for check " + check.name,
		}, fmt.Errorf("host check %q has empty command", check.name)
	}

	cmd := exec.CommandContext(ctx, check.command[0], check.command[1:]...)
	cmd.Dir = wsPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	result := stackRunResult{
		Status: "success",
		Action: "run",
		Output: stdout.String(),
		Stderr: stderr.String(),
	}

	if runErr == nil {
		return result, nil
	}

	// Distinguish "binary not found / permission denied / etc." (infrastructure
	// problem the operator must fix) from "binary ran and exited non-zero"
	// (real test/lint failure). Only the former should trigger isStackError().
	var execErr *exec.Error
	if errors.As(runErr, &execErr) {
		result.Status = "error"
		result.Code = "host_executable_missing"
		result.Message = execErr.Error()
		return result, runErr
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	} else {
		// Unusual: not an *exec.Error, not an *exec.ExitError. Could be
		// context cancellation. Surface as exit code -1 + the wrapped err
		// so the failure description still has signal.
		result.ExitCode = -1
	}

	// Mirror the stack-side "promote stderr into output when stdout is empty"
	// safety net so buildFailureDescription's truncation always has signal.
	if strings.TrimSpace(result.Output) == "" && strings.TrimSpace(result.Stderr) != "" {
		result.Output = result.Stderr
	}

	return result, runErr
}

// maxNDJSONLineBytes caps a single NDJSON envelope at 16 MiB. Stack embeds
// the inner command's full stdout/stderr inline in the "run" envelope, and
// docker-compose pulls easily exceed bufio's default 64 KiB buffer. The 2026-05-06
// huli-api regression was a 130 KiB envelope silently dropped by the scanner;
// without an explicit cap we fall back to the last parseable line (typically
// "create"), which has empty Output/Stderr — the verdict ends up with no
// signal even though stack captured everything.
//
// 16 MiB is comfortable headroom for any plausible stack output. Beyond that,
// scanner.Err() returns bufio.ErrTooLong and we surface a parse_error advisory
// rather than a misleading "test failed" verdict on a partial parse.
const maxNDJSONLineBytes = 16 * 1024 * 1024

// parseStackNDJSON scans NDJSON lines from stack run --json and returns the
// "run" action envelope. Falls back to the last parseable envelope if no "run"
// action is found. Returns false if no JSON could be parsed at all OR if the
// scanner hit a hard limit (line longer than maxNDJSONLineBytes) — partial
// parses MUST NOT be treated as success because the missing line is almost
// always the "run" envelope carrying the actual signal.
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
	scanner.Buffer(make([]byte, 0, 64*1024), maxNDJSONLineBytes)
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
	if scanner.Err() != nil {
		// A line exceeded our 16 MiB ceiling, OR an underlying read failed.
		// Either way, returning a partial parse would silently mask the
		// missing envelope (typically "run") and feed an empty-signal verdict
		// to the developer. Hard fail so the caller takes the parse_error
		// path and escalates to operator instead.
		return stackRunResult{}, false
	}
	return last, found
}

func (h *QualityGateHandler) passVerdict(summary string) []event.Envelope {
	return []event.Envelope{
		event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
			Persona:       "developer",
			SourcePersona: "quality-gate",
			Outcome:       event.VerdictPass,
			Summary:       summary,
		})).WithSource("handler:" + h.name),
	}
}

func (h *QualityGateHandler) failVerdict(summary string, issues []event.Issue, rawDiagnostics string) []event.Envelope {
	return []event.Envelope{
		event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
			Persona:        "developer",
			SourcePersona:  "quality-gate",
			Outcome:        event.VerdictFail,
			Issues:         issues,
			Summary:        summary,
			RawDiagnostics: rawDiagnostics,
		})).WithSource("handler:" + h.name),
	}
}

// unverifiableVerdict emits an advisory-fail verdict for cases where the
// quality-gate has no path to actually run lint/test on this repo (no run.sh
// at the workspace root, or stack returned an infrastructure-level error).
// The summary leads with the structural cause (kind) so operators can tell
// "we cannot verify" from "we ran and it failed". The Issue.Description
// includes the workspace path and any detectable build-runner alternative
// (Makefile) so the operator's manual verification step is named, not
// guessed at — directly addressing the pause-fatigue risk.
func (h *QualityGateHandler) unverifiableVerdict(correlationID, wsPath, kind, reason string) []event.Envelope {
	h.logger.Info("quality-gate: unverifiable, escalating to operator",
		slog.String("correlation", correlationID),
		slog.String("kind", kind),
		slog.String("workspace", wsPath),
	)
	desc := fmt.Sprintf(
		"Rick's quality-gate cannot run on this workspace.\n\n"+
			"Workspace: %s\nReason: %s\n\n"+
			"Operator action: verify the developer's diff manually (build / lint / "+
			"test using whatever runner this repo supports) and resume the workflow "+
			"to allow the committer to proceed. The diff has been reviewed by the "+
			"reviewer and qa personas but not actually executed.",
		wsPath, reason,
	)
	return h.advisoryFailVerdict(
		fmt.Sprintf("%s — quality checks cannot run; escalating to operator", kind),
		[]event.Issue{{
			Severity:    "major",
			Category:    "infrastructure",
			Description: desc,
		}},
		"", // no raw diagnostics — nothing was executed
	)
}

// passSummary names the runner in the success summary so operators reading
// verdicts can tell at a glance which gate actually ran. Existing
// run.sh-driven repos keep the historical "lint and test passed" wording so
// log scrapers and dashboards filtering on it don't break.
func passSummary(runner runnerKind) string {
	switch runner {
	case runnerMakeCheck:
		return "make check passed"
	case runnerManifest:
		return "manifest checks passed"
	default:
		return "lint and test passed"
	}
}

// reasonNoRunner builds the operator-facing reason text when no driveable
// runner could be found. Discriminates three sub-cases so the operator's
// next step is concrete:
//   - no run.sh, no Makefile → repo has no Rick-driveable build system
//   - no run.sh, Makefile without `check:` → operator should add a `check`
//     target (preferred) or a run.sh
//   - no run.sh, Makefile WITH `check:` → unreachable from here (detectRunner
//     would have returned runnerMakeCheck), but kept for safety
func reasonNoRunner(wsPath string) string {
	mfPath := filepath.Join(wsPath, "Makefile")
	if _, err := os.Stat(mfPath); err != nil {
		return "no run.sh at the workspace root and no Makefile fallback — Rick has no driver for this repo's build system"
	}
	// Makefile present but no `check:` target — detectRunner already verified
	// the absence; surface that fact so the operator can add the target rather
	// than guess at a manual command.
	return "no run.sh at the workspace root, and the Makefile has no `check` target Rick can call — add a `check` target (the org convention is `check: lint test test-race`) or a run.sh"
}

// reasonStackUnavailable builds the operator-facing reason text for stack
// infrastructure errors. The error code distinguishes user-actionable issues
// (multipass not installed) from environmental ones (no compose file).
func reasonStackUnavailable(code, message string) string {
	switch code {
	case "no_compose_file":
		return "stack could not find a docker-compose.yml at the workspace root — this repo's compose layout is non-standard or absent (e.g., compose lives under deploy/docker/)"
	case "repo_not_found":
		return "stack could not find the workspace path it was given — this is likely a deploy bug; check the workspace clone exists"
	case "multipass_not_installed":
		return "stack requires multipass for VM isolation; multipass is not installed on this host (install it or set RICK_DISABLE_QUALITY_GATE=1 to bypass the gate entirely)"
	case "multipass_error":
		if message != "" {
			return "stack's multipass invocation failed: " + message
		}
		return "stack's multipass invocation failed (see logs)"
	case "host_executable_missing":
		if message != "" {
			return "host runtime: command not found / not executable: " + message
		}
		return "host runtime: command not found / not executable on this host"
	case "host_command_invalid":
		return "host runtime: manifest declared an empty or malformed command argv"
	default:
		return "stack returned an infrastructure error (" + code + ")"
	}
}

// advisoryFailVerdict emits a fail verdict marked Advisory=true. The aggregate
// treats these as "pause for operator" rather than "retrigger developer", so
// the handler can fail without starting a feedback loop on a non-regression.
func (h *QualityGateHandler) advisoryFailVerdict(summary string, issues []event.Issue, rawDiagnostics string) []event.Envelope {
	return []event.Envelope{
		event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
			Persona:        "developer",
			SourcePersona:  "quality-gate",
			Outcome:        event.VerdictFail,
			Issues:         issues,
			Summary:        summary,
			Advisory:       true,
			RawDiagnostics: rawDiagnostics,
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

// mergeOutputAndStderr combines the inner-command stdout (from stack's
// "output" JSON field) with the captured stderr/diagnostic stream (from
// stack's "stderr" field plus rick's own subprocess stderr, populated by
// runCheck). When both are non-empty they are joined with a labeled header;
// when only one is non-empty it is returned as-is. Empty result means the
// whole pipeline was silent — caller should surface the exit-code-only
// degenerate message.
func mergeOutputAndStderr(stdout, stderr string) string {
	stdoutTrim := strings.TrimSpace(stdout)
	stderrTrim := strings.TrimSpace(stderr)
	switch {
	case stdoutTrim == "" && stderrTrim == "":
		return ""
	case stdoutTrim == "":
		return "[stack diagnostics / stderr]\n" + stderr
	case stderrTrim == "":
		return stdout
	default:
		return stdout + "\n\n[stack diagnostics / stderr]\n" + stderr
	}
}

// ansiRe matches ANSI escape sequences and backspace-overwrite pairs (spinner chars).
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]|.\x08`)

// rawDiagnosticsTailLines is the cap on the number of trailing lines of
// fullOutput propagated to VerdictPayload.RawDiagnostics. Sized so a typical
// Go test failure (FAIL summary + last few stack frames) survives intact, while
// long pre-test setup logs (Docker pulls, schema migrations) are truncated.
const rawDiagnosticsTailLines = 64

// formatRawDiagnostics returns the last rawDiagnosticsTailLines lines of the
// merged stdout/stderr blob, prefixed with the check name so multi-check
// failures (lint + test) stay disambiguated in the developer prompt.
func formatRawDiagnostics(checkName, fullOutput string) string {
	tail := tailLines(fullOutput, rawDiagnosticsTailLines)
	return fmt.Sprintf("--- %s ---\n%s", checkName, tail)
}

// tailLines returns the last n lines of s. If s has fewer than n lines, it is
// returned unchanged. Empty input → empty output.
func tailLines(s string, n int) string {
	if s == "" || n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

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
// inspection. Returns the file path or empty string if debug is disabled, or
// if the output is empty — writing a 0-byte file and then pointing the verdict
// at it is the antipattern that bit us on 2026-04-24 (operator chased a path
// that led to nothing).
func (h *QualityGateHandler) saveDebugOutput(correlationID, check, output string) string {
	if h.debugDir == "" {
		return ""
	}
	if strings.TrimSpace(output) == "" {
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
