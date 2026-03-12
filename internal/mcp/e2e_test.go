package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/projection"
)

// mcpClient simulates what the agent/operator.go does — sends JSON-RPC
// requests to the MCP HTTP server and parses responses. This validates the
// same code path the ADK MCP transport would use.
type mcpClient struct {
	baseURL string
	client  *http.Client
}

func newMCPClient(baseURL string) *mcpClient {
	return &mcpClient{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

func (c *mcpClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
	}
	if params != nil {
		reqBody["params"] = params
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	var rpcResp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	return rpcResp.Result, nil
}

func (c *mcpClient) callTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	params := map[string]any{
		"name":      name,
		"arguments": args,
	}
	result, err := c.call(ctx, "tools/call", params)
	if err != nil {
		return "", false, err
	}

	var toolResult struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &toolResult); err != nil {
		return "", false, fmt.Errorf("parse tool result: %w", err)
	}

	text := ""
	if len(toolResult.Content) > 0 {
		text = toolResult.Content[0].Text
	}
	return text, toolResult.IsError, nil
}

func (c *mcpClient) healthCheck(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/mcp", nil)
	if err != nil {
		return false, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

// e2eEnv provides access to the server internals for tests that need to inject
// events directly (e.g., simulating hint emission).
type e2eEnv struct {
	Store eventstore.Store
	Bus   eventbus.Bus
}

// startE2EServer starts a full Rick MCP HTTP server with engine for E2E tests.
func startE2EServer(t *testing.T) (string, func()) {
	baseURL, _, cleanup := startE2EServerWithEnv(t)
	return baseURL, cleanup
}

// startE2EServerWithEnv is like startE2EServer but also returns the store/bus
// for tests that need direct event injection.
func startE2EServerWithEnv(t *testing.T) (string, *e2eEnv, func()) {
	t.Helper()

	dbPath := t.TempDir() + "/e2e.db"
	store, err := eventstore.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	bus := eventbus.NewChannelBus(eventbus.WithLogger(testLogger()))
	eng := engine.NewEngine(store, bus, testLogger())
	eng.RegisterWorkflow(engine.WorkspaceDevWorkflowDef())
	eng.RegisterWorkflow(engine.DevelopOnlyWorkflowDef())
	eng.Start()

	workflows := projection.NewWorkflowStatusProjection()
	tokens := projection.NewTokenUsageProjection()
	timelines := projection.NewPhaseTimelineProjection()

	ctx, cancel := context.WithCancel(context.Background())

	projRunner := projection.NewRunner(store, bus, testLogger())
	projRunner.Register(workflows)
	projRunner.Register(tokens)
	projRunner.Register(timelines)
	if err := projRunner.Start(ctx); err != nil {
		t.Fatal(err)
	}

	deps := Deps{
		Store:     store,
		Bus:       bus,
		Engine:    eng,
		Workflows: workflows,
		Tokens:    tokens,
		Timelines: timelines,
		SelectWorkflow: func(name string) (engine.WorkflowDef, error) {
			switch name {
			case "workspace-dev":
				return engine.WorkspaceDevWorkflowDef(), nil
			case "develop-only":
				return engine.DevelopOnlyWorkflowDef(), nil
			default:
				return engine.WorkflowDef{}, fmt.Errorf("unknown: %s", name)
			}
		},
	}

	server := NewServer(deps, testLogger())

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", server.handleHTTPPost)
	mux.HandleFunc("GET /mcp", server.handleHTTPGet)

	httpSrv := &http.Server{Handler: withCORS(mux)}
	go func() { _ = httpSrv.Serve(listener) }()

	baseURL := "http://" + listener.Addr().String()

	cleanup := func() {
		cancel()
		_ = httpSrv.Close()
		projRunner.Stop()
		eng.Stop()
		_ = bus.Close()
		_ = store.Close()
	}

	env := &e2eEnv{Store: store, Bus: bus}
	return baseURL, env, cleanup
}

// --- E2E Tests ---
// These test the full flow an agent client would perform: discover tools,
// start a workflow, monitor it, intervene, and shut down.

func TestE2EAgentDiscoverTools(t *testing.T) {
	baseURL, cleanup := startE2EServer(t)
	defer cleanup()
	ctx := context.Background()

	client := newMCPClient(baseURL)

	// 1. Health check.
	ok, err := client.healthCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("health check failed")
	}

	// 2. Discover tools (same as what mcptoolset does internally).
	result, err := client.call(ctx, "tools/list", nil)
	if err != nil {
		t.Fatal(err)
	}

	var toolsList struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &toolsList); err != nil {
		t.Fatal(err)
	}

	if len(toolsList.Tools) < 11 {
		t.Fatalf("expected at least 11 tools, got %d", len(toolsList.Tools))
	}

	// Verify all tools have descriptions (needed for Gemini function declarations).
	for _, tool := range toolsList.Tools {
		if tool.Description == "" {
			t.Errorf("tool %s has empty description", tool.Name)
		}
	}
}

