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
// Detection is by substring match against any positional arg before --json,
// so it works for both `./run.sh lint` and the wrapped form
// `bash -c "./run.sh up && ./run.sh test"`.
func fakeStackCommandFail(check string) string {
	return `#!/bin/bash
args=("$@")
matched=0
for arg in "${args[@]}"; do
    if [ "$arg" = "--json" ]; then break; fi
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
	lintCheck := qualityCheck{name: "lint", command: []string{"./run.sh", "lint"}}
	_, err := h.runCheck(context.Background(), "/ws/my-repo", lintCheck)
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
