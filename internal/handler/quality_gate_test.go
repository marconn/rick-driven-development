package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// writeFakeStack creates a shell script that acts as a fake `stack` binary.
// The script receives `run <path> ./run.sh <check> --json --timeout <n>` and
// returns JSON envelopes matching the real stack CLI contract.
func writeFakeStack(t *testing.T, dir string, script string) string {
	t.Helper()
	bin := filepath.Join(dir, "fake-stack")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// fakeStackSuccess returns a script that emits NDJSON matching real stack output:
// create envelope, run envelope, destroy envelope — one per line.
func fakeStackSuccess() string {
	return `#!/bin/bash
cat <<'EOF'
{"action":"create","code_copy_path":"/tmp/test","compose_file":"deployments/docker-compose.yml","container":"huli-tmp-test","ip":"10.0.0.1","name":"tmp-test","status":"success"}
{"action":"run","exit_code":0,"kept":false,"output":"all good","stack":"tmp-test","status":"success"}
{"action":"destroy","code_copy_path":"/tmp/test","name":"tmp-test","purged":true,"status":"success"}
EOF
exit 0
`
}

// fakeStackCommandFail returns a script that emits NDJSON with a non-zero
// inner exit_code for the specified check (command failed inside VM).
// Detection scans every positional arg for a substring match, so it works
// for both `./run.sh lint` and the wrapped form
// `bash -c "./run.sh up && ./run.sh test"`, regardless of where the `--`
// separator sits in the command line.
func fakeStackCommandFail(check string) string {
	return `#!/bin/bash
args=("$@")
matched=0
for arg in "${args[@]}"; do
    if [[ "$arg" == *"./run.sh ` + check + `"* ]] || [ "$arg" = "` + check + `" ]; then
        matched=1
    fi
done
if [ "$matched" = "1" ]; then
    cat <<'EOF'
{"action":"create","code_copy_path":"/tmp/test","compose_file":"deployments/docker-compose.yml","container":"huli-tmp-test","ip":"10.0.0.1","name":"tmp-test","status":"success"}
{"action":"run","exit_code":1,"kept":false,"output":"FAIL: some test error","stack":"tmp-test","status":"success"}
{"action":"destroy","code_copy_path":"/tmp/test","name":"tmp-test","purged":true,"status":"success"}
EOF
    exit 1
fi
cat <<'EOF'
{"action":"create","code_copy_path":"/tmp/test","compose_file":"deployments/docker-compose.yml","container":"huli-tmp-test","ip":"10.0.0.1","name":"tmp-test","status":"success"}
{"action":"run","exit_code":0,"kept":false,"output":"ok","stack":"tmp-test","status":"success"}
{"action":"destroy","code_copy_path":"/tmp/test","name":"tmp-test","purged":true,"status":"success"}
EOF
exit 0
`
}

// fakeStackNoCompose returns a script that emits a stack error JSON for missing
// docker-compose.yml (exit code 31).
func fakeStackNoCompose() string {
	return `#!/bin/bash
cat <<'EOF'
{"status":"error","code":"no_compose_file","message":"no docker-compose.yml found"}
EOF
exit 31
`
}

func TestQualityGateNameAndSubscribes(t *testing.T) {
	h := NewQualityGate(testDeps())
	if h.Name() != "quality-gate" {
		t.Errorf("want name 'quality-gate', got %q", h.Name())
	}
	if h.Phase() != "quality-gate" {
		t.Errorf("want phase 'quality-gate', got %q", h.Phase())
	}
	if subs := h.Subscribes(); subs != nil {
		t.Errorf("want nil subscriptions for DAG-dispatched handler, got %v", subs)
	}
}

func TestQualityGateNoWorkspace(t *testing.T) {
	store := newMockStore()
	reqPayload := event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "do something",
		WorkflowID: "workspace-dev",
	})
	store.correlationEvents["corr-1"] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, reqPayload).WithCorrelation("corr-1"),
	}

	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: "stack", timeout: 300}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-1")

	_, err := h.Handle(context.Background(), triggerEvt)
	if err == nil {
		t.Fatal("expected error when no workspace found, got nil")
	}
	if !strings.Contains(err.Error(), "no workspace found") {
		t.Errorf("error should mention missing workspace, got: %v", err)
	}
}

func TestQualityGateNoRunScript(t *testing.T) {
	tmp := t.TempDir()

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-2"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-2"),
	}

	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: "stack", timeout: 300}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-2")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 verdict event, got %d", len(got))
	}
	assertVerdictOutcome(t, got[0], event.VerdictPass)
}

func TestQualityGatePassingChecks(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeStack := writeFakeStack(t, t.TempDir(), fakeStackSuccess())

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-3"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-3"),
	}

	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: fakeStack, timeout: 300}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-3")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 verdict event, got %d", len(got))
	}
	assertVerdictOutcome(t, got[0], event.VerdictPass)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if vp.Summary != "lint and test passed" {
		t.Errorf("unexpected summary: %s", vp.Summary)
	}
}

func TestQualityGateLintFails(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeStack := writeFakeStack(t, t.TempDir(), fakeStackCommandFail("lint"))

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-4"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-4"),
	}

	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: fakeStack, timeout: 300}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-4")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 verdict event, got %d", len(got))
	}
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if vp.Phase != "develop" {
		t.Errorf("want target phase 'develop', got %q", vp.Phase)
	}
	if vp.SourcePhase != "quality-gate" {
		t.Errorf("want source phase 'quality-gate', got %q", vp.SourcePhase)
	}
	if len(vp.Issues) == 0 {
		t.Error("expected at least one issue for lint failure")
	}
}