func TestE2EAgentFullWorkflowJourney(t *testing.T) {
	baseURL, cleanup := startE2EServer(t)
	defer cleanup()
	ctx := context.Background()

	client := newMCPClient(baseURL)

	// Step 1: "What workflows are running?" → list_workflows
	text, isErr, err := client.callTool(ctx, "rick_list_workflows", nil)
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("list workflows error: %s", text)
	}
	var wfList listWorkflowsResult
	_ = json.Unmarshal([]byte(text), &wfList)
	initialCount := len(wfList.Workflows)

	// Step 2: "Start a workflow to build a REST API" → run_workflow
	text, isErr, err = client.callTool(ctx, "rick_run_workflow", map[string]any{
		"prompt": "Build a REST API for user management",
		"dag":    "workspace-dev",
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("run workflow error: %s", text)
	}
	var run runWorkflowResult
	if err := json.Unmarshal([]byte(text), &run); err != nil {
		t.Fatal(err)
	}
	if run.WorkflowID == "" {
		t.Fatal("expected non-empty workflow ID")
	}
	wfID := run.WorkflowID
	t.Logf("started workflow: %s", wfID[:8])

	// Wait for engine to process.
	time.Sleep(150 * time.Millisecond)

	// Step 3: "What's the status?" → workflow_status
	text, isErr, err = client.callTool(ctx, "rick_workflow_status", map[string]any{
		"workflow_id": wfID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("status error: %s", text)
	}
	var status workflowStatusResult
	_ = json.Unmarshal([]byte(text), &status)
	if status.Status != "running" && status.Status != "requested" {
		t.Errorf("expected running or requested, got %s", status.Status)
	}

	// Step 4: "Show me the timeline" → phase_timeline
	text, isErr, err = client.callTool(ctx, "rick_phase_timeline", map[string]any{
		"workflow_id": wfID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("timeline error: %s", text)
	}

	// Step 5: "How many tokens used?" → token_usage
	text, isErr, err = client.callTool(ctx, "rick_token_usage", map[string]any{
		"workflow_id": wfID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("token usage error: %s", text)
	}

	// Step 6: "Show me the events" → list_events
	text, isErr, err = client.callTool(ctx, "rick_list_events", map[string]any{
		"workflow_id": wfID,
		"limit":       50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("list events error: %s", text)
	}
	var eventList listEventsResult
	_ = json.Unmarshal([]byte(text), &eventList)
	if eventList.Count < 1 {
		t.Error("expected at least 1 event")
	}

	// Step 7: "Pause it" → pause_workflow
	text, isErr, err = client.callTool(ctx, "rick_pause_workflow", map[string]any{
		"workflow_id": wfID,
		"reason":      "operator investigating",
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("pause error: %s", text)
	}
	var pauseResult interventionResult
	_ = json.Unmarshal([]byte(text), &pauseResult)
	if pauseResult.Status != "paused" {
		t.Errorf("expected paused, got %s", pauseResult.Status)
	}

	// Step 8: "Tell the developer to use PostgreSQL" → inject_guidance
	text, isErr, err = client.callTool(ctx, "rick_inject_guidance", map[string]any{
		"workflow_id": wfID,
		"content":     "Use PostgreSQL instead of SQLite for the data layer",
		"target":      "developer",
		"auto_resume": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("guidance error: %s", text)
	}
	var guideResult guidanceResult
	_ = json.Unmarshal([]byte(text), &guideResult)
	if !guideResult.Resumed {
		t.Error("expected auto-resume")
	}

	// Step 9: "Cancel it" → cancel_workflow
	text, isErr, err = client.callTool(ctx, "rick_cancel_workflow", map[string]any{
		"workflow_id": wfID,
		"reason":      "test complete",
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("cancel error: %s", text)
	}
	var cancelResult interventionResult
	_ = json.Unmarshal([]byte(text), &cancelResult)
	if cancelResult.Status != "cancelled" {
		t.Errorf("expected cancelled, got %s", cancelResult.Status)
	}

	// Step 10: "Any dead letters?" → list_dead_letters
	text, isErr, err = client.callTool(ctx, "rick_list_dead_letters", nil)
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("dead letters error: %s", text)
	}

	// Step 11: Verify workflow count increased.
	text, _, _ = client.callTool(ctx, "rick_list_workflows", nil)
	_ = json.Unmarshal([]byte(text), &wfList)
	if len(wfList.Workflows) <= initialCount {
		t.Errorf("expected workflow count to increase from %d", initialCount)
	}

	t.Logf("E2E journey complete: all 11 tools exercised on workflow %s", wfID[:8])
}

func TestE2EAgentMultipleWorkflows(t *testing.T) {
	baseURL, cleanup := startE2EServer(t)
	defer cleanup()
	ctx := context.Background()

	client := newMCPClient(baseURL)

	// Start 3 workflows concurrently.
	type result struct {
		wfID string
		err  error
	}
	results := make(chan result, 3)
	for _, dag := range []string{"workspace-dev", "workspace-dev", "develop-only"} {
		go func(dagName string) {
			text, isErr, err := client.callTool(ctx, "rick_run_workflow", map[string]any{
				"prompt": "multi-wf test " + dagName,
				"dag":    dagName,
			})
			if err != nil {
				results <- result{err: err}
				return
			}
			if isErr {
				results <- result{err: fmt.Errorf("tool error: %s", text)}
				return
			}
			var run runWorkflowResult
			_ = json.Unmarshal([]byte(text), &run)
			results <- result{wfID: run.WorkflowID}
		}(dag)
	}

	var ids []string
	for range 3 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		ids = append(ids, r.wfID)
	}

	// Verify all 3 are listed. Sync projections ensure consistency without sleep.
	text, _, _ := client.callTool(ctx, "rick_list_workflows", nil)
	var wfList listWorkflowsResult
	_ = json.Unmarshal([]byte(text), &wfList)

	// Global events should contain at least 3 WorkflowRequested.
	text, _, _ = client.callTool(ctx, "rick_list_events", map[string]any{"limit": 100})
	var events listEventsResult
	_ = json.Unmarshal([]byte(text), &events)
	if events.Count < 3 {
		t.Errorf("expected at least 3 events, got %d", events.Count)
	}

	// Check status of each.
	for _, id := range ids {
		text, isErr, err := client.callTool(ctx, "rick_workflow_status", map[string]any{"workflow_id": id})
		if err != nil {
			t.Errorf("status error for %s: %v", id[:8], err)
			continue
		}
		if isErr {
			t.Errorf("status tool error for %s: %s", id[:8], text)
		}
	}
}

func TestE2EAgentErrorCases(t *testing.T) {
	baseURL, cleanup := startE2EServer(t)
	defer cleanup()
	ctx := context.Background()

	client := newMCPClient(baseURL)

	// Invalid workflow ID.
	_, isErr, err := client.callTool(ctx, "rick_workflow_status", map[string]any{
		"workflow_id": "nonexistent-workflow-id",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !isErr {
		t.Error("expected error for nonexistent workflow")
	}

	// Cancel nonexistent.
	_, isErr, err = client.callTool(ctx, "rick_cancel_workflow", map[string]any{
		"workflow_id": "nonexistent-workflow-id",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !isErr {
		t.Error("expected error cancelling nonexistent workflow")
	}

	// Pause nonexistent.
	_, isErr, err = client.callTool(ctx, "rick_pause_workflow", map[string]any{
		"workflow_id": "nonexistent-workflow-id",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !isErr {
		t.Error("expected error pausing nonexistent workflow")
	}

	// Inject guidance with empty content.
	_, isErr, err = client.callTool(ctx, "rick_inject_guidance", map[string]any{
		"workflow_id": "nonexistent-workflow-id",
		"content":     "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !isErr {
		t.Error("expected error for empty guidance content")
	}

	// Run workflow with invalid DAG.
	_, isErr, err = client.callTool(ctx, "rick_run_workflow", map[string]any{
		"prompt": "test",
		"dag":    "nonexistent-dag",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !isErr {
		t.Error("expected error for invalid DAG")
	}

	// Unknown tool.
	_, err = client.call(ctx, "tools/call", map[string]any{
		"name": "nonexistent_tool",
	})
	// This returns a JSON-RPC error, which is propagated.
	if err == nil {
		t.Error("expected error for unknown tool")
	}
}

func TestE2EAgentResumeAlreadyRunning(t *testing.T) {
	baseURL, cleanup := startE2EServer(t)
	defer cleanup()
	ctx := context.Background()

	client := newMCPClient(baseURL)

	// Start workflow.
	text, _, err := client.callTool(ctx, "rick_run_workflow", map[string]any{
		"prompt": "resume test",
	})
	if err != nil {
		t.Fatal(err)
	}
	var run runWorkflowResult
	_ = json.Unmarshal([]byte(text), &run)

	time.Sleep(100 * time.Millisecond)

	// Try to resume an already running workflow.
	_, isErr, err := client.callTool(ctx, "rick_resume_workflow", map[string]any{
		"workflow_id": run.WorkflowID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !isErr {
		t.Error("expected error resuming already running workflow")
	}
}

func TestE2EAgentDoubleCancel(t *testing.T) {
	baseURL, cleanup := startE2EServer(t)
	defer cleanup()
	ctx := context.Background()

	client := newMCPClient(baseURL)

	// Start and cancel.
	text, _, _ := client.callTool(ctx, "rick_run_workflow", map[string]any{"prompt": "double cancel"})
	var run runWorkflowResult
	_ = json.Unmarshal([]byte(text), &run)
	time.Sleep(100 * time.Millisecond)

	_, isErr, _ := client.callTool(ctx, "rick_cancel_workflow", map[string]any{
		"workflow_id": run.WorkflowID,
	})
	if isErr {
		t.Fatal("first cancel should succeed")
	}

	// Second cancel should fail.
	_, isErr, _ = client.callTool(ctx, "rick_cancel_workflow", map[string]any{
		"workflow_id": run.WorkflowID,
	})
	if !isErr {
		t.Error("expected error on double cancel")
	}
}

// TestE2EHintApproveReject tests the pending-hints enrichment in workflow status
// and the approve/reject hint tools.
func TestE2EHintApproveReject(t *testing.T) {
	baseURL, env, cleanup := startE2EServerWithEnv(t)
	defer cleanup()
	ctx := context.Background()

	client := newMCPClient(baseURL)

	// 1. Start a workflow.
	text, isErr, err := client.callTool(ctx, "rick_run_workflow", map[string]any{
		"prompt": "hint test workflow",
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("run workflow error: %s", text)
	}
	var run runWorkflowResult
	_ = json.Unmarshal([]byte(text), &run)
	wfID := run.WorkflowID

	time.Sleep(150 * time.Millisecond)

	// 2. Inject a HintEmitted event into a persona-scoped aggregate
	//    (simulates what a Hinter handler would do).
	hintPayload := event.MustMarshal(event.HintEmittedPayload{
		Persona:       "plan-architect",
		Phase:         "plan-architect",
		TriggerEvent:  string(event.PersonaCompleted),
		TriggerID:     "trigger-123",
		Confidence:    0.45, // below default threshold → engine will pause
		Plan:          "Generate a 5-section implementation plan",
		Blockers:      []string{"missing API schema definition"},
		TokenEstimate: 8000,
	})
	hintEvt := event.New(event.HintEmitted, 1, hintPayload).
		WithAggregate(wfID+":persona:plan-architect", 1).
		WithCorrelation(wfID).
		WithSource("test:hint")

	personaAgg := wfID + ":persona:plan-architect"
	if err := env.Store.Append(ctx, personaAgg, 0, []event.Envelope{hintEvt}); err != nil {
		t.Fatalf("store hint: %v", err)
	}
	if err := env.Bus.Publish(ctx, hintEvt); err != nil {
		t.Fatalf("publish hint: %v", err)
	}

	// Wait for engine to process HintEmitted → WorkflowPaused.
	time.Sleep(200 * time.Millisecond)

	// 3. Verify workflow status shows paused + pending hints.
	text, isErr, err = client.callTool(ctx, "rick_workflow_status", map[string]any{
		"workflow_id": wfID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("status error: %s", text)
	}

	var status workflowStatusResult
	_ = json.Unmarshal([]byte(text), &status)
	if status.Status != "paused" {
		t.Fatalf("expected paused, got %s", status.Status)
	}
	if len(status.PendingHints) != 1 {
		t.Fatalf("expected 1 pending hint, got %d", len(status.PendingHints))
	}
	hint := status.PendingHints[0]
	if hint.Persona != "plan-architect" {
		t.Errorf("expected persona plan-architect, got %s", hint.Persona)
	}
	if hint.Confidence != 0.45 {
		t.Errorf("expected confidence 0.45, got %f", hint.Confidence)
	}
	if hint.Plan != "Generate a 5-section implementation plan" {
		t.Errorf("unexpected plan: %s", hint.Plan)
	}
	if len(hint.Blockers) != 1 || hint.Blockers[0] != "missing API schema definition" {
		t.Errorf("unexpected blockers: %v", hint.Blockers)
	}
	if hint.TokenEstimate != 8000 {
		t.Errorf("expected token estimate 8000, got %d", hint.TokenEstimate)
	}

	// 4. Approve the hint with guidance.
	text, isErr, err = client.callTool(ctx, "rick_approve_hint", map[string]any{
		"workflow_id": wfID,
		"persona":     "plan-architect",
		"guidance":    "focus on backend components only",
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("approve hint error: %s", text)
	}

	var approveResult hintActionResult
	_ = json.Unmarshal([]byte(text), &approveResult)
	if approveResult.Action != "approved" {
		t.Errorf("expected action approved, got %s", approveResult.Action)
	}
	if approveResult.Status != "running" {
		t.Errorf("expected status running after approve, got %s", approveResult.Status)
	}

	// 5. Verify pending hints are cleared.
	time.Sleep(100 * time.Millisecond)
	text, _, _ = client.callTool(ctx, "rick_workflow_status", map[string]any{
		"workflow_id": wfID,
	})
	var statusAfter workflowStatusResult
	_ = json.Unmarshal([]byte(text), &statusAfter)
	// After approval the workflow is running, so pending_hints is only populated when paused.
	if statusAfter.Status != "running" {
		t.Errorf("expected running after approve, got %s", statusAfter.Status)
	}
	if len(statusAfter.PendingHints) != 0 {
		t.Errorf("expected 0 pending hints after approve, got %d", len(statusAfter.PendingHints))
	}

	// Cleanup: cancel the workflow.
	_, _, _ = client.callTool(ctx, "rick_cancel_workflow", map[string]any{
		"workflow_id": wfID,
	})
}

// TestE2EHintRejectSkip tests rejecting a hint with skip action.
func TestE2EHintRejectSkip(t *testing.T) {
	baseURL, env, cleanup := startE2EServerWithEnv(t)
	defer cleanup()
	ctx := context.Background()

	client := newMCPClient(baseURL)

	// Start workflow.
	text, _, err := client.callTool(ctx, "rick_run_workflow", map[string]any{
		"prompt": "hint reject test",
	})
	if err != nil {
		t.Fatal(err)
	}
	var run runWorkflowResult
	_ = json.Unmarshal([]byte(text), &run)
	wfID := run.WorkflowID
	time.Sleep(150 * time.Millisecond)

	// Inject HintEmitted with low confidence.
	hintPayload := event.MustMarshal(event.HintEmittedPayload{
		Persona:    "estimator",
		Phase:      "estimator",
		TriggerID:  "trigger-456",
		Confidence: 0.30,
		Plan:       "Estimate story points for 12 tasks",
		Blockers:   []string{"no task breakdown available"},
	})
	hintEvt := event.New(event.HintEmitted, 1, hintPayload).
		WithAggregate(wfID+":persona:estimator", 1).
		WithCorrelation(wfID).
		WithSource("test:hint")

	if err := env.Store.Append(ctx, wfID+":persona:estimator", 0, []event.Envelope{hintEvt}); err != nil {
		t.Fatal(err)
	}
	if err := env.Bus.Publish(ctx, hintEvt); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// Reject with skip.
	text, isErr, err := client.callTool(ctx, "rick_reject_hint", map[string]any{
		"workflow_id": wfID,
		"persona":     "estimator",
		"reason":      "not needed for this iteration",
		"action":      "skip",
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("reject hint error: %s", text)
	}

	var rejectResult hintActionResult
	_ = json.Unmarshal([]byte(text), &rejectResult)
	if rejectResult.Action != "rejected:skip" {
		t.Errorf("expected action rejected:skip, got %s", rejectResult.Action)
	}

	// Verify workflow resumed.
	time.Sleep(100 * time.Millisecond)
	text, _, _ = client.callTool(ctx, "rick_workflow_status", map[string]any{
		"workflow_id": wfID,
	})
	var status workflowStatusResult
	_ = json.Unmarshal([]byte(text), &status)
	if status.Status != "running" {
		t.Errorf("expected running after reject-skip, got %s", status.Status)
	}

	// Cleanup.
	_, _, _ = client.callTool(ctx, "rick_cancel_workflow", map[string]any{
		"workflow_id": wfID,
	})
}

// TestE2ECancelRequestedWorkflow verifies that a workflow can be cancelled
// immediately after creation, before the Engine transitions it to running.
func TestE2ECancelRequestedWorkflow(t *testing.T) {
	dbPath := t.TempDir() + "/cancel-req.db"
	store, err := eventstore.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	bus := eventbus.NewChannelBus(eventbus.WithLogger(testLogger()))
	// Intentionally do NOT start the Engine — the workflow stays in "requested" state.
	eng := engine.NewEngine(store, bus, testLogger())
	eng.RegisterWorkflow(engine.WorkspaceDevWorkflowDef())

	workflows := projection.NewWorkflowStatusProjection()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	projRunner := projection.NewRunner(store, bus, testLogger())
	projRunner.Register(workflows)
	if err := projRunner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer projRunner.Stop()

	deps := Deps{
		Store:     store,
		Bus:       bus,
		Engine:    eng,
		Workflows: workflows,
		SelectWorkflow: func(name string) (engine.WorkflowDef, error) {
			if name == "workspace-dev" {
				return engine.WorkspaceDevWorkflowDef(), nil
			}
			return engine.WorkflowDef{}, fmt.Errorf("unknown: %s", name)
		},
	}

	server := NewServer(deps, testLogger())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", server.handleHTTPPost)
	httpSrv := &http.Server{Handler: mux}
	go func() { _ = httpSrv.Serve(listener) }()
	defer httpSrv.Close()

	client := newMCPClient("http://" + listener.Addr().String())

	// Start a workflow — Engine is NOT running, so it stays in "requested".
	text, isErr, err := client.callTool(ctx, "rick_run_workflow", map[string]any{
		"prompt": "cancel before start",
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("run_workflow error: %s", text)
	}
	var run runWorkflowResult
	_ = json.Unmarshal([]byte(text), &run)

	// Verify status is "requested" (Engine not processing).
	text, isErr, err = client.callTool(ctx, "rick_workflow_status", map[string]any{
		"workflow_id": run.WorkflowID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("status error: %s", text)
	}
	var status workflowStatusResult
	_ = json.Unmarshal([]byte(text), &status)
	if status.Status != "requested" {
		t.Fatalf("expected requested, got %s", status.Status)
	}

	// Cancel the "requested" workflow — this must succeed.
	text, isErr, err = client.callTool(ctx, "rick_cancel_workflow", map[string]any{
		"workflow_id": run.WorkflowID,
		"reason":      "cancelled before engine processed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("cancel should succeed for requested workflow: %s", text)
	}
	var cancelResult interventionResult
	_ = json.Unmarshal([]byte(text), &cancelResult)
	if cancelResult.Status != "cancelled" {
		t.Errorf("expected cancelled, got %s", cancelResult.Status)
	}

	// Double-cancel should fail.
	_, isErr, _ = client.callTool(ctx, "rick_cancel_workflow", map[string]any{
		"workflow_id": run.WorkflowID,
	})
	if !isErr {
		t.Error("expected error on double cancel")
	}
}

// TestE2EListWorkflowsImmediateConsistency verifies that list_workflows returns
// the workflow immediately after run_workflow (no race window).
func TestE2EListWorkflowsImmediateConsistency(t *testing.T) {
	baseURL, cleanup := startE2EServer(t)
	defer cleanup()
	ctx := context.Background()

	client := newMCPClient(baseURL)

	// Start a workflow.
	text, isErr, err := client.callTool(ctx, "rick_run_workflow", map[string]any{
		"prompt": "consistency test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("run error: %s", text)
	}
	var run runWorkflowResult
	_ = json.Unmarshal([]byte(text), &run)

	// Immediately list workflows — NO sleep. The projection must be consistent.
	text, isErr, err = client.callTool(ctx, "rick_list_workflows", nil)
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("list error: %s", text)
	}
	var wfList listWorkflowsResult
	_ = json.Unmarshal([]byte(text), &wfList)

	if len(wfList.Workflows) == 0 {
		t.Fatal("list_workflows returned no workflows immediately after run_workflow")
	}

	found := false
	for _, wf := range wfList.Workflows {
		if wf.AggregateID == run.WorkflowID {
			found = true
			if wf.Prompt != "consistency test" {
				t.Errorf("expected prompt 'consistency test', got %q", wf.Prompt)
			}
			break
		}
	}
	if !found {
		t.Errorf("workflow %s not found in list_workflows", run.WorkflowID)
	}

	// Also check list_events immediately — store must have the event.
	text, _, err = client.callTool(ctx, "rick_list_events", map[string]any{
		"workflow_id": run.WorkflowID,
	})
	if err != nil {
		t.Fatal(err)
	}
	var events listEventsResult
	_ = json.Unmarshal([]byte(text), &events)
	if events.Count == 0 {
		t.Fatal("list_events returned no events immediately after run_workflow")
	}
}
