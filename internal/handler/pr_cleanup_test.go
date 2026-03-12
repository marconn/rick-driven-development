package handler

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// ---------------------------------------------------------------------------
// PRCleanupHandler construction
// ---------------------------------------------------------------------------

func TestNewPRCleanup(t *testing.T) {
	h := NewPRCleanup(testDeps())
	if h.Name() != "pr-cleanup" {
		t.Errorf("want name 'pr-cleanup', got %q", h.Name())
	}

	// DAG-based dispatch — Subscribes returns nil.
	subs := h.Subscribes()
	if subs != nil {
		t.Errorf("want nil Subscribes (DAG-based dispatch), got %v", subs)
	}
}

// ---------------------------------------------------------------------------
// PRCleanupHandler.Handle — removes isolated workspace
// ---------------------------------------------------------------------------

func TestPRCleanupHandlerRemovesIsolatedWorkspace(t *testing.T) {
	tmp := t.TempDir()
	isolatedDir := filepath.Join(tmp, "isolated-workspace")
	if err := os.MkdirAll(isolatedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Place a sentinel file to confirm removal.
	sentinelFile := filepath.Join(isolatedDir, "sentinel.txt")
	if err := os.WriteFile(sentinelFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	store := newMockStore()
	corrID := "corr-cleanup-1"

	wsReadyEvt := event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
		Path:     isolatedDir,
		Branch:   "feature/test",
		Base:     "main",
		Isolated: true,
	})).WithAggregate("wf-1", 1).WithCorrelation(corrID)
	store.correlationEvents[corrID] = append(store.correlationEvents[corrID], wsReadyEvt)

	completedEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "pr-consolidator",
	})).WithAggregate("wf-1", 2).WithCorrelation(corrID)

	h := &PRCleanupHandler{store: store}
	evts, err := h.Handle(context.Background(), completedEvt)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if evts != nil {
		t.Errorf("cleanup handler should return nil events, got %v", evts)
	}

	// Workspace directory should be removed.
	if _, statErr := os.Stat(isolatedDir); !os.IsNotExist(statErr) {
		t.Error("expected isolated workspace to be removed")
	}
}

// ---------------------------------------------------------------------------
// PRCleanupHandler.Handle — non-isolated workspace is a no-op
// ---------------------------------------------------------------------------

func TestPRCleanupHandlerNoOpForNonIsolated(t *testing.T) {
	tmp := t.TempDir()
	sharedDir := filepath.Join(tmp, "shared-repo")
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	store := newMockStore()
	corrID := "corr-cleanup-2"

	wsReadyEvt := event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
		Path:     sharedDir,
		Branch:   "feature/shared",
		Base:     "main",
		Isolated: false, // NOT isolated
	})).WithAggregate("wf-2", 1).WithCorrelation(corrID)
	store.correlationEvents[corrID] = append(store.correlationEvents[corrID], wsReadyEvt)

	completedEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "pr-consolidator",
	})).WithAggregate("wf-2", 2).WithCorrelation(corrID)

	h := &PRCleanupHandler{store: store}
	_, err := h.Handle(context.Background(), completedEvt)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Shared directory should still exist.
	if _, statErr := os.Stat(sharedDir); statErr != nil {
		t.Error("shared workspace should NOT be removed by cleanup handler")
	}
}

// ---------------------------------------------------------------------------
// PRCleanupHandler.Handle — no WorkspaceReady event (no-op)
// ---------------------------------------------------------------------------

func TestPRCleanupHandlerNoWorkspaceReady(t *testing.T) {
	store := newMockStore()
	corrID := "corr-cleanup-3"
	// No WorkspaceReady event in the correlation chain.
	store.correlationEvents[corrID] = []event.Envelope{}

	completedEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "pr-consolidator",
	})).WithAggregate("wf-3", 1).WithCorrelation(corrID)

	h := &PRCleanupHandler{store: store}
	evts, err := h.Handle(context.Background(), completedEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evts != nil {
		t.Errorf("expected nil events, got %v", evts)
	}
}

// ---------------------------------------------------------------------------
// PRCleanupHandler.Handle — empty correlation
// ---------------------------------------------------------------------------

func TestPRCleanupHandlerEmptyCorrelation(t *testing.T) {
	h := NewPRCleanup(testDeps())
	env := event.New(event.PersonaCompleted, 1, nil)
	// CorrelationID defaults to "".
	evts, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evts != nil {
		t.Errorf("expected nil events for empty correlation, got %v", evts)
	}
}