func TestQualityGateTestFails(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeStack := writeFakeStack(t, t.TempDir(), fakeStackCommandFail("test"))

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-5"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-5"),
	}

	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: fakeStack, timeout: 300}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-5")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if vp.Summary != "test failed" {
		t.Errorf("expected summary 'test failed', got %q", vp.Summary)
	}
}

func TestQualityGateBothFail(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Fake stack that always returns non-zero exit_code.
	fakeStack := writeFakeStack(t, t.TempDir(), `#!/bin/bash
cat <<'EOF'
{"status":"success","action":"run","exit_code":1,"stack":"tmp-test","kept":false,"output":"check failed"}
EOF
exit 1
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-6"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-6"),
	}

	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: fakeStack, timeout: 300}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-6")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if vp.Summary != "lint failed; test failed" {
		t.Errorf("expected both failures in summary, got %q", vp.Summary)
	}
	if len(vp.Issues) != 2 {
		t.Errorf("expected 2 issues (one per check), got %d", len(vp.Issues))
	}
}

func TestQualityGateNoComposeFileSkips(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Fake stack that returns no_compose_file error.
	fakeStack := writeFakeStack(t, t.TempDir(), fakeStackNoCompose())

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-7"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-7"),
	}

	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: fakeStack, timeout: 300}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-7")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 verdict event, got %d", len(got))
	}
	// Stack-level errors should result in a pass (skip), not a failure.
	assertVerdictOutcome(t, got[0], event.VerdictPass)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if vp.Summary != "stack unavailable (no_compose_file), skipping quality checks" {
		t.Errorf("unexpected summary: %s", vp.Summary)
	}
}

func TestQualityGateEmptyCorrelation(t *testing.T) {
	h := NewQualityGate(testDeps())
	env := event.New(event.PersonaCompleted, 1, nil)

	_, err := h.Handle(context.Background(), env)
	if err == nil {
		t.Fatal("expected error when no workspace found (empty correlation), got nil")
	}
	if !strings.Contains(err.Error(), "no workspace found") {
		t.Errorf("error should mention missing workspace, got: %v", err)
	}
}

func TestTruncateOutput(t *testing.T) {
	t.Run("short passthrough", func(t *testing.T) {
		if got := truncateOutput("short", 100); got != "short" {
			t.Errorf("short string should pass through unchanged, got %q", got)
		}
	})

	t.Run("head+tail strategy", func(t *testing.T) {
		// 150 A's (head noise) + 50 B's (tail errors)
		long := strings.Repeat("A", 150) + strings.Repeat("B", 50)
		got := truncateOutput(long, 100)

		// Must contain truncation marker
		if !strings.Contains(got, "(truncated)") {
			t.Errorf("should contain truncation marker, got %q", got)
		}
		// Must start with head (A's) and end with tail (B's)
		if !strings.HasPrefix(got, "AAA") {
			t.Errorf("should start with head content, got %q", got)
		}
		if !strings.HasSuffix(got, strings.Repeat("B", 50)) {
			t.Errorf("should end with tail content, got %q", got)
		}
	})

	t.Run("strips ANSI codes", func(t *testing.T) {
		// ANSI-heavy input that fits within budget after stripping
		ansi := "\x1b[2K\x1b[0A\x1b[0ECreating VM  \b/\b-\b\\\b|\b/actual error here"
		got := truncateOutput(ansi, 200)
		if strings.Contains(got, "\x1b[") {
			t.Error("output should not contain ANSI escape sequences")
		}
		if !strings.Contains(got, "actual error here") {
			t.Errorf("should preserve actual content, got %q", got)
		}
	})

	t.Run("ANSI stripping avoids truncation", func(t *testing.T) {
		// 100 chars of real content + 200 chars of ANSI noise = 300 raw bytes
		// After stripping ANSI, only 100 chars remain — fits in budget
		real := strings.Repeat("E", 100)
		ansiNoise := strings.Repeat("\x1b[2K", 50) // 200 bytes of ANSI
		got := truncateOutput(ansiNoise+real, 200)
		if strings.Contains(got, "(truncated)") {
			t.Error("should not truncate after ANSI stripping brings it under budget")
		}
		if got != real {
			t.Errorf("expected clean content only, got %q", got)
		}
	})
}

func TestStackRunResultIsStackError(t *testing.T) {
	tests := []struct {
		name   string
		result stackRunResult
		want   bool
	}{
		{"success", stackRunResult{Status: "success"}, false},
		{"command fail", stackRunResult{Status: "error", Code: "unknown"}, false},
		{"no compose", stackRunResult{Status: "error", Code: "no_compose_file"}, true},
		{"repo not found", stackRunResult{Status: "error", Code: "repo_not_found"}, true},
		{"multipass missing", stackRunResult{Status: "error", Code: "multipass_not_installed"}, true},
		{"multipass error", stackRunResult{Status: "error", Code: "multipass_error"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.isStackError(); got != tt.want {
				t.Errorf("isStackError() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestQualityGateStackCrashesNonJSON verifies the fallback path when the stack
// binary crashes or returns non-JSON output (e.g., segfault, stderr-only).
// The runCheck JSON parse fails → parse_error code. Until 2026-04-29 this was
// a normal VerdictFail, which fed cobra-echo garbage into the developer
// iteration loop (3× before the operator cancelled). After 2026-04-29 a
// parse_error escalates as advisory so the workflow pauses for operator
// review instead of looping on undiagnosable output.
func TestQualityGateStackCrashesNonJSON(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Fake stack that prints non-JSON garbage and exits non-zero.
	fakeStack := writeFakeStack(t, t.TempDir(), `#!/bin/bash
echo "Segmentation fault (core dumped)" >&2
exit 139
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-crash"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-crash"),
	}

	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: fakeStack, timeout: 300}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-crash")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 verdict event, got %d", len(got))
	}
	// parse_error escalates as advisory fail: pause for operator instead of
	// retriggering the developer with garbage diagnostics.
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if !vp.Advisory {
		t.Errorf("parse_error verdict must be Advisory=true, got Advisory=%v", vp.Advisory)
	}
	if !strings.Contains(vp.Summary, "parse_error") {
		t.Errorf("summary should mention parse_error, got: %s", vp.Summary)
	}
	if len(vp.Issues) == 0 || vp.Issues[0].Category != "infrastructure" {
		t.Errorf("expected infrastructure-category issue, got: %#v", vp.Issues)
	}
}

