package handler

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/persona"
)

// initGitRepo creates a minimal git repo in dir with an initial commit.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %s (%v)", strings.Join(args, " "), out, err)
		}
	}
}

// initGitRepoWithRemote creates a bare remote and a local clone so that
// git log @{u}..HEAD and origin/<branch> comparisons work.
func initGitRepoWithRemote(t *testing.T) (localDir string) {
	t.Helper()
	bareDir := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.Command("git", "init", "--bare", bareDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %s (%v)", out, err)
	}

	localDir = filepath.Join(t.TempDir(), "local")
	cmd = exec.Command("git", "clone", bareDir, localDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %s (%v)", out, err)
	}

	for _, args := range [][]string{
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
		{"push", "origin", "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = localDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %s (%v)", strings.Join(args, " "), out, err)
		}
	}
	return localDir
}

func committerHandler(t *testing.T, store *mockStore) *CommitterHandler {
	t.Helper()
	return NewCommitterHandler(AIHandlerConfig{
		Name:     "committer",
		Phase:    "commit",
		Persona:  persona.Committer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "committed", Duration: time.Second}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})
}

func TestCommitterNameAndPhase(t *testing.T) {
	h := committerHandler(t, newMockStore())
	if h.Name() != "committer" {
		t.Errorf("want name 'committer', got %q", h.Name())
	}
	if h.Phase() != "commit" {
		t.Errorf("want phase 'commit', got %q", h.Phase())
	}
	if subs := h.Subscribes(); subs != nil {
		t.Errorf("want nil subscriptions for DAG-dispatched handler, got %v", subs)
	}
}

// TestCommitterNoChangesEmitsVerdictFail verifies that the committer emits
// VerdictFail when the workspace has no uncommitted changes and no unpushed
// commits. This is the core regression test for the ci-fix workflow completing
// without producing a fix.
func TestCommitterNoChangesEmitsVerdictFail(t *testing.T) {
	wsDir := initGitRepoWithRemote(t)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{
		Path:   wsDir,
		Branch: "main",
		Base:   "main",
	})
	store.correlationEvents["corr-nochange"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-nochange"),
	}

	h := committerHandler(t, store)
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-nochange")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event (VerdictFail), got %d", len(got))
	}
	if got[0].Type != event.VerdictRendered {
		t.Fatalf("expected VerdictRendered, got %s", got[0].Type)
	}

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if vp.Outcome != event.VerdictFail {
		t.Errorf("want VerdictFail, got %q", vp.Outcome)
	}
	if vp.Phase != "develop" {
		t.Errorf("want target phase 'develop', got %q", vp.Phase)
	}
	if vp.SourcePhase != "commit" {
		t.Errorf("want source phase 'commit', got %q", vp.SourcePhase)
	}
	if !strings.Contains(vp.Summary, "no code changes") {
		t.Errorf("summary should mention no code changes, got: %s", vp.Summary)
	}
}

// TestCommitterWithUncommittedChanges verifies that uncommitted file changes
// cause the committer to delegate to the AI handler (not short-circuit).
func TestCommitterWithUncommittedChanges(t *testing.T) {
	wsDir := initGitRepoWithRemote(t)

	// Create an uncommitted file.
	if err := os.WriteFile(filepath.Join(wsDir, "fix.go"), []byte("package fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{
		Path:   wsDir,
		Branch: "main",
		Base:   "main",
	})
	store.correlationEvents["corr-uncommitted"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-uncommitted"),
	}

	h := committerHandler(t, store)
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-uncommitted")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// AI handler returns AIRequestSent + AIResponseReceived.
	if len(got) != 2 {
		t.Fatalf("expected 2 AI events, got %d", len(got))
	}
	hasReq := false
	hasResp := false
	for _, e := range got {
		if e.Type == event.AIRequestSent {
			hasReq = true
		}
		if e.Type == event.AIResponseReceived {
			hasResp = true
		}
	}
	if !hasReq || !hasResp {
		t.Error("expected AIRequestSent and AIResponseReceived from delegated AI handler")
	}
}

