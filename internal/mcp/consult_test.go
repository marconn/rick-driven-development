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

// launchAndCaptureReq runs a job tool, waits for the stub backend to receive
// the request, and returns it. Fails the test if the job never ran.
func launchAndCaptureReq(t *testing.T, toolName string, args map[string]any) *backend.Request {
	t.Helper()
	be := &stubBackend{name: "test", resp: backend.Response{Output: "ok"}}
	deps, cleanup := testDepsWithBackend(t, be)
	defer cleanup()

	s := NewServer(deps, testLogger())
	defer s.Close()

	raw, _ := json.Marshal(args)
	result, err := s.tools[toolName].Handler(context.Background(), raw)
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", toolName, err)
	}
	cr := result.(consultResult)
	waitForStatus(t, s.jobs, cr.JobID, time.Second)

	req := be.lastRequest()
	if req == nil {
		t.Fatalf("%s: backend Run was never invoked", toolName)
	}
	return req
}

// Regression: a consult with no explicit yolo must default to yolo=true.
// Previously it inherited the server-wide Deps.Yolo (false under `rick mcp`),
// which left headless personas unable to read files — useless for analysis.
func TestToolConsult_YoloDefaultsTrue(t *testing.T) {
	req := launchAndCaptureReq(t, "rick_consult", map[string]any{
		"prompt": "Review the design",
		"mode":   "architect",
	})
	if !req.Yolo {
		t.Error("expected Yolo=true when caller omits yolo, got false")
	}
}

// Regression: an explicit yolo:false must still force read-only mode.
func TestToolConsult_ExplicitYoloFalseRespected(t *testing.T) {
	req := launchAndCaptureReq(t, "rick_consult", map[string]any{
		"prompt": "Review the design",
		"mode":   "reviewer",
		"yolo":   false,
	})
	if req.Yolo {
		t.Error("expected Yolo=false when caller passes yolo:false, got true")
	}
}

// Regression: rick_run shares the same default-on resolution as rick_consult.
func TestToolRun_YoloDefaultsTrue(t *testing.T) {
	req := launchAndCaptureReq(t, "rick_run", map[string]any{
		"prompt": "Implement the endpoint",
		"mode":   "developer",
	})
	if !req.Yolo {
		t.Error("expected Yolo=true when caller omits yolo, got false")
	}
}

// Regression: a consult that names a backend must resolve to THAT backend,
// not the server default. This is the consult-mode entry point for antigravity
// — the broken model handling in the antigravity driver only ever surfaced
// through this path (rick_consult backend=antigravity), so the selection
// wiring needs explicit coverage. We assert resolveBackend directly rather
// than launching the job: a named backend bypasses the injected stub and would
// otherwise exec the real `agy` subprocess.
func TestToolConsult_BackendSelectionAntigravity(t *testing.T) {
	be := &stubBackend{name: "default", resp: backend.Response{Output: "x"}}
	deps, cleanup := testDepsWithBackend(t, be)
	defer cleanup()

	s := NewServer(deps, testLogger())
	defer s.Close()

	resolved, err := s.resolveBackend("antigravity")
	if err != nil {
		t.Fatalf("resolveBackend(antigravity): %v", err)
	}
	if resolved.Name() != "antigravity" {
		t.Errorf("resolveBackend(antigravity).Name() = %q; want antigravity (named backend must win over server default)", resolved.Name())
	}

	// And an empty backend name must fall back to the injected server default.
	def, err := s.resolveBackend("")
	if err != nil {
		t.Fatalf("resolveBackend(\"\"): %v", err)
	}
	if def.Name() != "default" {
		t.Errorf("resolveBackend(\"\").Name() = %q; want the server-default stub", def.Name())
	}
}

// Regression: the model arg must reach backend.Request.Model. This is the
// field the antigravity driver mishandled (forwarding it as a nonexistent
// `-m` flag), and it is also how RICK_MODEL / per-call models reach every
// backend, so the consult→backend wiring is pinned here.
func TestToolConsult_ModelForwardedToRequest(t *testing.T) {
	req := launchAndCaptureReq(t, "rick_consult", map[string]any{
		"prompt": "Review the design",
		"mode":   "architect",
		"model":  "gemini-2.5-pro",
	})
	if req.Model != "gemini-2.5-pro" {
		t.Errorf("Request.Model = %q; want the consult model arg forwarded", req.Model)
	}
}

func TestToolRun_ModelForwardedToRequest(t *testing.T) {
	req := launchAndCaptureReq(t, "rick_run", map[string]any{
		"prompt": "Implement the endpoint",
		"mode":   "developer",
		"model":  "gemini-2.5-pro",
	})
	if req.Model != "gemini-2.5-pro" {
		t.Errorf("Request.Model = %q; want the run model arg forwarded", req.Model)
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