// TestQualityGate_RawDiagnosticsCarriedToVerdict is the regression for
// PR-D / 2026-04-29: when a real test run fails, the unfiltered tail of
// stack's stdout+stderr must reach VerdictPayload.RawDiagnostics so the
// developer's iteration prompt has actionable text even when
// buildFailureDescription's filter trimmed Issue.Description.
func TestQualityGate_RawDiagnosticsCarriedToVerdict(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Fake stack emits a realistic mix: Docker noise (would be filtered out
	// of the human-readable description) plus the actual test failure body.
	// RawDiagnostics must contain BOTH because the LLM is not the operator —
	// it benefits from the full stream when reasoning about the failure.
	fakeStack := writeFakeStack(t, t.TempDir(), `#!/bin/bash
args=("$@")
matched=0
for arg in "${args[@]}"; do
    if [[ "$arg" == *"./run.sh lint"* ]] || [ "$arg" = "lint" ]; then
        matched=1
    fi
done
if [ "$matched" = "1" ]; then
    cat <<'EOF'
{"action":"create","status":"success"}
{"action":"run","exit_code":1,"output":"Container redis Started\nContainer redis Healthy\nFAIL\tinternal/foo 0.5s\n--- FAIL: TestThing (0.01s)\n    foo_test.go:42: expected 1 got 0","stderr":"connection refused on 127.0.0.1:6379","status":"success"}
{"action":"destroy","status":"success"}
EOF
    exit 1
fi
cat <<'EOF'
{"action":"create","status":"success"}
{"action":"run","exit_code":0,"output":"ok","status":"success"}
{"action":"destroy","status":"success"}
EOF
exit 0
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-rawdiag"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-rawdiag"),
	}

	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: fakeStack, timeout: 300}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-rawdiag")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 verdict event, got %d", len(got))
	}
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if vp.RawDiagnostics == "" {
		t.Fatal("VerdictPayload.RawDiagnostics must be populated on a real failure")
	}
	for _, want := range []string{
		"--- lint ---",                  // section header per check
		"FAIL\tinternal/foo",            // actual test failure body
		"connection refused",            // stderr that filterDockerNoise would also keep but is the most common diag we lose in a real-world scenario
	} {
		if !strings.Contains(vp.RawDiagnostics, want) {
			t.Errorf("RawDiagnostics missing %q\n--- raw ---\n%s\n--- end ---", want, vp.RawDiagnostics)
		}
	}
}

// TestFormatFeedback_RendersRawDiagnostics is a small unit guard ensuring
// formatFeedback carries RawDiagnostics into the developer prompt body.
func TestFormatFeedback_RendersRawDiagnostics(t *testing.T) {
	p := event.FeedbackGeneratedPayload{
		Summary:        "lint failed",
		Issues:         []event.Issue{{Severity: "major", Category: "correctness", Description: "filtered description"}},
		RawDiagnostics: "FAIL: real test failure\nconnection refused on 127.0.0.1:6379",
	}
	got := formatFeedback(p)
	if !strings.Contains(got, "### Raw diagnostics") {
		t.Errorf("formatFeedback missing raw diagnostics section header:\n%s", got)
	}
	if !strings.Contains(got, "real test failure") {
		t.Errorf("formatFeedback missing raw diagnostic body:\n%s", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("formatFeedback missing infra-failure marker:\n%s", got)
	}
}

// TestQualityGate_ParseErrorEscalates_CobraEcho is the precise 2026-04-29
// regression: stack failed before reaching its run command and cobra echoed
// `Error: command exited with code 1` to BOTH stdout and stderr. Pre-fix Rick
// fed those bytes into the developer iteration loop as a code-regression
// verdict, burning 3 rounds of tokens. Post-fix the verdict is advisory and
// the captured bytes are persisted in the debug dir for the operator.
func TestQualityGate_ParseErrorEscalates_CobraEcho(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Mimic the cobra-default-error path that produced the 97-byte log file.
	fakeStack := writeFakeStack(t, t.TempDir(), `#!/bin/bash
echo "Error: command exited with code 1"
echo "Error: command exited with code 1" >&2
exit 1
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-cobra"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-cobra"),
	}

	debugDir := filepath.Join(t.TempDir(), "qg-debug")
	h := &QualityGateHandler{
		store:    store,
		name:     "quality-gate",
		stackBin: fakeStack,
		timeout:  300,
		debugDir: debugDir,
		logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-cobra")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 verdict event, got %d", len(got))
	}
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if !vp.Advisory {
		t.Fatalf("parse_error verdict must be Advisory=true, got: %#v", vp)
	}
	if len(vp.Issues) != 1 {
		t.Fatalf("expected exactly one issue, got: %#v", vp.Issues)
	}
	desc := vp.Issues[0].Description
	if !strings.Contains(desc, "stack output unparseable") {
		t.Errorf("issue description should explain parse_error, got: %s", desc)
	}
	if !strings.Contains(desc, "Error: command exited with code 1") {
		t.Errorf("issue description should embed the captured cobra echo, got: %s", desc)
	}
	if !strings.Contains(desc, "[full output:") {
		t.Errorf("issue description should reference the saved debug log, got: %s", desc)
	}

	// Debug artefact must exist with the raw captured bytes so the operator can
	// inspect what stack actually emitted.
	entries, rerr := os.ReadDir(debugDir)
	if rerr != nil {
		t.Fatalf("read debug dir: %v", rerr)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one qg-*.log file in the debug dir")
	}
	logBytes, lerr := os.ReadFile(filepath.Join(debugDir, entries[0].Name()))
	if lerr != nil {
		t.Fatalf("read debug log: %v", lerr)
	}
	if !strings.Contains(string(logBytes), "Error: command exited with code 1") {
		t.Errorf("debug log missing captured bytes:\n%s", string(logBytes))
	}
}

