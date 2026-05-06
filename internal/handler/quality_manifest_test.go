package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// writeManifest writes .rick/quality.yaml in the given workspace dir. Creates
// the .rick directory; fatal on any error.
func writeManifest(t *testing.T, wsPath, body string) {
	t.Helper()
	dir := filepath.Join(wsPath, ".rick")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "quality.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadQualityManifest_Missing exercises the back-compat default: no
// manifest means probing falls through; the loader returns (nil, nil).
func TestLoadQualityManifest_Missing(t *testing.T) {
	mf, err := loadQualityManifest(t.TempDir())
	if err != nil {
		t.Fatalf("missing manifest must not error, got: %v", err)
	}
	if mf != nil {
		t.Fatalf("missing manifest must return nil, got: %+v", mf)
	}
}

// TestLoadQualityManifest_ValidStack covers the common case: a stack-runtime
// manifest with two checks. Verifies field mapping and runtime default.
func TestLoadQualityManifest_ValidStack(t *testing.T) {
	tmp := t.TempDir()
	writeManifest(t, tmp, `
runtime: stack
checks:
  - name: lint
    command: ["./run.sh", "lint"]
  - name: test
    label: "./run.sh up && ./run.sh test"
    command: ["bash", "-c", "./run.sh up && ./run.sh test"]
`)
	mf, err := loadQualityManifest(tmp)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if mf.Runtime != runtimeStack {
		t.Errorf("runtime: want stack, got %q", mf.Runtime)
	}
	if len(mf.Checks) != 2 {
		t.Fatalf("want 2 checks, got %d", len(mf.Checks))
	}
	if mf.Checks[1].Label != "./run.sh up && ./run.sh test" {
		t.Errorf("test label: want explicit form, got %q", mf.Checks[1].Label)
	}
}

// TestLoadQualityManifest_RuntimeDefault verifies an omitted runtime field
// defaults to "stack" — back-compat for repos that only need the legacy mode.
func TestLoadQualityManifest_RuntimeDefault(t *testing.T) {
	tmp := t.TempDir()
	writeManifest(t, tmp, `
checks:
  - name: test
    command: ["./run.sh", "test"]
`)
	mf, err := loadQualityManifest(tmp)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if mf.Runtime != runtimeStack {
		t.Errorf("default runtime: want stack, got %q", mf.Runtime)
	}
}

// TestLoadQualityManifest_RejectsUnknownRuntime guards against typos like
// "container" or "podman" that look plausible but aren't supported.
func TestLoadQualityManifest_RejectsUnknownRuntime(t *testing.T) {
	tmp := t.TempDir()
	writeManifest(t, tmp, `
runtime: container
checks:
  - name: test
    command: ["./run.sh", "test"]
`)
	if _, err := loadQualityManifest(tmp); err == nil {
		t.Fatal("expected validation error for unknown runtime")
	}
}

// TestLoadQualityManifest_RejectsEmptyChecks: a manifest with zero checks is
// almost certainly a paste-error; rejecting at load is louder than emitting
// a successful pass-with-zero-checks verdict.
func TestLoadQualityManifest_RejectsEmptyChecks(t *testing.T) {
	tmp := t.TempDir()
	writeManifest(t, tmp, `
runtime: stack
checks: []
`)
	if _, err := loadQualityManifest(tmp); err == nil {
		t.Fatal("expected validation error for empty checks")
	}
}

// TestLoadQualityManifest_RejectsDuplicateNames: duplicate check names break
// debug filename collision (qg-*-name.log) and confuse the verdict summary.
func TestLoadQualityManifest_RejectsDuplicateNames(t *testing.T) {
	tmp := t.TempDir()
	writeManifest(t, tmp, `
checks:
  - name: test
    command: ["./run.sh", "test"]
  - name: test
    command: ["./run.sh", "test", "again"]
`)
	if _, err := loadQualityManifest(tmp); err == nil {
		t.Fatal("expected validation error for duplicate check names")
	}
}

// TestLoadQualityManifest_RejectsEmptyCommand: each check must specify a
// command argv. An empty list is a misconfiguration that would otherwise
// reach exec.Command and either panic or fail with a confusing error.
func TestLoadQualityManifest_RejectsEmptyCommand(t *testing.T) {
	tmp := t.TempDir()
	writeManifest(t, tmp, `
checks:
  - name: test
    command: []
`)
	if _, err := loadQualityManifest(tmp); err == nil {
		t.Fatal("expected validation error for empty command")
	}
}

// TestLoadQualityManifest_RejectsMalformedYAML covers operator typos that
// produce invalid YAML — must not silently fall back to probing.
func TestLoadQualityManifest_RejectsMalformedYAML(t *testing.T) {
	tmp := t.TempDir()
	writeManifest(t, tmp, "checks:\n  - name: test\n    command: [unterminated\n")
	if _, err := loadQualityManifest(tmp); err == nil {
		t.Fatal("expected parse error for malformed YAML")
	}
}

// TestDetectQualityPlan_ManifestPresentTakesPrecedence: when both .rick/quality.yaml
// and run.sh exist, the manifest wins. Validates the migration story —
// adding a manifest immediately overrides the legacy heuristic without
// requiring removal of run.sh.
func TestDetectQualityPlan_ManifestPresentTakesPrecedence(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, tmp, `
runtime: host
checks:
  - name: test
    command: ["go", "test", "./..."]
`)

	plan, err := detectQualityPlan(tmp)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if plan.runner != runnerManifest {
		t.Errorf("runner: want manifest, got %q", plan.runner)
	}
	if plan.runtime != runtimeHost {
		t.Errorf("runtime: want host, got %q", plan.runtime)
	}
	if len(plan.checks) != 1 || plan.checks[0].name != "test" {
		t.Errorf("checks: want [test], got %+v", plan.checks)
	}
}

