package handler

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// setupTestGitRepo creates a minimal git repo with origin pointing to itself.
// This mirrors the helper in internal/workspace/workspace_test.go but lives
// here so workspace_test.go can be a black-box test for the handler package.
func setupTestGitRepo(t *testing.T, basePath, name string) {
	t.Helper()
	repoPath := filepath.Join(basePath, name)
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", repoPath, err)
	}
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
		{"remote", "add", "origin", repoPath},
		{"fetch", "origin"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s (%v)", args, out, err)
		}
	}
}

// --- WorkspaceHandler tests ---

func TestNewWorkspace(t *testing.T) {
	h := NewWorkspace(testDeps())
	if h.Name() != "workspace" {
		t.Errorf("want name 'workspace', got %q", h.Name())
	}
	// Subscribes returns nil for DAG-dispatched handlers — subscriptions are
	// derived from workflow Graph definitions at runtime.
	subs := h.Subscribes()
	if subs != nil {
		t.Errorf("want nil subscriptions for DAG-dispatched handler, got %v", subs)
	}
}

func TestWorkspaceHandlerNoOpWhenNoParams(t *testing.T) {
	// WorkflowRequested with no workspace params → handler returns nil (no-op).
	store := newMockStore()

	reqPayload := event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "do something",
		WorkflowID: "workspace-dev",
		Source:     "raw",
		// Repo and Ticket intentionally empty — workspace setup skipped.
	})
	reqEvt := event.New(event.WorkflowRequested, 1, reqPayload).
		WithAggregate("wf-1", 1).
		WithCorrelation("corr-1")
	store.correlationEvents["corr-1"] = append(store.correlationEvents["corr-1"], reqEvt)

	startedEvt := event.New(event.WorkflowStarted, 1, event.MustMarshal(map[string]any{})).
		WithAggregate("wf-1", 2).
		WithCorrelation("corr-1")

	h := &WorkspaceHandler{store: store}
	got, err := h.Handle(context.Background(), startedEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil result for no-op, got %v", got)
	}
}

func TestWorkspaceHandlerReturnsWorkspaceReadyOnSuccess(t *testing.T) {
	// Integration path: real git repo, full workspace provisioning.
	tmp := t.TempDir()
	t.Setenv("RICK_REPOS_PATH", tmp)
	setupTestGitRepo(t, tmp, "myrepo")

	store := newMockStore()

	reqPayload := event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "implement feature",
		WorkflowID: "workspace-dev",
		Source:     "raw",
		Repo:       "myrepo",
		Ticket:     "PROJ-1001",
		BaseBranch: "",
		Isolate:    false,
	})
	reqEvt := event.New(event.WorkflowRequested, 1, reqPayload).
		WithAggregate("wf-2", 1).
		WithCorrelation("corr-2")
	store.correlationEvents["corr-2"] = append(store.correlationEvents["corr-2"], reqEvt)

	startedEvt := event.New(event.WorkflowStarted, 1, event.MustMarshal(map[string]any{})).
		WithAggregate("wf-2", 2).
		WithCorrelation("corr-2")

	h := &WorkspaceHandler{store: store}
	got, err := h.Handle(context.Background(), startedEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].Type != event.WorkspaceReady {
		t.Errorf("expected WorkspaceReady event, got %s", got[0].Type)
	}

	var payload event.WorkspaceReadyPayload
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Branch != "PROJ-1001" {
		t.Errorf("expected branch PROJ-1001, got %s", payload.Branch)
	}
	if payload.Base != "main" {
		t.Errorf("expected base main, got %s", payload.Base)
	}
	if payload.Isolated {
		t.Error("expected Isolated=false")
	}
}

func TestWorkspaceHandlerEmptyCorrelation(t *testing.T) {
	// No correlation ID → no params loaded → no-op.
	h := NewWorkspace(testDeps())
	env := event.New(event.WorkflowStarted, 1, nil)
	// CorrelationID defaults to empty string.
	got, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty correlation, got %v", got)
	}
}