// TestQualityGateStackRepoNotFound verifies that repo_not_found stack errors
// are treated as infrastructure skip (pass), just like no_compose_file.
func TestQualityGateStackRepoNotFound(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeStack := writeFakeStack(t, t.TempDir(), `#!/bin/bash
cat <<'EOF'
{"status":"error","code":"repo_not_found","message":"repository path does not exist"}
EOF
exit 31
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-norepo"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-norepo"),
	}

	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: fakeStack, timeout: 300}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-norepo")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictPass)

	var vp event.VerdictPayload
	_ = json.Unmarshal(got[0].Payload, &vp)
	if !strings.Contains(vp.Summary, "repo_not_found") {
		t.Errorf("summary should mention repo_not_found, got: %s", vp.Summary)
	}
}

// TestQualityGateStackMultipassNotInstalled verifies that
// multipass_not_installed stack errors are treated as infrastructure skip.
func TestQualityGateStackMultipassNotInstalled(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeStack := writeFakeStack(t, t.TempDir(), `#!/bin/bash
cat <<'EOF'
{"status":"error","code":"multipass_not_installed","message":"multipass: command not found"}
EOF
exit 31
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-nomp"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-nomp"),
	}

	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: fakeStack, timeout: 300}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-nomp")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictPass)

	var vp event.VerdictPayload
	_ = json.Unmarshal(got[0].Payload, &vp)
	if !strings.Contains(vp.Summary, "multipass_not_installed") {
		t.Errorf("summary should mention multipass_not_installed, got: %s", vp.Summary)
	}
}

// TestQualityGateRunCheckPassesCorrectArgs verifies that runCheck invokes
// the stack binary with the expected argument format:
// stack run --json --timeout <n> <wsPath> -- <check command...>
//
// Stack flags must appear before the repo path and command; the `--` separator
// is mandatory so cobra does not try to parse flag-like args inside the inner
// command (e.g. `bash -c "…"` would otherwise fail with
// `unknown shorthand flag: 'c' in -c`).
func TestQualityGateRunCheckPassesCorrectArgs(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Fake stack that captures its own args into a file, then returns success.
	argsFile := filepath.Join(t.TempDir(), "captured-args")
	fakeStack := writeFakeStack(t, t.TempDir(), `#!/bin/bash
printf '%s\n' "$@" > `+argsFile+`
cat <<'EOF'
{"status":"success","action":"run","exit_code":0,"stack":"tmp-test","kept":false,"output":"ok"}
EOF
exit 0
`)

	h := &QualityGateHandler{store: newMockStore(), name: "quality-gate", stackBin: fakeStack, timeout: 42}
	testCheck := qualityCheck{name: "test", command: []string{"bash", "-c", "./run.sh up && ./run.sh test"}}
	_, err := h.runCheck(context.Background(), "/ws/my-repo", testCheck)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	argsRaw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsRaw)), "\n")

	want := []string{"run", "--json", "--timeout", "42", "/ws/my-repo", "--", "bash", "-c", "./run.sh up && ./run.sh test"}
	if len(args) != len(want) {
		t.Fatalf("expected %d args, got %d: %v", len(want), len(args), args)
	}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("arg[%d]: want %q, got %q", i, w, args[i])
		}
	}
}