// TestDetectQualityPlan_FallbackToProbe: no manifest, run.sh present →
// runs the legacy probing path with stack runtime. This is the back-compat
// guarantee — every repo without a manifest behaves exactly as before.
func TestDetectQualityPlan_FallbackToProbe(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	plan, err := detectQualityPlan(tmp)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if plan.runner != runnerRunSh {
		t.Errorf("runner: want run.sh, got %q", plan.runner)
	}
	if plan.runtime != runtimeStack {
		t.Errorf("runtime: want stack (probing default), got %q", plan.runtime)
	}
	if len(plan.checks) != 2 {
		t.Errorf("checks: want 2 (lint+test), got %d", len(plan.checks))
	}
}

// TestDetectQualityPlan_NoDriver: empty workspace, no manifest, no run.sh,
// no Makefile → empty plan with runnerNone. Caller emits advisory.
func TestDetectQualityPlan_NoDriver(t *testing.T) {
	plan, err := detectQualityPlan(t.TempDir())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if plan.runner != runnerNone {
		t.Errorf("runner: want none, got %q", plan.runner)
	}
	if len(plan.checks) != 0 {
		t.Errorf("checks: want 0, got %d", len(plan.checks))
	}
}

// TestDetectQualityPlan_BrokenManifestSurfacesError: a malformed manifest
// returns an error so Handle can escalate as advisory rather than silently
// fall back to probing.
func TestDetectQualityPlan_BrokenManifestSurfacesError(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, tmp, "runtime: bogus\nchecks: []\n")

	if _, err := detectQualityPlan(tmp); err == nil {
		t.Fatal("expected error from broken manifest, got nil")
	}
}

// TestQualityGate_HostRuntimeGated: manifest declares host but env is unset
// → advisory verdict ("host_runtime_not_allowed"). Default-deny on host
// runtime is the safety boundary; a missing env var must not fail open.
func TestQualityGate_HostRuntimeGated(t *testing.T) {
	t.Setenv("RICK_ALLOW_HOST_RUNTIME", "")
	tmp := t.TempDir()
	writeManifest(t, tmp, `
runtime: host
checks:
  - name: test
    command: ["true"]
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-host-gated"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-host-gated"),
	}
	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: "stack", timeout: 300, logger: slog.Default()}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-host-gated")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if !vp.Advisory {
		t.Errorf("host-runtime gating must produce advisory verdict, got non-advisory")
	}
	if !strings.Contains(vp.Summary, "host_runtime_not_allowed") {
		t.Errorf("summary should name the gating reason; got %q", vp.Summary)
	}
}

// TestQualityGate_HostRuntime_Executes: with the env set and a manifest
// declaring host runtime + an always-passing command, the verdict is pass
// and no stack VM is spawned.
func TestQualityGate_HostRuntime_Executes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host runtime test uses /bin/true; not portable to windows")
	}
	t.Setenv("RICK_ALLOW_HOST_RUNTIME", "1")
	tmp := t.TempDir()
	writeManifest(t, tmp, `
runtime: host
checks:
  - name: test
    command: ["true"]
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-host-ok"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-host-ok"),
	}
	// A bogus stackBin to prove host runtime never invokes it.
	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: "/nonexistent/stack", timeout: 300, logger: slog.Default()}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-host-ok")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictPass)
}

