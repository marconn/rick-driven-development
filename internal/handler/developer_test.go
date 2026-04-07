package handler

import (
	"context"
	"encoding/json"
	"errors"
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

// ---------------------------------------------------------------------------
// mockStoreError returns an error from LoadByCorrelation after the first
// successful call. This lets the AIHandler's buildPromptContext succeed on
// the first call, then simulates a transient failure on the subsequent
// resolveWorkspace call inside DeveloperHandler. It embeds *mockStore so all
// other Store interface methods delegate to the stubs in ai_test.go.
// ---------------------------------------------------------------------------

type mockStoreError struct {
	*mockStore
	err   error
	calls int // number of LoadByCorrelation calls made
}

func (s *mockStoreError) LoadByCorrelation(ctx context.Context, correlationID string) ([]event.Envelope, error) {
	s.calls++
	if s.calls > 1 {
		// First call succeeds (AI buildPromptContext). Subsequent calls fail
		// (DeveloperHandler's resolveWorkspace post-Handle check).
		return nil, s.err
	}
	return s.mockStore.LoadByCorrelation(ctx, correlationID)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// newDeveloperHandlerForTest creates a DeveloperHandler backed by the given
// store. The mock backend always returns a successful response.
func newDeveloperHandlerForTest(t *testing.T, store *mockStore) *DeveloperHandler {
	t.Helper()
	return NewDeveloperHandler(AIHandlerConfig{
		Name:     "developer",
		Phase:    "develop",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "implemented", Duration: time.Second}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})
}

// newDeveloperHandlerWithErrStore creates a DeveloperHandler backed by a
// store that errors on LoadByCorrelation.
func newDeveloperHandlerWithErrStore(t *testing.T, store *mockStoreError) *DeveloperHandler {
	t.Helper()
	return NewDeveloperHandler(AIHandlerConfig{
		Name:     "developer",
		Phase:    "develop",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", response: &backend.Response{Output: "implemented", Duration: time.Second}},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})
}

// ---------------------------------------------------------------------------
// Interface compliance
// ---------------------------------------------------------------------------

func TestDeveloperHandlerNameAndPhase(t *testing.T) {
	h := newDeveloperHandlerForTest(t, newMockStore())
	if h.Name() != "developer" {
		t.Errorf("want name 'developer', got %q", h.Name())
	}
	if h.Phase() != "develop" {
		t.Errorf("want phase 'develop', got %q", h.Phase())
	}
	if subs := h.Subscribes(); subs != nil {
		t.Errorf("want nil subscriptions for DAG-dispatched handler, got %v", subs)
	}
}

// ---------------------------------------------------------------------------
// Test case 1: No workspace in correlation chain.
//
// The AI returns 2 events (AIRequestSent + AIResponseReceived). No
// WorkspaceReady event exists in the store so resolveWorkspace returns an
// empty path. Expect: exactly those 2 events, no VerdictRendered appended.
// ---------------------------------------------------------------------------

func TestDeveloperNoWorkspacePassThrough(t *testing.T) {
	store := newMockStore()
	store.correlationEvents["corr-nows"] = []event.Envelope{
		event.New(event.WorkflowRequested, 1,
			event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "do stuff"}),
		).WithCorrelation("corr-nows"),
	}

	h := newDeveloperHandlerForTest(t, store)
	got, err := h.Handle(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-nows"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// AI returns AIRequestSent + AIResponseReceived — no verdict appended.
	if len(got) != 2 {
		t.Fatalf("expected 2 AI events, got %d", len(got))
	}
	for _, e := range got {
		if e.Type == event.VerdictRendered {
			t.Error("unexpected VerdictRendered: no workspace path should skip the check")
		}
	}
}

// ---------------------------------------------------------------------------
// Test case 2: Workspace with uncommitted changes.
//
// A tempdir git repo with an untracked file. workspaceHasChanges returns true
// → AI events returned unchanged, no VerdictRendered appended.
// ---------------------------------------------------------------------------

func TestDeveloperWithUncommittedChanges(t *testing.T) {
	wsDir := initGitRepoWithRemote(t)
	if err := os.WriteFile(filepath.Join(wsDir, "impl.go"), []byte("package impl\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := newMockStore()
	store.correlationEvents["corr-uncommitted"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
			Path: wsDir, Branch: "main", Base: "main",
		})).WithCorrelation("corr-uncommitted"),
	}

	h := newDeveloperHandlerForTest(t, store)
	got, err := h.Handle(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-uncommitted"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 AI events (uncommitted changes detected), got %d", len(got))
	}
	for _, e := range got {
		if e.Type == event.VerdictRendered {
			t.Error("unexpected VerdictRendered: workspace has uncommitted changes")
		}
	}
}