// TestQualityGateContextCancellation verifies that a cancelled context
// propagates through to the stack binary execution.
func TestQualityGateContextCancellation(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Fake stack that sleeps forever (should be killed by context cancel).
	fakeStack := writeFakeStack(t, t.TempDir(), `#!/bin/bash
sleep 60
`)

	h := &QualityGateHandler{store: newMockStore(), name: "quality-gate", stackBin: fakeStack, timeout: 300}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := h.runCheck(ctx, tmp, qualityCheck{name: "lint", command: []string{"./run.sh", "lint"}})
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

// TestQualityGateFailVerdictTruncatesLargeOutput verifies that when a check
// fails with very large output, the issue description is truncated to avoid
// bloating event payloads.
func TestQualityGateFailVerdictTruncatesLargeOutput(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Generate 5000 chars of output — well above the 2000 char truncation limit.
	bigOutput := strings.Repeat("X", 5000)
	fakeStack := writeFakeStack(t, t.TempDir(), `#!/bin/bash
cat <<'EOF'
{"status":"success","action":"run","exit_code":1,"stack":"tmp-test","kept":false,"output":"`+bigOutput+`"}
EOF
exit 1
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-big"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-big"),
	}

	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: fakeStack, timeout: 300}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-big")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if len(vp.Issues) == 0 {
		t.Fatal("expected at least one issue")
	}
	// The issue description should be truncated (2000 chars + prefix + suffix).
	if len(vp.Issues[0].Description) > 2200 {
		t.Errorf("issue description should be truncated, got length %d", len(vp.Issues[0].Description))
	}
	if !strings.Contains(vp.Issues[0].Description, "(truncated)") {
		t.Error("truncated output should contain truncation marker")
	}
}

// TestQualityGateStackBinaryMissing verifies behavior when the stack binary
// path does not exist at all.
func TestQualityGateStackBinaryMissing(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-nobin"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-nobin"),
	}

	h := &QualityGateHandler{store: store, name: "quality-gate", stackBin: "/nonexistent/stack-binary", timeout: 300}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-nobin")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Missing binary → exec fails → parse_error → NOT a stack error → VerdictFail.
	assertVerdictOutcome(t, got[0], event.VerdictFail)
}

func TestParseStackNDJSON(t *testing.T) {
	t.Run("single json line", func(t *testing.T) {
		data := []byte(`{"action":"run","exit_code":0,"output":"ok","status":"success"}`)
		r, ok := parseStackNDJSON(data)
		if !ok {
			t.Fatal("expected successful parse")
		}
		if r.Action != "run" || r.ExitCode != 0 {
			t.Errorf("unexpected result: action=%s exit_code=%d", r.Action, r.ExitCode)
		}
	})

	t.Run("three line NDJSON", func(t *testing.T) {
		data := []byte(`{"action":"create","status":"success","name":"tmp-1"}
{"action":"run","exit_code":1,"output":"lint error: unused var","status":"success"}
{"action":"destroy","status":"success","name":"tmp-1"}`)
		r, ok := parseStackNDJSON(data)
		if !ok {
			t.Fatal("expected successful parse")
		}
		if r.Action != "run" {
			t.Errorf("should find run envelope, got action=%s", r.Action)
		}
		if r.ExitCode != 1 {
			t.Errorf("want exit_code=1, got %d", r.ExitCode)
		}
		if r.Output != "lint error: unused var" {
			t.Errorf("unexpected output: %s", r.Output)
		}
	})

	t.Run("NDJSON with ANSI and non-JSON lines", func(t *testing.T) {
		data := []byte("\x1b[2K\x1b[0ACreating VM  /\nImage resized.\n" +
			`{"action":"create","status":"success"}` + "\n" +
			`{"action":"run","exit_code":0,"output":"pass","status":"success"}` + "\n" +
			`{"action":"destroy","status":"success"}`)
		r, ok := parseStackNDJSON(data)
		if !ok {
			t.Fatal("expected successful parse")
		}
		if r.Action != "run" || r.Output != "pass" {
			t.Errorf("unexpected result: action=%s output=%s", r.Action, r.Output)
		}
	})

	t.Run("no json at all", func(t *testing.T) {
		data := []byte("some garbage\nmore garbage\n")
		_, ok := parseStackNDJSON(data)
		if ok {
			t.Error("expected parse failure for non-JSON input")
		}
	})

	t.Run("no run envelope falls back to last", func(t *testing.T) {
		data := []byte(`{"action":"create","status":"success"}
{"action":"destroy","status":"success","code":"no_compose_file"}`)
		r, ok := parseStackNDJSON(data)
		if !ok {
			t.Fatal("expected successful parse")
		}
		// Should return last parsed envelope (destroy).
		if r.Action != "destroy" {
			t.Errorf("expected fallback to last envelope, got action=%s", r.Action)
		}
	})
}

func TestFilterDockerNoise(t *testing.T) {
	input := strings.Join([]string{
		" Container deployments-mysql-1 Creating ",
		" Container deployments-mysql-1 Created ",
		" Container deployments-mysql-1 Starting ",
		" Container deployments-mysql-1 Started ",
		" Container deployments-redis-1 Started ",
		" Network deployments_default Creating ",
		" Network deployments_default Created ",
		"7ad3271a525f: Pulling fs layer",
		"7ad3271a525f: Verifying Checksum",
		"7ad3271a525f: Download complete",
		"7ad3271a525f: Pull complete",
		"Digest: sha256:abc123",
		"Status: Downloaded newer image for golangci/golangci-lint:v1.64.8",
		"Unable to find image 'golangci/golangci-lint:v1.64.8' locally",
		"v1.64.8: Pulling from golangci/golangci-lint",
		"actual lint error: unused variable x",
		"FAIL: TestSomething",
		"exit status 1",
	}, "\n")

	got := filterDockerNoise(input)

	// Should keep actual error lines
	if !strings.Contains(got, "actual lint error: unused variable x") {
		t.Error("should keep actual lint errors")
	}
	if !strings.Contains(got, "FAIL: TestSomething") {
		t.Error("should keep test failures")
	}
	if !strings.Contains(got, "exit status 1") {
		t.Error("should keep exit status")
	}

	// Should remove Docker noise
	if strings.Contains(got, "Container deployments") {
		t.Error("should filter Docker container lifecycle lines")
	}
	if strings.Contains(got, "Network deployments") {
		t.Error("should filter Docker network lines")
	}
	if strings.Contains(got, "Pulling fs layer") {
		t.Error("should filter Docker image pull lines")
	}
	if strings.Contains(got, "sha256:") {
		t.Error("should filter Docker digest lines")
	}
}

func TestQualityGateDebugOutput(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	debugDir := filepath.Join(t.TempDir(), "debug")

	// Fake stack that returns NDJSON with Docker noise in the output field.
	dockerNoise := strings.Repeat(" Container deployments-mysql-1 Started \\n", 50) +
		"actual error: undefined function foo"
	fakeStack := writeFakeStack(t, t.TempDir(), `#!/bin/bash
cat <<'EOF'
{"action":"create","status":"success"}
{"action":"run","exit_code":1,"output":"`+dockerNoise+`","status":"success"}
{"action":"destroy","status":"success"}
EOF
exit 1
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-debug"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-debug"),
	}

	h := &QualityGateHandler{
		store:    store,
		name:     "quality-gate",
		stackBin: fakeStack,
		timeout:  300,
		debugDir: debugDir,
		logger:   slog.Default(),
	}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-debug")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	// Verify debug file was created.
	debugFiles, err := os.ReadDir(debugDir)
	if err != nil {
		t.Fatalf("debug dir should exist: %v", err)
	}
	if len(debugFiles) == 0 {
		t.Fatal("expected at least one debug file")
	}

	// Verify verdict references the debug file.
	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if len(vp.Issues) == 0 {
		t.Fatal("expected issues")
	}
	if !strings.Contains(vp.Issues[0].Description, "[full output:") {
		t.Error("verdict should reference debug file path")
	}
	// Verify Docker noise is filtered from the verdict description.
	if strings.Contains(vp.Issues[0].Description, "Container deployments") {
		t.Error("verdict description should not contain Docker noise")
	}
	// Verify actual error survives.
	if !strings.Contains(vp.Issues[0].Description, "actual error: undefined function foo") {
		t.Errorf("verdict should contain actual error, got: %s", vp.Issues[0].Description)
	}
}