// TestQualityGate_HostRuntime_ExecMissing: a manifest pointing at a
// nonexistent executable must produce an advisory infrastructure verdict
// (host_executable_missing), not a "test failed" regression report. The
// developer cannot fix a missing binary.
func TestQualityGate_HostRuntime_ExecMissing(t *testing.T) {
	t.Setenv("RICK_ALLOW_HOST_RUNTIME", "1")
	tmp := t.TempDir()
	writeManifest(t, tmp, `
runtime: host
checks:
  - name: test
    command: ["this-binary-does-not-exist-anywhere"]
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-missing-bin"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-missing-bin"),
	}
	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: "stack", timeout: 300, logger: slog.Default()}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-missing-bin")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if !vp.Advisory {
		t.Errorf("missing executable must produce advisory verdict (operator action required)")
	}
	if len(vp.Issues) == 0 || vp.Issues[0].Category != "infrastructure" {
		t.Errorf("issue category: want infrastructure, got %+v", vp.Issues)
	}
}

// TestParseStackNDJSON_LargeRunEnvelope is the regression for the 2026-05-06
// huli-api silent failure. Stack inlines 100KB+ of docker-pull noise into
// the "run" envelope's stderr field; bufio.Scanner's default 64KB buffer
// silently dropped the line and we fell back to the empty "create" envelope.
// This test feeds a 200KB envelope (well above 64KB, well below 16MB) and
// verifies the run envelope's payload survives intact.
func TestParseStackNDJSON_LargeRunEnvelope(t *testing.T) {
	// Build a stderr blob big enough to blow past the old 64 KiB ceiling.
	// 200 KiB of repeating filler is enough; the failure mode in production
	// was 130 KiB.
	bigStderr := strings.Repeat("docker-pull-noise ", 12*1024) // ~204 KiB
	runLine, err := json.Marshal(stackRunResult{
		Action:   "run",
		Status:   "success",
		ExitCode: 1,
		Output:   "",
		Stderr:   bigStderr + "\nno such service: code\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	ndjson := []byte(`{"action":"create","status":"success"}` + "\n" +
		string(runLine) + "\n" +
		`{"action":"destroy","status":"success"}` + "\n")

	got, ok := parseStackNDJSON(ndjson)
	if !ok {
		t.Fatal("parseStackNDJSON returned !ok for a 200KB run envelope; the 64KB scanner limit is back")
	}
	if got.Action != "run" {
		t.Errorf("action: want run, got %q (parser fell back to create/destroy)", got.Action)
	}
	if got.ExitCode != 1 {
		t.Errorf("exit_code: want 1, got %d", got.ExitCode)
	}
	if !strings.Contains(got.Stderr, "no such service: code") {
		t.Errorf("stderr lost the diagnostic; first 200 chars: %q", truncateForLog(got.Stderr, 200))
	}
}

// TestParseStackNDJSON_OversizedLine: a single NDJSON line beyond our 16 MiB
// ceiling is treated as a hard parse failure (returns false). Better to
// surface parse_error to the operator than silently fall back to a partial
// parse that loses the run envelope.
//
// The test prepends a small envelope before the oversized one so the data
// is NDJSON (multi-line). A single-JSON-document fast path in parseStackNDJSON
// would otherwise short-circuit before the scanner runs, hiding the size
// limit. Production stack output is always multi-line.
func TestParseStackNDJSON_OversizedLine(t *testing.T) {
	bigStderr := strings.Repeat("x", 17*1024*1024)
	bigLine := fmt.Sprintf(`{"action":"run","status":"success","stderr":%q}`, bigStderr)
	ndjson := []byte(`{"action":"create","status":"success"}` + "\n" + bigLine + "\n")

	if _, ok := parseStackNDJSON(ndjson); ok {
		t.Fatal("oversized line must produce parseOK=false; partial parses are not allowed")
	}
}

// truncateForLog returns the first n chars of s for compact test failure
// messages. Defined locally to avoid coupling the test to truncateOutput's
// head+tail strategy (which would obscure parse failures here).
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
