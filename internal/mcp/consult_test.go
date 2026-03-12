package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/projection"
)

// testDepsWithBackend builds a Deps with a stub Backend injected.
func testDepsWithBackend(t *testing.T, be backend.Backend) (Deps, func()) {
	t.Helper()
	dbPath := t.TempDir() + "/test.db"
	store, err := eventstore.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	bus := eventbus.NewChannelBus(eventbus.WithLogger(testLogger()))
	eng := engine.NewEngine(store, bus, testLogger())
	eng.RegisterWorkflow(engine.WorkspaceDevWorkflowDef())

	workflows := projection.NewWorkflowStatusProjection()
	tokens := projection.NewTokenUsageProjection()
	timelines := projection.NewPhaseTimelineProjection()
	verdicts := projection.NewVerdictProjection()

	deps := Deps{
		Store:     store,
		Bus:       bus,
		Engine:    eng,
		Workflows: workflows,
		Tokens:    tokens,
		Timelines: timelines,
		Verdicts:  verdicts,
		Backend:   be,
		SelectWorkflow: func(name string) (engine.WorkflowDef, error) {
			return engine.WorkspaceDevWorkflowDef(), nil
		},
	}

	return deps, func() {
		_ = bus.Close()
		_ = store.Close()
	}
}

// --- toolConsult tests ---

func TestToolConsult_MissingPrompt(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_consult", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing prompt")
	}
	if !strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolConsult_WithBackend(t *testing.T) {
	be := &stubBackend{name: "test", resp: backend.Response{Output: "architect advice"}}
	deps, cleanup := testDepsWithBackend(t, be)
	defer cleanup()

	s := NewServer(deps, testLogger())
	defer s.Close()

	raw, _ := json.Marshal(map[string]any{
		"prompt": "How should I design this service?",
		"mode":   "architect",
	})
	tool := s.tools["rick_consult"]
	result, err := tool.Handler(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cr, ok := result.(consultResult)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if cr.Status != "running" {
		t.Errorf("expected status running, got %q", cr.Status)
	}
	if cr.JobID == "" {
		t.Error("expected non-empty job_id")
	}
	if cr.Mode != "architect" {
		t.Errorf("expected mode 'architect', got %q", cr.Mode)
	}

	// Verify the job exists in the manager.
	_, err = s.jobs.Get(cr.JobID)
	if err != nil {
		t.Fatalf("job not found: %v", err)
	}
}

func TestToolRun_WithBackend(t *testing.T) {
	be := &stubBackend{name: "test", resp: backend.Response{Output: "code written"}}
	deps, cleanup := testDepsWithBackend(t, be)
	defer cleanup()

	s := NewServer(deps, testLogger())
	defer s.Close()

	raw, _ := json.Marshal(map[string]any{
		"prompt": "Implement a REST endpoint",
		"mode":   "developer",
	})
	tool := s.tools["rick_run"]
	result, err := tool.Handler(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cr, ok := result.(consultResult)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if cr.Status != "running" {
		t.Errorf("expected status running, got %q", cr.Status)
	}
	if cr.JobID == "" {
		t.Error("expected non-empty job_id")
	}
}

func TestToolRun_MissingPrompt(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_run", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing prompt")
	}
	if !strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolConsult_InvalidMode(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	// Unknown mode — no backend configured, but error should be about persona.
	_, err := callTool(t, s, "rick_consult", map[string]any{
		"prompt": "test",
		"mode":   "nonexistent-mode",
	})
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestToolConsult_WithBackendAndContextFiles(t *testing.T) {
	be := &stubBackend{name: "test", latency: 100 * time.Millisecond, resp: backend.Response{Output: "done"}}
	deps, cleanup := testDepsWithBackend(t, be)
	defer cleanup()

	s := NewServer(deps, testLogger())
	defer s.Close()

	raw, _ := json.Marshal(map[string]any{
		"prompt":        "Review this code",
		"mode":          "reviewer",
		"context_files": []string{"/path/to/file.go"},
	})
	tool := s.tools["rick_consult"]
	result, err := tool.Handler(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cr := result.(consultResult)
	if cr.JobID == "" {
		t.Error("expected job_id")
	}
}