// TestQualityGateDestroysKeptStacks verifies that VMs kept on failure are
// explicitly destroyed so the next iteration starts from a clean slate.
func TestQualityGateDestroysKeptStacks(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Track destroy calls: the fake stack logs them to a file.
	destroyLog := filepath.Join(t.TempDir(), "destroy-calls")

	// Fake stack: `run` subcommand returns kept=true with a stack name;
	// `destroy` subcommand appends the stack name to destroyLog.
	fakeStack := writeFakeStack(t, t.TempDir(), `#!/bin/bash
if [ "$1" = "destroy" ]; then
    echo "$2" >> `+destroyLog+`
    exit 0
fi
cat <<'EOF'
{"action":"run","exit_code":1,"kept":true,"output":"FAIL","stack":"tmp-qg-abc123","status":"success"}
EOF
exit 1
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-kept"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-kept"),
	}

	h := &QualityGateHandler{
		store:    store,
		name:     "quality-gate",
		stackBin: fakeStack,
		timeout:  300,
		logger:   slog.Default(),
	}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-kept")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	// Verify destroy was called for the kept stacks.
	raw, err := os.ReadFile(destroyLog)
	if err != nil {
		t.Fatalf("destroy log should exist: %v", err)
	}
	destroyed := strings.Split(strings.TrimSpace(string(raw)), "\n")
	// Both lint and test runs return kept=true with the same stack name,
	// so we expect two destroy calls.
	if len(destroyed) != 2 {
		t.Fatalf("expected 2 destroy calls, got %d: %v", len(destroyed), destroyed)
	}
	for _, name := range destroyed {
		if name != "tmp-qg-abc123" {
			t.Errorf("expected destroy of 'tmp-qg-abc123', got %q", name)
		}
	}
}

// TestQualityGateNoDestroyWhenNotKept verifies that destroy is NOT called
// when the stack was already cleaned up by the run command (kept=false).
func TestQualityGateNoDestroyWhenNotKept(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	destroyLog := filepath.Join(t.TempDir(), "destroy-calls")

	fakeStack := writeFakeStack(t, t.TempDir(), `#!/bin/bash
if [ "$1" = "destroy" ]; then
    echo "$2" >> `+destroyLog+`
    exit 0
fi
cat <<'EOF'
{"action":"run","exit_code":0,"kept":false,"output":"ok","stack":"tmp-qg-xyz","status":"success"}
EOF
exit 0
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-notkept"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-notkept"),
	}

	h := &QualityGateHandler{
		store:    store,
		name:     "quality-gate",
		stackBin: fakeStack,
		timeout:  300,
		logger:   slog.Default(),
	}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-notkept")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictPass)

	// Destroy log should not exist — no kept stacks to clean up.
	if _, err := os.Stat(destroyLog); err == nil {
		raw, _ := os.ReadFile(destroyLog)
		t.Errorf("destroy should not be called when kept=false, but got: %s", string(raw))
	}
}