func TestWorkspaceHandlerReadsRepoFromEnrichment(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("RICK_REPOS_PATH", tmp)
	setupTestGitRepo(t, tmp, "myrepo")

	store := newMockStore()

	// WorkflowRequested without repo — repo comes from enrichment.
	reqPayload := event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "fix PROJ-1001",
		WorkflowID: "jira-dev",
		Source:     "mcp",
		Ticket:     "PROJ-1001",
		// Repo intentionally empty.
	})
	reqEvt := event.New(event.WorkflowRequested, 1, reqPayload).
		WithAggregate("wf-jira", 1).
		WithCorrelation("corr-jira")

	// jira-context enrichment provides the repo.
	enrichPayload := event.MustMarshal(event.ContextEnrichmentPayload{
		Source:  "jira-context",
		Kind:    "ticket",
		Summary: "Jira ticket PROJ-1001",
		Items: []event.EnrichmentItem{
			{Name: "ticket", Reason: "PROJ-1001"},
			{Name: "repo", Reason: "myrepo"},
		},
	})
	enrichEvt := event.New(event.ContextEnrichment, 1, enrichPayload).
		WithAggregate("wf-jira:persona:jira-context", 1).
		WithCorrelation("corr-jira")

	store.correlationEvents["corr-jira"] = []event.Envelope{reqEvt, enrichEvt}

	triggerEvt := event.New(event.PersonaCompleted, 1, nil).
		WithAggregate("wf-jira", 3).
		WithCorrelation("corr-jira")

	h := NewWorkspace(testDeps())
	h.store = store

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].Type != event.WorkspaceReady {
		t.Errorf("expected WorkspaceReady, got %s", got[0].Type)
	}

	var payload event.WorkspaceReadyPayload
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Branch != "PROJ-1001" {
		t.Errorf("expected branch PROJ-1001, got %s", payload.Branch)
	}
}

func TestWorkspaceHandlerIsolatedUsesCorrelationSuffix(t *testing.T) {
	// Isolated workspace should include correlation ID prefix in path to avoid collisions.
	tmp := t.TempDir()
	t.Setenv("RICK_REPOS_PATH", tmp)
	setupTestGitRepo(t, tmp, "myrepo")

	store := newMockStore()

	reqPayload := event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "implement feature",
		WorkflowID: "jira-dev",
		Source:     "mcp",
		Repo:       "myrepo",
		Ticket:     "PROJ-2001",
		Isolate:    true,
	})
	reqEvt := event.New(event.WorkflowRequested, 1, reqPayload).
		WithAggregate("wf-iso", 1).
		WithCorrelation("abcd1234-5678-90ef-ghij-klmnopqrstuv")
	store.correlationEvents["abcd1234-5678-90ef-ghij-klmnopqrstuv"] = append(
		store.correlationEvents["abcd1234-5678-90ef-ghij-klmnopqrstuv"], reqEvt)

	triggerEvt := event.New(event.WorkflowStarted, 1, event.MustMarshal(map[string]any{})).
		WithAggregate("wf-iso", 2).
		WithCorrelation("abcd1234-5678-90ef-ghij-klmnopqrstuv")

	h := &WorkspaceHandler{store: store}
	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}

	var payload event.WorkspaceReadyPayload
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if !payload.Isolated {
		t.Error("expected Isolated=true")
	}

	// New canonical format: <repo>-rick-ws-<id> where id is the 8-char
	// correlation prefix used as suffix by the workspace handler.
	expectedPath := filepath.Join(tmp, "myrepo-rick-ws-abcd1234")
	if payload.Path != expectedPath {
		t.Errorf("expected path %s, got %s", expectedPath, payload.Path)
	}

	t.Cleanup(func() { os.RemoveAll(expectedPath) })
}

// ---------------------------------------------------------------------------
// WorkspaceHandler error paths
// ---------------------------------------------------------------------------