// ---------------------------------------------------------------------------
// Test case 3: Workspace with unpushed commit.
//
// A tempdir git repo with a committed-but-not-pushed change. The @{u}..HEAD
// check detects it → no VerdictRendered appended.
// ---------------------------------------------------------------------------

func TestDeveloperWithUnpushedCommit(t *testing.T) {
	wsDir := initGitRepoWithRemote(t)

	if err := os.WriteFile(filepath.Join(wsDir, "impl.go"), []byte("package impl\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "impl.go"},
		{"commit", "-m", "implement feature"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wsDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %s (%v)", strings.Join(args, " "), out, err)
		}
	}

	store := newMockStore()
	store.correlationEvents["corr-unpushed"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
			Path: wsDir, Branch: "main", Base: "main",
		})).WithCorrelation("corr-unpushed"),
	}

	h := newDeveloperHandlerForTest(t, store)
	got, err := h.Handle(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-unpushed"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 AI events (unpushed commit detected), got %d", len(got))
	}
	for _, e := range got {
		if e.Type == event.VerdictRendered {
			t.Error("unexpected VerdictRendered: workspace has unpushed commits")
		}
	}
}

// ---------------------------------------------------------------------------
// Test case 4: Clean workspace — hallucination detected.
//
// A freshly cloned repo where the AI ran but produced no changes. Expect:
// 2 AI events + 1 VerdictRendered{fail, phase=develop, issues non-empty}.
// This is the primary regression test for HULI-33546.
// ---------------------------------------------------------------------------

func TestDeveloperCleanWorkspaceEmitsVerdictFail(t *testing.T) {
	wsDir := initGitRepoWithRemote(t)

	store := newMockStore()
	store.correlationEvents["corr-clean"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
			Path: wsDir, Branch: "main", Base: "main",
		})).WithCorrelation("corr-clean"),
	}

	h := newDeveloperHandlerForTest(t, store)
	got, err := h.Handle(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-clean"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 2 AI events + 1 VerdictRendered.
	if len(got) != 3 {
		t.Fatalf("expected 3 events (2 AI + 1 verdict), got %d: %v", len(got), eventTypes(got))
	}

	verdictEvt := got[2]
	if verdictEvt.Type != event.VerdictRendered {
		t.Fatalf("expected VerdictRendered as last event, got %s", verdictEvt.Type)
	}

	var vp event.VerdictPayload
	if err := json.Unmarshal(verdictEvt.Payload, &vp); err != nil {
		t.Fatalf("unmarshal VerdictPayload: %v", err)
	}
	if vp.Outcome != event.VerdictFail {
		t.Errorf("want VerdictFail, got %q", vp.Outcome)
	}
	if vp.Phase != "develop" {
		t.Errorf("want target phase 'develop', got %q", vp.Phase)
	}
	if vp.SourcePhase != "develop" {
		t.Errorf("want source phase 'develop', got %q", vp.SourcePhase)
	}
	if len(vp.Issues) == 0 {
		t.Error("expected at least one issue in verdict")
	}
	if !strings.Contains(vp.Summary, "hallucinated") {
		t.Errorf("summary should mention hallucination, got: %q", vp.Summary)
	}
}

