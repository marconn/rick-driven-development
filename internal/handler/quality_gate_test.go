package handler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil result when no workspace, got %d events", len(got))
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

	got, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty correlation, got %v", got)
	}
}

func TestTruncateOutput(t *testing.T) {
	short := "short"
	if got := truncateOutput(short, 100); got != short {
		t.Errorf("short string should pass through unchanged, got %q", got)
	}

	long := string(make([]byte, 200))
	got := truncateOutput(long, 100)
	if len(got) > 120 { // 100 + truncation message
		t.Errorf("output should be truncated, got length %d", len(got))
	}
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
