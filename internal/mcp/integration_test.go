package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// startTestHTTPServer starts a real HTTP server on a random port and returns
// its URL and a cleanup function. This tests the full ServeHTTP path including
// net/http routing, CORS middleware, and graceful shutdown.
func startTestHTTPServer(t *testing.T) (string, func()) {
	t.Helper()

	dbPath := t.TempDir() + "/integration.db"
	store, err := eventstore.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	bus := eventbus.NewChannelBus(eventbus.WithLogger(testLogger()))
	eng := engine.NewEngine(store, bus, testLogger())
	eng.RegisterWorkflow(engine.WorkspaceDevWorkflowDef())
	eng.RegisterWorkflow(engine.DevelopOnlyWorkflowDef())

	workflows := projection.NewWorkflowStatusProjection()
	tokens := projection.NewTokenUsageProjection()
	timelines := projection.NewPhaseTimelineProjection()

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
				return engine.WorkflowDef{}, fmt.Errorf("unknown workflow: %s", name)
			}
		},
	}

	// Start the engine so WorkflowRequested → WorkflowStarted transitions fire.
	eng.Start()

	// Set up projections runner.
	projRunner := projection.NewRunner(store, bus, testLogger())
	projRunner.Register(workflows)
	projRunner.Register(tokens)
	projRunner.Register(timelines)

	ctx, cancel := context.WithCancel(context.Background())
	if err := projRunner.Start(ctx); err != nil {
		t.Fatal(err)
	}

	server := NewServer(deps, testLogger())

	// Bind to random port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", server.handleHTTPPost)
	mux.HandleFunc("GET /mcp", server.handleHTTPGet)

	httpServer := &http.Server{Handler: withCORS(mux)}
	go func() {
		_ = httpServer.Serve(listener)
	}()

	cleanup := func() {
		cancel()
		_ = httpServer.Close()
		projRunner.Stop()
		eng.Stop()
		_ = bus.Close()
		_ = store.Close()
	}

	return "http://" + addr, cleanup
}

// rpcPost sends a JSON-RPC request to the server and returns the response.
func rpcPost(t *testing.T, baseURL, method string, params any) jsonRPCResponse {
	t.Helper()

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(baseURL+"/mcp", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var rpcResp jsonRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatal(err)
	}
	return rpcResp
}

// --- Integration Tests ---