// ---------------------------------------------------------------------------
// Test case 5: AI handler errors.
//
// The fake backend returns an error. DeveloperHandler must propagate
// (nil, err) without running the workspace check.
// ---------------------------------------------------------------------------

func TestDeveloperAIErrorPropagated(t *testing.T) {
	store := newMockStore()
	store.correlationEvents["corr-aierr"] = []event.Envelope{
		event.New(event.WorkflowRequested, 1,
			event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "do stuff"}),
		).WithCorrelation("corr-aierr"),
	}

	aiErr := errors.New("backend timeout")
	h := NewDeveloperHandler(AIHandlerConfig{
		Name:     "developer",
		Phase:    "develop",
		Persona:  persona.Developer,
		Backend:  &mockBackend{name: "claude", err: aiErr},
		Store:    store,
		Personas: persona.DefaultRegistry(),
		Builder:  persona.NewPromptBuilder(),
	})

	got, err := h.Handle(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-aierr"))
	if err == nil {
		t.Fatal("expected error propagation from AI backend failure")
	}
	if !strings.Contains(err.Error(), "backend") {
		t.Errorf("expected backend error, got: %v", err)
	}
	for _, e := range got {
		if e.Type == event.VerdictRendered {
			t.Error("unexpected VerdictRendered on AI error path")
		}
	}
}

// ---------------------------------------------------------------------------
// Test case 6: resolveWorkspace error (store unavailable).
//
// When LoadByCorrelation errors, DeveloperHandler treats the result as
// best-effort and returns AI events unchanged. We prefer a false-negative
// (missed hallucination) over blocking legitimate work due to infrastructure
// failure — the committer will still catch it at commit time.
// ---------------------------------------------------------------------------

func TestDeveloperResolveWorkspaceError(t *testing.T) {
	base := newMockStore()
	// Seed the base store so the AI's buildPromptContext succeeds on the first
	// LoadByCorrelation call.
	base.correlationEvents["corr-storeerr"] = []event.Envelope{
		event.New(event.WorkflowRequested, 1,
			event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "do stuff"}),
		).WithCorrelation("corr-storeerr"),
	}
	storeErr := &mockStoreError{
		mockStore: base,
		err:       errors.New("store unavailable"),
	}

	h := newDeveloperHandlerWithErrStore(t, storeErr)
	got, err := h.Handle(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-storeerr"))
	if err != nil {
		t.Fatalf("handler should absorb store failure, got: %v", err)
	}
	// AI events returned unchanged — no VerdictRendered on infra failure.
	if len(got) != 2 {
		t.Fatalf("expected 2 AI events on store error, got %d", len(got))
	}
	for _, e := range got {
		if e.Type == event.VerdictRendered {
			t.Error("unexpected VerdictRendered on store error path")
		}
	}
}

// ---------------------------------------------------------------------------
// Test case 7: git check error (invalid workspace path).
//
// Providing a non-existent directory makes git exit non-zero. The handler
// must absorb the error and return AI events unchanged.
// ---------------------------------------------------------------------------

func TestDeveloperGitCheckError(t *testing.T) {
	store := newMockStore()
	store.correlationEvents["corr-badpath"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
			Path: "/nonexistent/path/that/does/not/exist", Branch: "main", Base: "main",
		})).WithCorrelation("corr-badpath"),
	}

	h := newDeveloperHandlerForTest(t, store)
	got, err := h.Handle(context.Background(), event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-badpath"))
	if err != nil {
		t.Fatalf("handler should absorb git failure, got: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 AI events on git error, got %d", len(got))
	}
	for _, e := range got {
		if e.Type == event.VerdictRendered {
			t.Error("unexpected VerdictRendered on git check error path")
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// eventTypes returns a slice of event type strings for readable test output.
func eventTypes(envs []event.Envelope) []string {
	types := make([]string, len(envs))
	for i, e := range envs {
		types[i] = string(e.Type)
	}
	return types
}
