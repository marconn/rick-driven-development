package handler

import (
	"context"
	"encoding/json"
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

// fakeStackSuccess returns a script that emits a stack run success JSON envelope.
func fakeStackSuccess() string {
	return `#!/bin/bash
cat <<'EOF'
{"status":"success","action":"run","exit_code":0,"stack":"tmp-test","kept":false,"output":"all good"}
EOF
exit 0
`
}

// fakeStackCommandFail returns a script that emits a stack run success envelope
// but with non-zero inner exit_code (command failed inside VM).
func fakeStackCommandFail(check string) string {
	return `#!/bin/bash
# Extract check name: last arg before --json
args=("$@")
check=""
for arg in "${args[@]}"; do
    if [ "$arg" = "--json" ]; then break; fi
    check="$arg"
done
if [ "$check" = "` + check + `" ]; then
    cat <<'EOF'
{"status":"success","action":"run","exit_code":1,"stack":"tmp-test","kept":false,"output":"FAIL: some test error"}
EOF
    exit 1
fi
cat <<'EOF'
{"status":"success","action":"run","exit_code":0,"stack":"tmp-test","kept":false,"output":"ok"}
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
// The runCheck JSON parse fails → parse_error code → NOT a stack error → VerdictFail.
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
	// parse_error is NOT a stack infrastructure error → should be VerdictFail, not skip.
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(vp.Summary, "lint failed") {
		t.Errorf("summary should report lint failure, got: %s", vp.Summary)
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
// stack run <wsPath> ./run.sh <check> --json --timeout <n>
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
	_, err := h.runCheck(context.Background(), "/ws/my-repo", "lint")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	argsRaw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsRaw)), "\n")

	want := []string{"run", "/ws/my-repo", "./run.sh", "lint", "--json", "--timeout", "42"}
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

	_, err := h.runCheck(ctx, tmp, "lint")
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