// TestCommitterWithUnpushedCommits verifies that local commits not yet pushed
// to the remote cause the committer to delegate to the AI handler.
func TestCommitterWithUnpushedCommits(t *testing.T) {
	wsDir := initGitRepoWithRemote(t)

	// Make a local commit that hasn't been pushed.
	if err := os.WriteFile(filepath.Join(wsDir, "fix.go"), []byte("package fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "fix.go"},
		{"commit", "-m", "local fix"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wsDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %s (%v)", strings.Join(args, " "), out, err)
		}
	}

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{
		Path:   wsDir,
		Branch: "main",
		Base:   "main",
	})
	store.correlationEvents["corr-unpushed"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-unpushed"),
	}

	h := committerHandler(t, store)
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-unpushed")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should delegate to AI (has unpushed commits).
	if len(got) != 2 {
		t.Fatalf("expected 2 AI events for unpushed commits, got %d", len(got))
	}
}

// TestCommitterNoWorkspaceSkipsCheck verifies that prompt-only workflows
// (no workspace) skip the change check and delegate directly to the AI handler.
func TestCommitterNoWorkspaceSkipsCheck(t *testing.T) {
	store := newMockStore()
	store.correlationEvents["corr-nows"] = []event.Envelope{
		event.New(event.WorkflowRequested, 1,
			event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "do stuff"}),
		).WithCorrelation("corr-nows"),
	}

	h := committerHandler(t, store)
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-nows")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No workspace → skip change check → delegate to AI.
	if len(got) != 2 {
		t.Fatalf("expected 2 AI events for prompt-only workflow, got %d", len(got))
	}
}

// TestWorkspaceHasChangesCleanRepo verifies the helper returns false for
// a clean repo with no uncommitted changes or unpushed commits.
func TestWorkspaceHasChangesCleanRepo(t *testing.T) {
	wsDir := initGitRepoWithRemote(t)

	has, err := workspaceHasChanges(context.Background(), wsDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Error("expected no changes in clean repo")
	}
}

// TestWorkspaceHasChangesUntracked verifies the helper detects untracked files.
func TestWorkspaceHasChangesUntracked(t *testing.T) {
	wsDir := initGitRepoWithRemote(t)

	if err := os.WriteFile(filepath.Join(wsDir, "new.go"), []byte("package new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	has, err := workspaceHasChanges(context.Background(), wsDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !has {
		t.Error("expected changes for untracked file")
	}
}

// TestWorkspaceHasChangesLocalNoRemote verifies the fallback path when no
// upstream tracking branch is set (e.g., newly created local branch).
func TestWorkspaceHasChangesLocalNoRemote(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// No remote set — the @{u} check will fail, fallback to origin/<branch>.
	// Since there's no origin, fallback also fails → returns false.
	has, err := workspaceHasChanges(context.Background(), dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Error("expected no changes when no remote is configured")
	}
}

func TestResolveWorkspacePath(t *testing.T) {
	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{
		Path:   "/tmp/test-workspace",
		Branch: "feature",
	})
	store.correlationEvents["corr-ws"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-ws"),
	}

	path, err := resolveWorkspacePath(context.Background(), store, "corr-ws")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "/tmp/test-workspace" {
		t.Errorf("expected /tmp/test-workspace, got %q", path)
	}
}

func TestResolveWorkspacePathEmpty(t *testing.T) {
	path, err := resolveWorkspacePath(context.Background(), newMockStore(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path for empty correlation, got %q", path)
	}
}

func TestResolveWorkspacePathNoEvents(t *testing.T) {
	store := newMockStore()
	store.correlationEvents["corr-empty"] = []event.Envelope{
		event.New(event.WorkflowRequested, 1,
			event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "test"}),
		).WithCorrelation("corr-empty"),
	}

	path, err := resolveWorkspacePath(context.Background(), store, "corr-empty")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path when no WorkspaceReady, got %q", path)
	}
}
