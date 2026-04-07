package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fakeMCPDashboard returns a handler that responds to known tool calls.
// It captures the last request args for assertion in tests.
func fakeMCPDashboard(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"server":{"name":"rick"}}`)) //nolint:errcheck
			return
		}

		var req mcpRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}

		// Extract tool name and args from params.
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		paramsJSON, err := json.Marshal(req.Params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		if err := json.Unmarshal(paramsJSON, &params); err != nil {
			t.Fatalf("unmarshal params: %v", err)
		}

		var resultText string
		switch params.Name {
		case "rick_list_workflows":
			resultText = `{"workflows":[{"aggregate_id":"wf-1","workflow_id":"workspace-dev","status":"running","started_at":"2026-03-16T10:00:00Z"}]}`
		case "rick_list_events":
			resultText = `{"events":[{"id":"evt-1","type":"workflow.started","version":1,"timestamp":"2026-03-16T10:00:00Z","source":"mcp"}],"count":1}`
		case "rick_workflow_status":
			resultText = `{"id":"wf-1","status":"running","workflow_id":"workspace-dev","version":5,"tokens_used":1000,"completed_personas":{"researcher":true},"feedback_count":{"developer":2}}`
		case "rick_phase_timeline":
			resultText = `{"workflow_id":"wf-1","phases":[{"phase":"researcher","status":"completed","iterations":1,"started_at":"2026-03-16T10:00:00Z","completed_at":"2026-03-16T10:00:30Z","duration_ms":30000}]}`
		case "rick_token_usage":
			resultText = `{"workflow_id":"wf-1","total":1000,"by_phase":{"researcher":500,"architect":500},"by_backend":{"claude":1000}}`
		case "rick_list_dead_letters":
			resultText = `{"dead_letters":[],"count":0}`
		case "rick_pause_workflow":
			resultText = `{"workflow_id":"wf-1","action":"paused","status":"paused"}`
		case "rick_cancel_workflow":
			resultText = `{"workflow_id":"wf-1","action":"cancelled","status":"cancelled"}`
		case "rick_resume_workflow":
			resultText = `{"workflow_id":"wf-1","action":"resumed","status":"running"}`
		case "rick_inject_guidance":
			resultText = `{"workflow_id":"wf-1","action":"guidance_injected","resumed":true}`
		case "rick_workflow_verdicts":
			resultText = `{"workflow_id":"wf-1","verdicts":[{"phase":"developer","source_phase":"reviewer","outcome":"fail","summary":"Missing error handling","issues":[{"severity":"major","category":"correctness","description":"Nil pointer dereference possible","file":"main.go","line":42}]}],"count":1}`
		case "rick_persona_output":
			resultText = `{"workflow_id":"wf-1","persona":"developer","output":"Generated code output...","truncated":false,"backend":"claude","tokens_used":2500,"duration_ms":8000}`
		case "rick_approve_hint":
			resultText = `{"workflow_id":"wf-1","action":"hint_approved","status":"running"}`
		case "rick_reject_hint":
			resultText = `{"workflow_id":"wf-1","action":"hint_rejected","status":"running"}`
		default:
			resultText = `"unknown tool"`
		}

		resp := mcpResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
		}
		result, _ := json.Marshal(mcpToolResult{
			Content: []mcpContent{{Type: "text", Text: resultText}},
		})
		resp.Result = result
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}
}

func newTestApp(t *testing.T) (*App, func()) {
	t.Helper()
	t.Setenv("GOOGLE_API_KEY", "test-key")
	t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")

	srv := newFakeMCP(t, fakeMCPDashboard(t))
	app := NewApp()
	app.mcpClient = NewMCPClient(srv.URL)
	app.ctx = context.Background()
	return app, srv.Close
}

// newBrokenApp creates an app pointing to an unreachable MCP server.
func newBrokenApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("GOOGLE_API_KEY", "test-key")
	t.Setenv("RICK_SERVER_URL", "http://127.0.0.1:1/mcp")

	app := NewApp()
	app.mcpClient = NewMCPClient("http://127.0.0.1:1/mcp")
	app.ctx = context.Background()
	return app
}

// newMalformedApp creates an app backed by a server that returns invalid JSON in tool results.
func newMalformedApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("GOOGLE_API_KEY", "test-key")
	t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")

	srv := newFakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req mcpRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck

		resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}
		// Valid JSON-RPC but the inner text is not valid JSON.
		result, _ := json.Marshal(mcpToolResult{
			Content: []mcpContent{{Type: "text", Text: "this is not valid json {{{"}},
		})
		resp.Result = result
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	})

	app := NewApp()
	app.mcpClient = NewMCPClient(srv.URL)
	app.ctx = context.Background()
	return app
}

func TestListWorkflows(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()

	workflows, err := app.ListWorkflows()
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(workflows) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(workflows))
	}
	wf := workflows[0]
	if wf.AggregateID != "wf-1" {
		t.Errorf("expected aggregate_id wf-1, got %s", wf.AggregateID)
	}
	if wf.WorkflowID != "workspace-dev" {
		t.Errorf("expected workflow_id default, got %s", wf.WorkflowID)
	}
	if wf.Status != "running" {
		t.Errorf("expected running, got %s", wf.Status)
	}
	if wf.StartedAt != "2026-03-16T10:00:00Z" {
		t.Errorf("expected started_at, got %s", wf.StartedAt)
	}
}

func TestListEvents(t *testing.T) {
	t.Run("global", func(t *testing.T) {
		app, cleanup := newTestApp(t)
		defer cleanup()

		events, err := app.ListEvents("", 50)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("expected 1 event, got %d", len(events))
		}
		if events[0].Type != "workflow.started" {
			t.Errorf("expected workflow.started, got %s", events[0].Type)
		}
		if events[0].ID != "evt-1" {
			t.Errorf("expected ID evt-1, got %s", events[0].ID)
		}
	})

	t.Run("with_workflow_filter", func(t *testing.T) {
		app, cleanup := newTestApp(t)
		defer cleanup()

		events, err := app.ListEvents("wf-1", 50)
		if err != nil {
			t.Fatalf("ListEvents with filter: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("expected 1 event, got %d", len(events))
		}
	})

	t.Run("default_limit", func(t *testing.T) {
		app, cleanup := newTestApp(t)
		defer cleanup()

		// Passing 0 should trigger the default limit of 100.
		events, err := app.ListEvents("", 0)
		if err != nil {
			t.Fatalf("ListEvents default limit: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("expected 1 event, got %d", len(events))
		}
	})
}

func TestWorkflowStatus(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()

	detail, err := app.WorkflowStatus("wf-1")
	if err != nil {
		t.Fatalf("WorkflowStatus: %v", err)
	}
	if detail.ID != "wf-1" {
		t.Errorf("expected ID wf-1, got %s", detail.ID)
	}
	if detail.Status != "running" {
		t.Errorf("expected running, got %s", detail.Status)
	}
	if detail.WorkflowID != "workspace-dev" {
		t.Errorf("expected workflow_id default, got %s", detail.WorkflowID)
	}
	if detail.Version != 5 {
		t.Errorf("expected version 5, got %d", detail.Version)
	}
	if detail.TokensUsed != 1000 {
		t.Errorf("expected 1000 tokens, got %d", detail.TokensUsed)
	}
	if !detail.CompletedPersonas["researcher"] {
		t.Error("expected researcher in completed_personas")
	}
	if detail.FeedbackCount["developer"] != 2 {
		t.Errorf("expected developer feedback_count=2, got %d", detail.FeedbackCount["developer"])
	}
}

func TestPhaseTimeline(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()

	phases, err := app.PhaseTimeline("wf-1")
	if err != nil {
		t.Fatalf("PhaseTimeline: %v", err)
	}
	if len(phases) != 1 {
		t.Fatalf("expected 1 phase, got %d", len(phases))
	}
	p := phases[0]
	if p.Phase != "researcher" {
		t.Errorf("expected researcher, got %s", p.Phase)
	}
	if p.Status != "completed" {
		t.Errorf("expected completed, got %s", p.Status)
	}
	if p.Iterations != 1 {
		t.Errorf("expected 1 iteration, got %d", p.Iterations)
	}
	if p.DurationMS != 30000 {
		t.Errorf("expected 30000ms, got %d", p.DurationMS)
	}
	if p.StartedAt != "2026-03-16T10:00:00Z" {
		t.Errorf("expected started_at, got %s", p.StartedAt)
	}
	if p.CompletedAt != "2026-03-16T10:00:30Z" {
		t.Errorf("expected completed_at, got %s", p.CompletedAt)
	}
}

func TestTokenUsage(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()

	usage, err := app.TokenUsageForWorkflow("wf-1")
	if err != nil {
		t.Fatalf("TokenUsage: %v", err)
	}
	if usage.WorkflowID != "wf-1" {
		t.Errorf("expected workflow_id wf-1, got %s", usage.WorkflowID)
	}
	if usage.Total != 1000 {
		t.Errorf("expected 1000, got %d", usage.Total)
	}
	if usage.ByPhase["researcher"] != 500 {
		t.Errorf("expected researcher=500, got %d", usage.ByPhase["researcher"])
	}
	if usage.ByPhase["architect"] != 500 {
		t.Errorf("expected architect=500, got %d", usage.ByPhase["architect"])
	}
	if usage.ByBackend["claude"] != 1000 {
		t.Errorf("expected claude=1000, got %d", usage.ByBackend["claude"])
	}
}

func TestListDeadLetters(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()

	dls, err := app.ListDeadLetters()
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(dls) != 0 {
		t.Errorf("expected 0 dead letters, got %d", len(dls))
	}
}

// TestInterventions uses table-driven subtests for pause/cancel/resume.
func TestInterventions(t *testing.T) {
	tests := []struct {
		name       string
		action     func(app *App) (*ActionResult, error)
		wantAction string
		wantStatus string
	}{
		{
			name:       "pause",
			action:     func(a *App) (*ActionResult, error) { return a.PauseWorkflow("wf-1", "testing") },
			wantAction: "paused",
			wantStatus: "paused",
		},
		{
			name:       "cancel",
			action:     func(a *App) (*ActionResult, error) { return a.CancelWorkflow("wf-1", "testing") },
			wantAction: "cancelled",
			wantStatus: "cancelled",
		},
		{
			name:       "resume",
			action:     func(a *App) (*ActionResult, error) { return a.ResumeWorkflow("wf-1", "testing") },
			wantAction: "resumed",
			wantStatus: "running",
		},
		{
			name:       "pause_empty_reason",
			action:     func(a *App) (*ActionResult, error) { return a.PauseWorkflow("wf-1", "") },
			wantAction: "paused",
			wantStatus: "paused",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, cleanup := newTestApp(t)
			defer cleanup()

			result, err := tt.action(app)
			if err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if result.Action != tt.wantAction {
				t.Errorf("expected action %s, got %s", tt.wantAction, result.Action)
			}
			if result.Status != tt.wantStatus {
				t.Errorf("expected status %s, got %s", tt.wantStatus, result.Status)
			}
			if result.WorkflowID != "wf-1" {
				t.Errorf("expected workflow_id wf-1, got %s", result.WorkflowID)
			}
		})
	}
}

func TestInjectGuidance(t *testing.T) {
	t.Run("with_target", func(t *testing.T) {
		app, cleanup := newTestApp(t)
		defer cleanup()

		result, err := app.InjectGuidance("wf-1", "focus on error handling", "developer")
		if err != nil {
			t.Fatalf("InjectGuidance: %v", err)
		}
		if result.Action != "guidance_injected" {
			t.Errorf("expected guidance_injected, got %s", result.Action)
		}
		if !result.Resumed {
			t.Error("expected resumed=true")
		}
	})

	t.Run("without_target", func(t *testing.T) {
		app, cleanup := newTestApp(t)
		defer cleanup()

		result, err := app.InjectGuidance("wf-1", "focus on error handling", "")
		if err != nil {
			t.Fatalf("InjectGuidance without target: %v", err)
		}
		if result.Action != "guidance_injected" {
			t.Errorf("expected guidance_injected, got %s", result.Action)
		}
	})
}

func TestWorkflowVerdicts(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()

	verdicts, err := app.WorkflowVerdicts("wf-1")
	if err != nil {
		t.Fatalf("WorkflowVerdicts: %v", err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("expected 1 verdict, got %d", len(verdicts))
	}
	v := verdicts[0]
	if v.Phase != "developer" {
		t.Errorf("expected phase developer, got %s", v.Phase)
	}
	if v.SourcePhase != "reviewer" {
		t.Errorf("expected source_phase reviewer, got %s", v.SourcePhase)
	}
	if v.Outcome != "fail" {
		t.Errorf("expected outcome fail, got %s", v.Outcome)
	}
	if len(v.Issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(v.Issues))
	}
	if v.Issues[0].Severity != "major" {
		t.Errorf("expected severity major, got %s", v.Issues[0].Severity)
	}
	if v.Issues[0].File != "main.go" {
		t.Errorf("expected file main.go, got %s", v.Issues[0].File)
	}
	if v.Issues[0].Line != 42 {
		t.Errorf("expected line 42, got %d", v.Issues[0].Line)
	}
}

func TestPersonaOutput(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()

	output, err := app.PersonaOutput("wf-1", "developer")
	if err != nil {
		t.Fatalf("PersonaOutput: %v", err)
	}
	if output.Persona != "developer" {
		t.Errorf("expected persona developer, got %s", output.Persona)
	}
	if output.Output != "Generated code output..." {
		t.Errorf("expected output text, got %s", output.Output)
	}
	if output.Backend != "claude" {
		t.Errorf("expected backend claude, got %s", output.Backend)
	}
	if output.TokensUsed != 2500 {
		t.Errorf("expected 2500 tokens, got %d", output.TokensUsed)
	}
	if output.Truncated {
		t.Error("expected truncated=false")
	}
}

// --- ApproveHint / RejectHint Tests ---

// fakeMCPWithArgCapture returns a fake MCP handler that, in addition to routing tool
// calls to the dashboard responses, stores the parsed arguments of the last call.
func fakeMCPWithArgCapture(t *testing.T) (http.HandlerFunc, func() map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var lastArgs map[string]any

	base := fakeMCPDashboard(t)
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// Read body for arg capture without consuming it: we peek at params.
			// We clone the request body parsing inline.
			var req mcpRequest
			body, _ := json.Marshal(r.Body) // won't work — need to buffer
			_ = body

			// Re-parse from the request after base handles it via a proxy approach:
			// Instead, we intercept the body via a buffer and delegate.
			buf := new(strings.Builder)
			decoder := json.NewDecoder(r.Body)
			if err := decoder.Decode(&req); err == nil {
				// Extract args from params.
				var params struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				}
				if raw, err := json.Marshal(req.Params); err == nil {
					if err := json.Unmarshal(raw, &params); err == nil {
						var args map[string]any
						if err := json.Unmarshal(params.Arguments, &args); err == nil {
							mu.Lock()
							lastArgs = args
							mu.Unlock()
						}
					}
				}

				// Re-encode and replay through the base handler logic.
				// Since body is consumed, we need to reconstruct the response ourselves.
				resultText := dashboardResultFor(params.Name)
				resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}
				result, _ := json.Marshal(mcpToolResult{
					Content: []mcpContent{{Type: "text", Text: resultText}},
				})
				resp.Result = result
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp) //nolint:errcheck
				return
			}
			_ = buf
		}
		base(w, r)
	}
	return handler, func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return lastArgs
	}
}

// dashboardResultFor returns the canned JSON response for a given tool name.
// Must stay in sync with fakeMCPDashboard's switch.
func dashboardResultFor(name string) string {
	switch name {
	case "rick_list_workflows":
		return `{"workflows":[{"aggregate_id":"wf-1","workflow_id":"workspace-dev","status":"running","started_at":"2026-03-16T10:00:00Z"}]}`
	case "rick_approve_hint":
		return `{"workflow_id":"wf-1","action":"hint_approved","status":"running"}`
	case "rick_reject_hint":
		return `{"workflow_id":"wf-1","action":"hint_rejected","status":"running"}`
	default:
		return `"unknown tool"`
	}
}

func TestApproveHint(t *testing.T) {
	t.Run("with_guidance", func(t *testing.T) {
		handler, getLastArgs := fakeMCPWithArgCapture(t)
		t.Setenv("GOOGLE_API_KEY", "test-key")
		t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")
		srv := newFakeMCP(t, handler)
		app := NewApp()
		app.mcpClient = NewMCPClient(srv.URL)
		app.ctx = context.Background()

		result, err := app.ApproveHint("wf-1", "developer", "focus on error handling")
		if err != nil {
			t.Fatalf("ApproveHint: %v", err)
		}
		if result.Action != "hint_approved" {
			t.Errorf("expected action hint_approved, got %s", result.Action)
		}
		if result.WorkflowID != "wf-1" {
			t.Errorf("expected workflow_id wf-1, got %s", result.WorkflowID)
		}

		args := getLastArgs()
		if args == nil {
			t.Fatal("expected args to be captured")
		}
		if _, ok := args["guidance"]; !ok {
			t.Error("expected 'guidance' in args when guidance is provided")
		}
		if args["guidance"] != "focus on error handling" {
			t.Errorf("expected guidance 'focus on error handling', got %v", args["guidance"])
		}
		if args["workflow_id"] != "wf-1" {
			t.Errorf("expected workflow_id wf-1, got %v", args["workflow_id"])
		}
		if args["persona"] != "developer" {
			t.Errorf("expected persona developer, got %v", args["persona"])
		}
	})

	t.Run("without_guidance", func(t *testing.T) {
		handler, getLastArgs := fakeMCPWithArgCapture(t)
		t.Setenv("GOOGLE_API_KEY", "test-key")
		t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")
		srv := newFakeMCP(t, handler)
		app := NewApp()
		app.mcpClient = NewMCPClient(srv.URL)
		app.ctx = context.Background()

		result, err := app.ApproveHint("wf-1", "architect", "")
		if err != nil {
			t.Fatalf("ApproveHint without guidance: %v", err)
		}
		if result.Action != "hint_approved" {
			t.Errorf("expected action hint_approved, got %s", result.Action)
		}

		args := getLastArgs()
		if _, ok := args["guidance"]; ok {
			t.Error("expected 'guidance' NOT in args when guidance is empty")
		}
	})

	t.Run("mcp_error", func(t *testing.T) {
		app := newBrokenApp(t)
		_, err := app.ApproveHint("wf-1", "developer", "")
		if err == nil {
			t.Fatal("expected error when MCP server is down")
		}
		if !strings.Contains(err.Error(), "approve hint") {
			t.Errorf("expected error to contain 'approve hint', got: %s", err.Error())
		}
	})
}

func TestRejectHint(t *testing.T) {
	t.Run("action_skip", func(t *testing.T) {
		handler, getLastArgs := fakeMCPWithArgCapture(t)
		t.Setenv("GOOGLE_API_KEY", "test-key")
		t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")
		srv := newFakeMCP(t, handler)
		app := NewApp()
		app.mcpClient = NewMCPClient(srv.URL)
		app.ctx = context.Background()

		result, err := app.RejectHint("wf-1", "developer", "not needed", "skip")
		if err != nil {
			t.Fatalf("RejectHint skip: %v", err)
		}
		if result.Action != "hint_rejected" {
			t.Errorf("expected action hint_rejected, got %s", result.Action)
		}

		args := getLastArgs()
		if args["action"] != "skip" {
			t.Errorf("expected action 'skip', got %v", args["action"])
		}
		if args["reason"] != "not needed" {
			t.Errorf("expected reason 'not needed', got %v", args["reason"])
		}
	})

	t.Run("action_fail", func(t *testing.T) {
		handler, getLastArgs := fakeMCPWithArgCapture(t)
		t.Setenv("GOOGLE_API_KEY", "test-key")
		t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")
		srv := newFakeMCP(t, handler)
		app := NewApp()
		app.mcpClient = NewMCPClient(srv.URL)
		app.ctx = context.Background()

		_, err := app.RejectHint("wf-1", "developer", "bad plan", "fail")
		if err != nil {
			t.Fatalf("RejectHint fail: %v", err)
		}
		args := getLastArgs()
		if args["action"] != "fail" {
			t.Errorf("expected action 'fail', got %v", args["action"])
		}
	})

	t.Run("empty_action", func(t *testing.T) {
		handler, getLastArgs := fakeMCPWithArgCapture(t)
		t.Setenv("GOOGLE_API_KEY", "test-key")
		t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")
		srv := newFakeMCP(t, handler)
		app := NewApp()
		app.mcpClient = NewMCPClient(srv.URL)
		app.ctx = context.Background()

		_, err := app.RejectHint("wf-1", "developer", "", "")
		if err != nil {
			t.Fatalf("RejectHint empty action: %v", err)
		}
		args := getLastArgs()
		if _, ok := args["action"]; ok {
			t.Error("expected 'action' NOT in args when action is empty")
		}
	})

	t.Run("with_reason_no_action", func(t *testing.T) {
		handler, getLastArgs := fakeMCPWithArgCapture(t)
		t.Setenv("GOOGLE_API_KEY", "test-key")
		t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")
		srv := newFakeMCP(t, handler)
		app := NewApp()
		app.mcpClient = NewMCPClient(srv.URL)
		app.ctx = context.Background()

		_, err := app.RejectHint("wf-1", "developer", "low confidence", "")
		if err != nil {
			t.Fatalf("RejectHint with reason only: %v", err)
		}
		args := getLastArgs()
		if args["reason"] != "low confidence" {
			t.Errorf("expected reason 'low confidence', got %v", args["reason"])
		}
		if _, ok := args["action"]; ok {
			t.Error("expected 'action' NOT in args when action is empty")
		}
	})

	t.Run("mcp_error", func(t *testing.T) {
		app := newBrokenApp(t)
		_, err := app.RejectHint("wf-1", "developer", "", "skip")
		if err == nil {
			t.Fatal("expected error when MCP server is down")
		}
		if !strings.Contains(err.Error(), "reject hint") {
			t.Errorf("expected error to contain 'reject hint', got: %s", err.Error())
		}
	})
}

// --- Error Propagation Tests ---

func TestDashboard_MCPServerDown(t *testing.T) {
	app := newBrokenApp(t)

	tests := []struct {
		name string
		call func() error
	}{
		{"ListWorkflows", func() error { _, err := app.ListWorkflows(); return err }},
		{"ListEvents", func() error { _, err := app.ListEvents("", 50); return err }},
		{"WorkflowStatus", func() error { _, err := app.WorkflowStatus("wf-1"); return err }},
		{"PhaseTimeline", func() error { _, err := app.PhaseTimeline("wf-1"); return err }},
		{"TokenUsage", func() error { _, err := app.TokenUsageForWorkflow("wf-1"); return err }},
		{"ListDeadLetters", func() error { _, err := app.ListDeadLetters(); return err }},
		{"PauseWorkflow", func() error { _, err := app.PauseWorkflow("wf-1", "test"); return err }},
		{"CancelWorkflow", func() error { _, err := app.CancelWorkflow("wf-1", "test"); return err }},
		{"ResumeWorkflow", func() error { _, err := app.ResumeWorkflow("wf-1", "test"); return err }},
		{"InjectGuidance", func() error { _, err := app.InjectGuidance("wf-1", "test", ""); return err }},
		{"WorkflowVerdicts", func() error { _, err := app.WorkflowVerdicts("wf-1"); return err }},
		{"PersonaOutput", func() error { _, err := app.PersonaOutput("wf-1", "dev"); return err }},
		{"ApproveHint", func() error { _, err := app.ApproveHint("wf-1", "developer", ""); return err }},
		{"RejectHint", func() error { _, err := app.RejectHint("wf-1", "developer", "", "skip"); return err }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if err == nil {
				t.Fatalf("%s: expected error when server is down", tt.name)
			}
		})
	}
}

func TestDashboard_MalformedJSON(t *testing.T) {
	app := newMalformedApp(t)

	tests := []struct {
		name    string
		call    func() error
		wantErr string
	}{
		{"ListWorkflows", func() error { _, err := app.ListWorkflows(); return err }, "unmarshal"},
		{"ListEvents", func() error { _, err := app.ListEvents("wf-1", 50); return err }, "unmarshal"},
		{"ListDeadLetters", func() error { _, err := app.ListDeadLetters(); return err }, "unmarshal"},
		{"WorkflowStatus", func() error { _, err := app.WorkflowStatus("wf-1"); return err }, "unmarshal"},
		{"PhaseTimeline", func() error { _, err := app.PhaseTimeline("wf-1"); return err }, "unmarshal"},
		{"TokenUsage", func() error { _, err := app.TokenUsageForWorkflow("wf-1"); return err }, "unmarshal"},
		{"WorkflowVerdicts", func() error { _, err := app.WorkflowVerdicts("wf-1"); return err }, "unmarshal"},
		{"PersonaOutput", func() error { _, err := app.PersonaOutput("wf-1", "dev"); return err }, "unmarshal"},
		{"PauseWorkflow", func() error { _, err := app.PauseWorkflow("wf-1", "test"); return err }, "unmarshal"},
		{"CancelWorkflow", func() error { _, err := app.CancelWorkflow("wf-1", "test"); return err }, "unmarshal"},
		{"ResumeWorkflow", func() error { _, err := app.ResumeWorkflow("wf-1", "test"); return err }, "unmarshal"},
		{"InjectGuidance", func() error { _, err := app.InjectGuidance("wf-1", "msg", ""); return err }, "unmarshal"},
		{"ApproveHint", func() error { _, err := app.ApproveHint("wf-1", "developer", ""); return err }, "unmarshal"},
		{"RejectHint", func() error { _, err := app.RejectHint("wf-1", "developer", "", "skip"); return err }, "unmarshal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if err == nil {
				t.Fatalf("%s: expected unmarshal error", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s: expected error containing %q, got: %s", tt.name, tt.wantErr, err.Error())
			}
		})
	}
}