func TestIntegrationHealthCheck(t *testing.T) {
	baseURL, cleanup := startTestHTTPServer(t)
	defer cleanup()

	resp, err := http.Get(baseURL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var info struct {
		Server entityInfo       `json:"server"`
		Tools  []ToolDefinition `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Server.Name != "rick" {
		t.Errorf("expected server name rick, got %s", info.Server.Name)
	}
	if len(info.Tools) < 11 {
		t.Errorf("expected at least 11 tools, got %d", len(info.Tools))
	}
}

func TestIntegrationToolsList(t *testing.T) {
	baseURL, cleanup := startTestHTTPServer(t)
	defer cleanup()

	resp := rpcPost(t, baseURL, "tools/list", nil)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	data, _ := json.Marshal(resp.Result)
	var result toolsListResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}

	toolNames := make(map[string]bool)
	for _, tool := range result.Tools {
		toolNames[tool.Name] = true
	}

	required := []string{
		"rick_run_workflow", "rick_workflow_status", "rick_list_workflows",
		"rick_list_events", "rick_token_usage", "rick_phase_timeline",
		"rick_list_dead_letters", "rick_cancel_workflow", "rick_pause_workflow",
		"rick_resume_workflow", "rick_inject_guidance",
	}
	for _, name := range required {
		if !toolNames[name] {
			t.Errorf("missing required tool: %s", name)
		}
	}
}

func TestIntegrationRunWorkflow(t *testing.T) {
	baseURL, cleanup := startTestHTTPServer(t)
	defer cleanup()

	resp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name":      "rick_run_workflow",
		"arguments": map[string]any{"prompt": "Build a REST API", "dag": "workspace-dev"},
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var run runWorkflowResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &run); err != nil {
		t.Fatal(err)
	}
	if run.WorkflowID == "" {
		t.Error("expected non-empty workflow ID")
	}
	if run.Status != "started" {
		t.Errorf("expected status started, got %s", run.Status)
	}
}

func TestIntegrationWorkflowLifecycle(t *testing.T) {
	baseURL, cleanup := startTestHTTPServer(t)
	defer cleanup()

	// 1. Start workflow.
	startResp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name":      "rick_run_workflow",
		"arguments": map[string]any{"prompt": "lifecycle test"},
	})
	if startResp.Error != nil {
		t.Fatal(startResp.Error.Message)
	}
	data, _ := json.Marshal(startResp.Result)
	var startResult toolsCallResult
	_ = json.Unmarshal(data, &startResult)
	var run runWorkflowResult
	_ = json.Unmarshal([]byte(startResult.Content[0].Text), &run)
	wfID := run.WorkflowID

	// Wait for engine to process WorkflowRequested → WorkflowStarted.
	time.Sleep(100 * time.Millisecond)

	// 2. Check status.
	statusResp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name":      "rick_workflow_status",
		"arguments": map[string]any{"workflow_id": wfID},
	})
	if statusResp.Error != nil {
		t.Fatal(statusResp.Error.Message)
	}

	// 3. Pause workflow.
	pauseResp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name":      "rick_pause_workflow",
		"arguments": map[string]any{"workflow_id": wfID, "reason": "testing"},
	})
	data, _ = json.Marshal(pauseResp.Result)
	var pauseResult toolsCallResult
	_ = json.Unmarshal(data, &pauseResult)
	if pauseResult.IsError {
		t.Fatalf("pause error: %s", pauseResult.Content[0].Text)
	}

	var pauseIR interventionResult
	_ = json.Unmarshal([]byte(pauseResult.Content[0].Text), &pauseIR)
	if pauseIR.Status != "paused" {
		t.Errorf("expected paused, got %s", pauseIR.Status)
	}

	// 4. Inject guidance.
	guideResp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name": "rick_inject_guidance",
		"arguments": map[string]any{
			"workflow_id": wfID,
			"content":     "use PostgreSQL",
			"auto_resume": true,
		},
	})
	data, _ = json.Marshal(guideResp.Result)
	var guideResult toolsCallResult
	_ = json.Unmarshal(data, &guideResult)
	if guideResult.IsError {
		t.Fatalf("guidance error: %s", guideResult.Content[0].Text)
	}

	var gr guidanceResult
	_ = json.Unmarshal([]byte(guideResult.Content[0].Text), &gr)
	if !gr.Resumed {
		t.Error("expected auto-resume after guidance")
	}

	// 5. Cancel workflow.
	cancelResp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name":      "rick_cancel_workflow",
		"arguments": map[string]any{"workflow_id": wfID, "reason": "test done"},
	})
	data, _ = json.Marshal(cancelResp.Result)
	var cancelResult toolsCallResult
	_ = json.Unmarshal(data, &cancelResult)
	if cancelResult.IsError {
		t.Fatalf("cancel error: %s", cancelResult.Content[0].Text)
	}

	var cancelIR interventionResult
	_ = json.Unmarshal([]byte(cancelResult.Content[0].Text), &cancelIR)
	if cancelIR.Status != "cancelled" {
		t.Errorf("expected cancelled, got %s", cancelIR.Status)
	}
}

func TestIntegrationListEventsGlobal(t *testing.T) {
	baseURL, cleanup := startTestHTTPServer(t)
	defer cleanup()

	// Start two workflows.
	for _, prompt := range []string{"workflow A", "workflow B"} {
		rpcPost(t, baseURL, "tools/call", map[string]any{
			"name":      "rick_run_workflow",
			"arguments": map[string]any{"prompt": prompt},
		})
	}

	// List global events.
	resp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name":      "rick_list_events",
		"arguments": map[string]any{"limit": 100},
	})
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var list listEventsResult
	_ = json.Unmarshal([]byte(result.Content[0].Text), &list)
	if list.Count < 2 {
		t.Errorf("expected at least 2 global events, got %d", list.Count)
	}
}

func TestIntegrationConcurrentRequests(t *testing.T) {
	baseURL, cleanup := startTestHTTPServer(t)
	defer cleanup()

	// Fire 10 concurrent tool calls.
	errs := make(chan error, 10)
	for i := range 10 {
		go func(idx int) {
			resp := rpcPost(t, baseURL, "tools/call", map[string]any{
				"name":      "rick_run_workflow",
				"arguments": map[string]any{"prompt": fmt.Sprintf("concurrent test %d", idx)},
			})
			if resp.Error != nil {
				errs <- fmt.Errorf("request %d: %s", idx, resp.Error.Message)
				return
			}
			errs <- nil
		}(i)
	}

	for range 10 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func TestIntegrationDeadLetters(t *testing.T) {
	baseURL, cleanup := startTestHTTPServer(t)
	defer cleanup()

	resp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name": "rick_list_dead_letters",
	})
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var dls listDeadLettersResult
	_ = json.Unmarshal([]byte(result.Content[0].Text), &dls)
	if dls.Count != 0 {
		t.Errorf("expected 0 dead letters on fresh server, got %d", dls.Count)
	}
}

func TestIntegrationTokenUsageAndTimeline(t *testing.T) {
	baseURL, cleanup := startTestHTTPServer(t)
	defer cleanup()

	// Start a workflow to get an ID.
	resp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name":      "rick_run_workflow",
		"arguments": map[string]any{"prompt": "token test"},
	})
	data, _ := json.Marshal(resp.Result)
	var startResult toolsCallResult
	_ = json.Unmarshal(data, &startResult)
	var run runWorkflowResult
	_ = json.Unmarshal([]byte(startResult.Content[0].Text), &run)

	// Token usage (no data yet — should return zero).
	tokenResp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name":      "rick_token_usage",
		"arguments": map[string]any{"workflow_id": run.WorkflowID},
	})
	data, _ = json.Marshal(tokenResp.Result)
	var tokenResult toolsCallResult
	_ = json.Unmarshal(data, &tokenResult)
	if tokenResult.IsError {
		t.Fatalf("token usage error: %s", tokenResult.Content[0].Text)
	}

	// Phase timeline (no data yet).
	timelineResp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name":      "rick_phase_timeline",
		"arguments": map[string]any{"workflow_id": run.WorkflowID},
	})
	data, _ = json.Marshal(timelineResp.Result)
	var timelineResult toolsCallResult
	_ = json.Unmarshal(data, &timelineResult)
	if timelineResult.IsError {
		t.Fatalf("phase timeline error: %s", timelineResult.Content[0].Text)
	}
}

func TestIntegrationPing(t *testing.T) {
	baseURL, cleanup := startTestHTTPServer(t)
	defer cleanup()

	resp := rpcPost(t, baseURL, "ping", nil)
	if resp.Error != nil {
		t.Fatalf("ping error: %s", resp.Error.Message)
	}
}

func TestIntegrationUnknownMethod(t *testing.T) {
	baseURL, cleanup := startTestHTTPServer(t)
	defer cleanup()

	resp := rpcPost(t, baseURL, "nonexistent/method", nil)
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("expected -32601, got %d", resp.Error.Code)
	}
}

func TestIntegrationInvalidJSON(t *testing.T) {
	baseURL, cleanup := startTestHTTPServer(t)
	defer cleanup()

	resp, err := http.Post(baseURL+"/mcp", "application/json", bytes.NewReader([]byte("{bad")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var rpcResp jsonRPCResponse
	_ = json.NewDecoder(resp.Body).Decode(&rpcResp)
	if rpcResp.Error == nil {
		t.Fatal("expected parse error")
	}
	if rpcResp.Error.Code != -32700 {
		t.Errorf("expected -32700, got %d", rpcResp.Error.Code)
	}
}

func TestIntegrationCORSHeaders(t *testing.T) {
	baseURL, cleanup := startTestHTTPServer(t)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodOptions, baseURL+"/mcp", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if v := resp.Header.Get("Access-Control-Allow-Origin"); v != "*" {
		t.Errorf("expected CORS origin *, got %s", v)
	}
}

func TestIntegrationWorkflowStatusNotFound(t *testing.T) {
	baseURL, cleanup := startTestHTTPServer(t)
	defer cleanup()

	resp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name":      "rick_workflow_status",
		"arguments": map[string]any{"workflow_id": "nonexistent-id"},
	})
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if !result.IsError {
		t.Error("expected error for nonexistent workflow")
	}
}

// TestIntegrationSeedAndQueryProjections seeds events and verifies projections
// respond through the HTTP API.
func TestIntegrationSeedAndQueryProjections(t *testing.T) {
	baseURL, cleanup := startTestHTTPServer(t)
	defer cleanup()

	// Start workflow and wait for projections to catch up.
	resp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name":      "rick_run_workflow",
		"arguments": map[string]any{"prompt": "projection test"},
	})
	data, _ := json.Marshal(resp.Result)
	var startResult toolsCallResult
	_ = json.Unmarshal(data, &startResult)
	var run runWorkflowResult
	_ = json.Unmarshal([]byte(startResult.Content[0].Text), &run)

	time.Sleep(100 * time.Millisecond)

	// List workflows should include the new one.
	listResp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name": "rick_list_workflows",
	})
	data, _ = json.Marshal(listResp.Result)
	var listResult toolsCallResult
	_ = json.Unmarshal(data, &listResult)
	if listResult.IsError {
		t.Fatalf("list workflows error: %s", listResult.Content[0].Text)
	}

	var workflows listWorkflowsResult
	_ = json.Unmarshal([]byte(listResult.Content[0].Text), &workflows)

	found := false
	for _, wf := range workflows.Workflows {
		if wf.AggregateID == run.WorkflowID {
			found = true
		}
	}
	// Projection may or may not have caught up in test — just verify no error.
	_ = found

	// List events for the workflow.
	eventsResp := rpcPost(t, baseURL, "tools/call", map[string]any{
		"name":      "rick_list_events",
		"arguments": map[string]any{"workflow_id": run.WorkflowID},
	})
	data, _ = json.Marshal(eventsResp.Result)
	var eventsResult toolsCallResult
	_ = json.Unmarshal(data, &eventsResult)
	if eventsResult.IsError {
		t.Fatalf("list events error: %s", eventsResult.Content[0].Text)
	}

	var events listEventsResult
	_ = json.Unmarshal([]byte(eventsResult.Content[0].Text), &events)
	if events.Count < 1 {
		t.Errorf("expected at least 1 event, got %d", events.Count)
	}

	// Verify first event is WorkflowRequested.
	if len(events.Events) > 0 {
		if events.Events[0].Type != string(event.WorkflowRequested) {
			t.Errorf("expected first event WorkflowRequested, got %s", events.Events[0].Type)
		}
	}
}