// assertVerdictOutcome checks that an envelope is a VerdictRendered with the expected outcome.
func assertVerdictOutcome(t *testing.T, env event.Envelope, want event.VerdictOutcome) {
	t.Helper()
	if env.Type != event.VerdictRendered {
		t.Fatalf("expected VerdictRendered event, got %s", env.Type)
	}
	var vp event.VerdictPayload
	if err := json.Unmarshal(env.Payload, &vp); err != nil {
		t.Fatalf("unmarshal verdict: %v", err)
	}
	if vp.Outcome != want {
		t.Errorf("want verdict outcome %q, got %q", want, vp.Outcome)
	}
}

// TestBuildFailureDescription_EmptyAfterFilter verifies that when
// filterDockerNoise strips every line the description falls back to the raw
// unfiltered tail instead of emitting an empty body. This is the root-cause
// fix for the 2026-04-22 docs-only PR loop where quality-gate emitted
// `./run.sh test failed:\n` with zero diagnostic content and the developer
// had to re-run the suite manually to discover what failed.
func TestBuildFailureDescription_EmptyAfterFilter(t *testing.T) {
	// All lines match the docker-compose noise regex; filter collapses to
	// empty, but the raw tail must still surface something.
	rawAllNoise := strings.Repeat(
		"Container deployments-mysql-1 Started\nNetwork deployments-default Created\n", 5)
	desc := buildFailureDescription("test", rawAllNoise, "")
	if !strings.Contains(desc, "raw tail follows") {
		t.Errorf("expected raw-tail marker when filter empties output, got:\n%s", desc)
	}
	if !strings.Contains(desc, "Container") {
		t.Errorf("raw tail should contain some of the noise we dropped, got:\n%s", desc)
	}
	if strings.HasSuffix(desc, "failed:\n") {
		t.Error("description must not end at the colon-newline — empty body is the bug we're fixing")
	}
}

// TestBuildFailureDescription_NoOutput verifies the empty-input guard —
// when the tool emitted nothing at all we still produce a non-empty
// description so the operator sees a machine-readable signal.
func TestBuildFailureDescription_NoOutput(t *testing.T) {
	desc := buildFailureDescription("test", "", "")
	if !strings.Contains(desc, "no output captured") {
		t.Errorf("expected no-output marker, got:\n%s", desc)
	}
}

// TestBuildFailureDescription_CleanedPath verifies the common case — when
// filter leaves actionable content we use it and do NOT emit the raw-tail
// marker (that's the escape hatch, not the primary path).
func TestBuildFailureDescription_CleanedPath(t *testing.T) {
	raw := "Container foo Started\nFAIL: TestImporter (expected 200, got 500)\n"
	desc := buildFailureDescription("test", raw, "")
	if !strings.Contains(desc, "FAIL: TestImporter") {
		t.Errorf("cleaned path should surface the FAIL line, got:\n%s", desc)
	}
	if strings.Contains(desc, "raw tail follows") {
		t.Error("cleaned path must not use the raw-tail escape hatch")
	}
}

// TestIsDocsOnlyDiff verifies the whitelist classifier — only real docs
// files pass; a single code file disqualifies the whole diff.
func TestIsDocsOnlyDiff(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  bool
	}{
		{"single markdown", []string{"CLAUDE.md"}, true},
		{"docs tree", []string{"docs/overview.md", "docs/api.rst"}, true},
		{"codeowners and license", []string{"CODEOWNERS", "LICENSE"}, true},
		{"mixed docs + code", []string{"README.md", "internal/foo.go"}, false},
		{"pure Go", []string{"cmd/rick/main.go"}, false},
		{"empty list", nil, false},
		{"only blank lines", []string{"  ", ""}, false},
		{"nested markdown", []string{"agent/frontend/README.md", ".github/ISSUE_TEMPLATE/bug.md"}, true},
		{"dotfile config", []string{".golangci.yml"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDocsOnlyDiff(tc.files); got != tc.want {
				t.Errorf("isDocsOnlyDiff(%v) = %v, want %v", tc.files, got, tc.want)
			}
		})
	}
}

// TestQualityGate_DocsOnlyShortCircuits verifies the end-to-end fast-pass:
// when a ContextGit event in the correlation chain reports a docs-only
// modified-files set, Handle must return VerdictPass without ever invoking
// the stack binary. Uses stackBin="/nonexistent/should-never-run" so a
// regression (gate actually spawning a subprocess) fails loudly.
func TestQualityGate_DocsOnlyShortCircuits(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	gitPayload := event.MustMarshal(event.ContextGitPayload{
		HEAD:          "deadbeef",
		Branch:        "HULI-33678-add-claude-md",
		ModifiedFiles: []string{"CLAUDE.md"},
	})
	store.correlationEvents["corr-docs"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-docs"),
		event.New(event.ContextGit, 1, gitPayload).WithCorrelation("corr-docs"),
	}

	h := &QualityGateHandler{
		store:    store,
		name:     "quality-gate",
		stackBin: "/nonexistent/should-never-run",
		timeout:  300,
		logger:   slog.Default(),
	}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-docs")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictPass)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(vp.Summary, "docs-only") {
		t.Errorf("expected docs-only marker in summary, got %q", vp.Summary)
	}
}

