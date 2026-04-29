package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// phasedDeveloperHandler is a stubHandler that also implements handler.Phased
// returning "develop" — exercises the Phased-based detection branch.
type phasedDeveloperHandler struct {
	stubHandler
	phase string
}

func (p *phasedDeveloperHandler) Phase() string { return p.phase }

// gitInitWith prepares a fresh git repo in a temp dir, makes one initial
// commit, and optionally stages a file to leave the working tree dirty.
func gitInitWith(t *testing.T, dirty bool) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "commit.gpgsign", "false")
	run("config", "user.email", "test@test")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", "base.txt")
	run("commit", "-q", "-m", "base")
	if dirty {
		if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("work\n"), 0o644); err != nil {
			t.Fatalf("write staged: %v", err)
		}
		run("add", "staged.txt")
	}
	return dir
}

// seedWorkspaceReady appends a WorkspaceReady event onto the correlation chain
// of the runner's store so developerOutputGuardTrips can resolve the path.
func seedWorkspaceReady(t *testing.T, runner *PersonaRunner, correlationID, path string) {
	t.Helper()
	wsReady := event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
		Path: path, Branch: "feature", Base: "main",
	})).WithCorrelation(correlationID).WithAggregate(correlationID+":ws", 1)
	if err := runner.store.Append(context.Background(), correlationID+":ws", 0, []event.Envelope{wsReady}); err != nil {
		t.Fatalf("seed WorkspaceReady: %v", err)
	}
}

// TestDeveloperOutputGuard_TripsOnTinyOutputWithDirtyWorkspace is the 2026-04-29
// regression: a developer-class handler emitted a tiny output (~7 bytes) while
// the workspace had uncommitted changes; reviewer downstream then FAILed every
// iteration on garbage. The guard must classify this as PersonaFailed with
// FailureKindOutputTruncated so the workflow pauses instead of looping.
func TestDeveloperOutputGuard_TripsOnTinyOutputWithDirtyWorkspace(t *testing.T) {
	runner, _, _, _ := newTestPersonaRunner(t)
	dir := gitInitWith(t, true /* dirty */)
	seedWorkspaceReady(t, runner, "corr-tiny", dir)

	h := &stubHandler{name: "developer"}
	if !runner.developerOutputGuardTrips(h, "corr-tiny", 7 /* len(`["sub"]`) */) {
		t.Fatal("guard should have tripped: developer + 7-byte output + dirty workspace")
	}
}

// TestDeveloperOutputGuard_QuietOnLargeOutput verifies legitimate developer
// runs (multi-KB structured output) do not trip the guard regardless of
// workspace state — false positives would block normal completion.
func TestDeveloperOutputGuard_QuietOnLargeOutput(t *testing.T) {
	runner, _, _, _ := newTestPersonaRunner(t)
	dir := gitInitWith(t, true /* dirty */)
	seedWorkspaceReady(t, runner, "corr-large", dir)

	h := &stubHandler{name: "developer"}
	if runner.developerOutputGuardTrips(h, "corr-large", 1024) {
		t.Error("guard must not trip on legitimate multi-KB output")
	}
}

// TestDeveloperOutputGuard_QuietOnCleanWorkspace asserts that a tiny output
// against a clean workspace is not flagged — no divergence between captured
// output and workspace state means the model didn't actually do work, which is
// a different failure mode handled elsewhere (review verdicts).
func TestDeveloperOutputGuard_QuietOnCleanWorkspace(t *testing.T) {
	runner, _, _, _ := newTestPersonaRunner(t)
	dir := gitInitWith(t, false /* clean */)
	seedWorkspaceReady(t, runner, "corr-clean", dir)

	h := &stubHandler{name: "developer"}
	if runner.developerOutputGuardTrips(h, "corr-clean", 7) {
		t.Error("guard must not trip on tiny output when workspace is clean")
	}
}

// TestDeveloperOutputGuard_QuietOnNonDeveloperHandler asserts the guard only
// applies to developer-class handlers. Architects/reviewers/etc. that produce
// short structured output are legitimate — no false fails.
func TestDeveloperOutputGuard_QuietOnNonDeveloperHandler(t *testing.T) {
	runner, _, _, _ := newTestPersonaRunner(t)
	dir := gitInitWith(t, true /* dirty */)
	seedWorkspaceReady(t, runner, "corr-nondev", dir)

	h := &stubHandler{name: "researcher"}
	if runner.developerOutputGuardTrips(h, "corr-nondev", 7) {
		t.Error("guard must not trip on non-developer handlers")
	}
}

// TestDeveloperOutputGuard_DetectsDeveloperViaPhased asserts handler.Phased
// returning "develop" enables the guard even when the handler name differs.
func TestDeveloperOutputGuard_DetectsDeveloperViaPhased(t *testing.T) {
	runner, _, _, _ := newTestPersonaRunner(t)
	dir := gitInitWith(t, true /* dirty */)
	seedWorkspaceReady(t, runner, "corr-phased", dir)

	h := &phasedDeveloperHandler{
		stubHandler: stubHandler{name: "custom-developer-name"},
		phase:       "develop",
	}
	if !runner.developerOutputGuardTrips(h, "corr-phased", 7) {
		t.Error("guard must trip when handler.Phased reports develop phase")
	}
}

// TestDeveloperOutputGuard_QuietWhenNoWorkspace asserts the guard stands down
// for develop-only / synthetic flows that have no WorkspaceReady on the chain;
// the divergence signal is meaningless without a checkout to compare against.
func TestDeveloperOutputGuard_QuietWhenNoWorkspace(t *testing.T) {
	runner, _, _, _ := newTestPersonaRunner(t)

	h := &stubHandler{name: "developer"}
	if runner.developerOutputGuardTrips(h, "corr-no-ws", 7) {
		t.Error("guard must not trip when no WorkspaceReady is on the chain")
	}
}

// TestAIResponseTextLen covers the payload-decoding branches of the guard so
// future schema changes break tests rather than silently disable the guard.
func TestAIResponseTextLen(t *testing.T) {
	t.Run("plain_text_output", func(t *testing.T) {
		raw, _ := json.Marshal("the JWT contains the claims [\"sub\"] for verification")
		payload := event.MustMarshal(event.AIResponsePayload{Output: raw, Structured: false})
		if got := aiResponseTextLen(payload); got != len("the JWT contains the claims [\"sub\"] for verification") {
			t.Errorf("plain-text length: got %d", got)
		}
	})
	t.Run("structured_json_output", func(t *testing.T) {
		payload := event.MustMarshal(event.AIResponsePayload{Output: json.RawMessage(`["sub"]`), Structured: true})
		if got := aiResponseTextLen(payload); got != 7 {
			t.Errorf("structured length: got %d", got)
		}
	})
	t.Run("malformed_payload", func(t *testing.T) {
		if got := aiResponseTextLen([]byte("{not json")); got != -1 {
			t.Errorf("malformed payload should return -1 (fail-open), got %d", got)
		}
	})
}