func TestWorkspaceHandlerSetupWorkspaceError(t *testing.T) {
	// RICK_REPOS_PATH points to a non-existent directory — SetupWorkspace should fail.
	t.Setenv("RICK_REPOS_PATH", "/nonexistent/path/that/does/not/exist")

	store := newMockStore()
	reqPayload := event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "implement feature",
		WorkflowID: "workspace-dev",
		Repo:       "my-nonexistent-repo",
		Ticket:     "PROJ-9999",
	})
	reqEvt := event.New(event.WorkflowRequested, 1, reqPayload).
		WithAggregate("wf-err", 1).
		WithCorrelation("corr-err")
	store.correlationEvents["corr-err"] = append(store.correlationEvents["corr-err"], reqEvt)

	startedEvt := event.New(event.WorkflowStarted, 1, event.MustMarshal(map[string]any{})).
		WithAggregate("wf-err", 2).
		WithCorrelation("corr-err")

	h := &WorkspaceHandler{store: store}
	_, err := h.Handle(context.Background(), startedEvt)
	if err == nil {
		t.Fatal("expected error when SetupWorkspace fails (repo not found)")
	}
	if !strings.Contains(err.Error(), "workspace handler") {
		t.Errorf("error should mention 'workspace handler', got: %v", err)
	}
}

func TestLoadWorkspaceParamsConflictingEnrichmentRepos(t *testing.T) {
	// Multiple enrichment events with conflicting repo values.
	// The last one in the event stream wins (since we overwrite).
	store := newMockStore()

	reqPayload := event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "do something",
		WorkflowID: "jira-dev",
		// Repo intentionally empty — comes from enrichment.
	})

	// Two enrichment events with different repo values.
	enrichPayload1 := event.MustMarshal(event.ContextEnrichmentPayload{
		Source: "jira-context",
		Kind:   "ticket",
		Items:  []event.EnrichmentItem{{Name: "repo", Reason: "first-repo"}},
	})
	enrichPayload2 := event.MustMarshal(event.ContextEnrichmentPayload{
		Source: "jira-context",
		Kind:   "ticket",
		Items:  []event.EnrichmentItem{{Name: "repo", Reason: "second-repo"}},
	})

	store.correlationEvents["corr-conflict"] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, reqPayload).WithCorrelation("corr-conflict"),
		event.New(event.ContextEnrichment, 1, enrichPayload1).WithCorrelation("corr-conflict"),
		event.New(event.ContextEnrichment, 1, enrichPayload2).WithCorrelation("corr-conflict"),
	}

	// WorkspaceHandler always reads enrichment — loadWorkspaceParams checks
	// ContextEnrichment events for repo info.
	h := &WorkspaceHandler{
		store: store,
		name:  "workspace",
	}

	params, err := h.loadWorkspaceParams(context.Background(), "corr-conflict")
	if err != nil {
		t.Fatalf("loadWorkspaceParams: %v", err)
	}

	// Last enrichment item wins (overwrite behavior in the for loop).
	if params.Repo != "second-repo" {
		t.Errorf("want repo 'second-repo' (last enrichment wins), got %q", params.Repo)
	}
}

func TestLoadWorkspaceParamsEnrichmentIgnoredWhenRepoPresent(t *testing.T) {
	// When WorkflowRequested has an explicit repo, enrichment should NOT override it.
	store := newMockStore()

	reqPayload := event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "do something",
		Repo:   "explicit-repo",
	})
	enrichPayload := event.MustMarshal(event.ContextEnrichmentPayload{
		Source: "jira-context",
		Kind:   "ticket",
		Items:  []event.EnrichmentItem{{Name: "repo", Reason: "enrichment-repo"}},
	})

	store.correlationEvents["corr-explicit"] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, reqPayload).WithCorrelation("corr-explicit"),
		event.New(event.ContextEnrichment, 1, enrichPayload).WithCorrelation("corr-explicit"),
	}

	h := &WorkspaceHandler{
		store: store,
		name:  "workspace",
	}

	params, err := h.loadWorkspaceParams(context.Background(), "corr-explicit")
	if err != nil {
		t.Fatalf("loadWorkspaceParams: %v", err)
	}

	// Explicit repo from WorkflowRequested should take precedence.
	if params.Repo != "explicit-repo" {
		t.Errorf("want repo 'explicit-repo' (explicit takes precedence), got %q", params.Repo)
	}
}