// TestQualityGate_MixedDiffRunsFullGate confirms the short-circuit does NOT
// trigger when the diff contains any code file — the full gate runs and
// returns the stack's verdict. Catches false positives from an over-broad
// whitelist.
func TestQualityGate_MixedDiffRunsFullGate(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeStack := writeFakeStack(t, t.TempDir(), fakeStackSuccess())

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	gitPayload := event.MustMarshal(event.ContextGitPayload{
		ModifiedFiles: []string{"CLAUDE.md", "internal/foo.go"},
	})
	store.correlationEvents["corr-mixed"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-mixed"),
		event.New(event.ContextGit, 1, gitPayload).WithCorrelation("corr-mixed"),
	}

	h := &QualityGateHandler{
		store:    store,
		name:     "quality-gate",
		stackBin: fakeStack,
		timeout:  300,
		logger:   slog.Default(),
	}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-mixed")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Full gate ran and passed (fakeStackSuccess).
	assertVerdictOutcome(t, got[0], event.VerdictPass)
	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(vp.Summary, "docs-only") {
		t.Errorf("mixed diff must not take docs-only fast-pass, got summary %q", vp.Summary)
	}
}

// TestMergeOutputAndStderr covers the helper that stitches stack's "output"
// and "stderr" JSON fields into a single verdict body. The 2026-04-22 /
// 2026-04-24 bug was precisely the empty-empty case masquerading as "no
// output captured" — we now get a non-empty body as long as either stream
// carried signal.
func TestMergeOutputAndStderr(t *testing.T) {
	tests := []struct {
		name, stdout, stderr, want string
	}{
		{"both empty", "", "", ""},
		{"only stdout", "actual failure", "", "actual failure"},
		{
			"only stderr",
			"",
			"Warning: Docker not ready",
			"[stack diagnostics / stderr]\nWarning: Docker not ready",
		},
		{
			"both present",
			"inner stdout",
			"vm diag",
			"inner stdout\n\n[stack diagnostics / stderr]\nvm diag",
		},
		{"whitespace-only stdout treated as empty", "   \n  ", "diag", "[stack diagnostics / stderr]\ndiag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeOutputAndStderr(tt.stdout, tt.stderr)
			if got != tt.want {
				t.Errorf("mergeOutputAndStderr(%q,%q)\n  got:  %q\n  want: %q", tt.stdout, tt.stderr, got, tt.want)
			}
		})
	}
}

// TestSaveDebugOutput_SkipsEmpty guards the regression from 2026-04-24 where
// an empty log file was written and its path advertised in the verdict, so
// operators chased a zero-byte artifact.
func TestSaveDebugOutput_SkipsEmpty(t *testing.T) {
	debugDir := t.TempDir()
	h := &QualityGateHandler{
		debugDir: debugDir,
		logger:   slog.Default(),
	}

	if path := h.saveDebugOutput("corr-xxx", "test", ""); path != "" {
		t.Errorf("saveDebugOutput must return empty for blank input, got %q", path)
	}
	if path := h.saveDebugOutput("corr-xxx", "test", "   \n\t "); path != "" {
		t.Errorf("saveDebugOutput must return empty for whitespace-only input, got %q", path)
	}

	entries, err := os.ReadDir(debugDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("debug dir must stay empty on blank input, got %d entries", len(entries))
	}

	// Sanity: non-empty input still writes.
	if path := h.saveDebugOutput("corr-xxx", "test", "real error"); path == "" {
		t.Error("saveDebugOutput must write for non-empty input")
	}
}

// TestQualityGate_StackStderrFieldReachesVerdict simulates stack emitting the
// new "stderr" field in its JSON envelope with an empty "output" — the exact
// shape we will see when multipass kills the VM mid-run or the inner shell
// fails before writing anything. The verdict body must carry the diagnostic
// text from the stderr field instead of collapsing to "[no output captured]".
func TestQualityGate_StackStderrFieldReachesVerdict(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Fake stack emits a run envelope with output="" and stderr populated.
	fakeStack := writeFakeStack(t, t.TempDir(), `#!/bin/bash
cat <<'EOF'
{"action":"create","status":"success"}
{"action":"run","exit_code":1,"output":"","stderr":"[WARNING] Docker may not be fully ready\n[STEP] Localize code failed","status":"success"}
{"action":"destroy","status":"success"}
EOF
exit 1
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp, Branch: "test"})
	store.correlationEvents["corr-stderr"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-stderr"),
	}

	debugDir := filepath.Join(t.TempDir(), "debug")
	h := &QualityGateHandler{
		store:    store,
		name:     "quality-gate",
		stackBin: fakeStack,
		timeout:  300,
		debugDir: debugDir,
		logger:   slog.Default(),
	}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-stderr")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if len(vp.Issues) == 0 {
		t.Fatal("expected issues")
	}
	desc := vp.Issues[0].Description
	if !strings.Contains(desc, "Docker may not be fully ready") {
		t.Errorf("verdict must contain stack stderr diagnostic, got: %s", desc)
	}
	if strings.Contains(desc, "[no output captured") {
		t.Errorf("verdict must not fall through to empty-capture branch when stderr is populated, got: %s", desc)
	}

	// Debug file must be non-empty and referenced in the verdict.
	entries, err := os.ReadDir(debugDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("debug file should be written when stderr has content")
	}
	for _, e := range entries {
		info, _ := e.Info()
		if info.Size() == 0 {
			t.Errorf("debug file %s is 0 bytes — the bug we just fixed", e.Name())
		}
	}
}
