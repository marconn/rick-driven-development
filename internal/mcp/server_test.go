package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/projection"
)

// --- Test Helpers ---

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

func testDeps(t *testing.T) (Deps, func()) {
	t.Helper()
	dbPath := t.TempDir() + "/test.db"
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

	verdicts := projection.NewVerdictProjection()

	deps := Deps{
		Store:     store,
		Bus:       bus,
		Engine:    eng,
		Workflows: workflows,
		Tokens:    tokens,
		Timelines: timelines,
		Verdicts:  verdicts,
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

	cleanup := func() {
		_ = bus.Close()
		_ = store.Close()
	}
	return deps, cleanup
}

// sendRequest encodes a JSON-RPC request and returns the raw line.
func sendRequest(id int, method string, params any) string {
	req := jsonRPCRequest{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage(fmt.Sprintf("%d", id)),
		Method:  method,
	}
	if params != nil {
		data, _ := json.Marshal(params)
		req.Params = data
	}
	line, _ := json.Marshal(req)
	return string(line)
}

// sendNotification encodes a JSON-RPC notification (no id).
func sendNotification(method string) string {
	req := map[string]any{
		"jsonrpc": jsonRPCVersion,
		"method":  method,
	}
	line, _ := json.Marshal(req)
	return string(line)
}

// parseResponse decodes a JSON-RPC response from a line.
func parseResponse(t *testing.T, line string) jsonRPCResponse {
	t.Helper()
	var resp jsonRPCResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("parse response: %v\nline: %s", err, line)
	}
	return resp
}

// serveLines sends multiple lines and collects all response lines.
func serveLines(t *testing.T, s *Server, lines ...string) []string {
	t.Helper()
	input := strings.Join(lines, "\n") + "\n"
	var out bytes.Buffer
	err := s.Serve(context.Background(), strings.NewReader(input), &out)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimSpace(out.String())
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// --- Tests ---

func TestInitialize(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s,
		sendRequest(1, methodInitialize, initializeParams{
			ProtocolVersion: protocolVersion,
			ClientInfo:      entityInfo{Name: "test", Version: "1.0"},
		}),
	)

	if len(lines) != 1 {
		t.Fatalf("expected 1 response, got %d", len(lines))
	}

	resp := parseResponse(t, lines[0])
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	data, _ := json.Marshal(resp.Result)
	var result initializeResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.ProtocolVersion != protocolVersion {
		t.Errorf("expected protocol %s, got %s", protocolVersion, result.ProtocolVersion)
	}
	if result.ServerInfo.Name != "rick" {
		t.Errorf("expected server name rick, got %s", result.ServerInfo.Name)
	}
	if result.Capabilities.Tools == nil {
		t.Error("expected tools capability")
	}
}

func TestInitializedNotification(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendNotification(methodInitialized))

	// Notifications produce no response
	if len(lines) != 0 {
		t.Fatalf("expected 0 responses for notification, got %d", len(lines))
	}

	s.mu.Lock()
	if !s.initialized {
		t.Error("expected server to be marked initialized")
	}
	s.mu.Unlock()
}

func TestPing(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, methodPing, nil))
	if len(lines) != 1 {
		t.Fatalf("expected 1 response, got %d", len(lines))
	}
	resp := parseResponse(t, lines[0])
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
}

func TestToolsList(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, methodToolsList, nil))
	if len(lines) != 1 {
		t.Fatalf("expected 1 response, got %d", len(lines))
	}
	resp := parseResponse(t, lines[0])
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	data, _ := json.Marshal(resp.Result)
	var result toolsListResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}

	expectedTools := map[string]bool{
		"rick_run_workflow":      false,
		"rick_workflow_status":   false,
		"rick_list_workflows":    false,
		"rick_list_events":       false,
		"rick_token_usage":       false,
		"rick_phase_timeline":    false,
		"rick_workflow_verdicts": false,
		"rick_persona_output":    false,
		"rick_list_dead_letters": false,
		"rick_cancel_workflow":   false,
		"rick_pause_workflow":    false,
		"rick_resume_workflow":   false,
		"rick_inject_guidance":   false,
	}

	for _, tool := range result.Tools {
		if _, ok := expectedTools[tool.Name]; ok {
			expectedTools[tool.Name] = true
		}
	}
	for name, found := range expectedTools {
		if !found {
			t.Errorf("expected tool %s not found in tools/list", name)
		}
	}
}

func TestToolRunWorkflow(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_run_workflow",
		Arguments: json.RawMessage(`{"prompt":"Build a REST API","dag":"workspace-dev"}`),
	}))

	if len(lines) != 1 {
		t.Fatalf("expected 1 response, got %d", len(lines))
	}
	resp := parseResponse(t, lines[0])
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %s", result.Content[0].Text)
	}

	// Parse the inner JSON content
	var run runWorkflowResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &run); err != nil {
		t.Fatal(err)
	}
	if run.WorkflowID == "" {
		t.Error("expected non-empty workflow_id")
	}
	if run.Status != "started" {
		t.Errorf("expected status started, got %s", run.Status)
	}
	if run.DAG != "workspace-dev" {
		t.Errorf("expected dag workspace-dev, got %s", run.DAG)
	}
}

func TestToolRunWorkflowMissingPrompt(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_run_workflow",
		Arguments: json.RawMessage(`{}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if !result.IsError {
		t.Error("expected error for missing prompt")
	}
}

func TestToolRunWorkflowInvalidDAG(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_run_workflow",
		Arguments: json.RawMessage(`{"prompt":"test","dag":"nonexistent"}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if !result.IsError {
		t.Error("expected error for invalid DAG")
	}
}

func TestToolWorkflowStatus(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	// Seed an event
	aggregateID := "test-workflow-1"
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "test prompt",
		WorkflowID: "workspace-dev",
		Source:     "test",
	})).WithAggregate(aggregateID, 1).WithCorrelation("corr-1").WithSource("test")

	if err := deps.Store.Append(context.Background(), aggregateID, 0, []event.Envelope{reqEvt}); err != nil {
		t.Fatal(err)
	}

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_workflow_status",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s"}`, aggregateID)),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var status workflowStatusResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &status); err != nil {
		t.Fatal(err)
	}
	if status.Status != "requested" {
		t.Errorf("expected status requested, got %s", status.Status)
	}
	if status.WorkflowID != "workspace-dev" {
		t.Errorf("expected workflow_id workspace-dev, got %s", status.WorkflowID)
	}
}

func TestToolWorkflowStatusNotFound(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_workflow_status",
		Arguments: json.RawMessage(`{"workflow_id":"nonexistent"}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if !result.IsError {
		t.Error("expected error for nonexistent workflow")
	}
}

func TestToolListWorkflows(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	// Seed projection
	env := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		WorkflowID: "workspace-dev",
	})).WithAggregate("wf-1", 1).WithCorrelation("c-1")
	_ = deps.Workflows.Handle(context.Background(), env)

	s := NewServer(deps, testLogger())
	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name: "rick_list_workflows",
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var list listWorkflowsResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Workflows) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(list.Workflows))
	}
	if list.Workflows[0].AggregateID != "wf-1" {
		t.Errorf("expected wf-1, got %s", list.Workflows[0].AggregateID)
	}
}

func TestToolListEvents(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	// Seed events
	aggregateID := "evt-wf-1"
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "workspace-dev",
	})).WithAggregate(aggregateID, 1).WithCorrelation("c-1").WithSource("test")

	startEvt := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "workspace-dev", Phases: []string{"research"},
	})).WithAggregate(aggregateID, 2).WithCorrelation("c-1").WithSource("test")

	_ = deps.Store.Append(context.Background(), aggregateID, 0, []event.Envelope{reqEvt, startEvt})

	// List events for workflow
	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_list_events",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s"}`, aggregateID)),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var list listEventsResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &list); err != nil {
		t.Fatal(err)
	}
	if list.Count != 2 {
		t.Errorf("expected 2 events, got %d", list.Count)
	}
}

func TestToolListEventsGlobal(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	// Seed events across two workflows
	for _, id := range []string{"g-wf-1", "g-wf-2"} {
		evt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "test", WorkflowID: "workspace-dev",
		})).WithAggregate(id, 1).WithCorrelation("c-"+id).WithSource("test")
		_ = deps.Store.Append(context.Background(), id, 0, []event.Envelope{evt})
	}

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_list_events",
		Arguments: json.RawMessage(`{"limit":10}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var list listEventsResult
	_ = json.Unmarshal([]byte(result.Content[0].Text), &list)
	if list.Count != 2 {
		t.Errorf("expected 2 events from global stream, got %d", list.Count)
	}
}

func TestToolTokenUsage(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	// Seed token usage
	aiEvt := event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
		Phase: "research", Backend: "claude", TokensUsed: 1500, DurationMS: 3000,
	})).WithAggregate("tk-wf-1", 1).WithCorrelation("c-1")
	_ = deps.Tokens.Handle(context.Background(), aiEvt)

	s := NewServer(deps, testLogger())
	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_token_usage",
		Arguments: json.RawMessage(`{"workflow_id":"tk-wf-1"}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var usage tokenUsageResult
	_ = json.Unmarshal([]byte(result.Content[0].Text), &usage)
	if usage.Total != 1500 {
		t.Errorf("expected 1500 tokens, got %d", usage.Total)
	}
	if usage.ByPhase["research"] != 1500 {
		t.Errorf("expected 1500 for research, got %d", usage.ByPhase["research"])
	}
}

func TestToolTokenUsageNotTracked(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_token_usage",
		Arguments: json.RawMessage(`{"workflow_id":"nonexistent"}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Error("expected success with zero usage for unknown workflow")
	}

	var usage tokenUsageResult
	_ = json.Unmarshal([]byte(result.Content[0].Text), &usage)
	if usage.Total != 0 {
		t.Errorf("expected 0 tokens, got %d", usage.Total)
	}
}

func TestToolPhaseTimeline(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	// Seed timeline with a PersonaCompleted event. The projection keys by CorrelationID
	// (the workflow aggregate ID), so we use WithCorrelation("tl-wf-1") here.
	completeEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:    "research",
		DurationMS: 5000,
	})).WithAggregate("tl-wf-1:persona:research", 1).WithCorrelation("tl-wf-1")

	_ = deps.Timelines.Handle(context.Background(), completeEvt)

	s := NewServer(deps, testLogger())
	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_phase_timeline",
		Arguments: json.RawMessage(`{"workflow_id":"tl-wf-1"}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var timeline phaseTimelineResult
	_ = json.Unmarshal([]byte(result.Content[0].Text), &timeline)
	if len(timeline.Phases) != 1 {
		t.Fatalf("expected 1 phase, got %d", len(timeline.Phases))
	}
	if timeline.Phases[0].Phase != "research" {
		t.Errorf("expected phase research, got %s", timeline.Phases[0].Phase)
	}
	if timeline.Phases[0].DurationMS < 5000 {
		t.Errorf("expected duration >= 5000ms, got %d", timeline.Phases[0].DurationMS)
	}
}

func TestToolListDeadLetters(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	// Seed a dead letter
	dl := eventstore.DeadLetter{
		ID:       "dl-1",
		EventID:  "evt-1",
		Handler:  "researcher",
		Error:    "timeout",
		Attempts: 3,
		FailedAt: "2026-03-12T10:00:00Z",
	}
	_ = deps.Store.RecordDeadLetter(context.Background(), dl)

	s := NewServer(deps, testLogger())
	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name: "rick_list_dead_letters",
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var dls listDeadLettersResult
	_ = json.Unmarshal([]byte(result.Content[0].Text), &dls)
	if dls.Count != 1 {
		t.Fatalf("expected 1 dead letter, got %d", dls.Count)
	}
	if dls.DeadLetters[0].Handler != "researcher" {
		t.Errorf("expected handler researcher, got %s", dls.DeadLetters[0].Handler)
	}
}

func TestToolListDeadLettersEmpty(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name: "rick_list_dead_letters",
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var dls listDeadLettersResult
	_ = json.Unmarshal([]byte(result.Content[0].Text), &dls)
	if dls.Count != 0 {
		t.Errorf("expected 0 dead letters, got %d", dls.Count)
	}
}

func TestUnknownMethod(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, "nonexistent/method", nil))
	resp := parseResponse(t, lines[0])
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("expected code -32601, got %d", resp.Error.Code)
	}
}

func TestUnknownTool(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name: "nonexistent_tool",
	}))

	resp := parseResponse(t, lines[0])
	if resp.Error == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestInvalidJSON(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, "{invalid json")
	resp := parseResponse(t, lines[0])
	if resp.Error == nil {
		t.Fatal("expected parse error")
	}
	if resp.Error.Code != -32700 {
		t.Errorf("expected code -32700, got %d", resp.Error.Code)
	}
}

// --- Operator Intervention Tool Tests ---

func TestToolCancelWorkflow(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	// Seed a running workflow
	aggregateID := "cancel-wf-1"
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "workspace-dev",
	})).WithAggregate(aggregateID, 1).WithCorrelation(aggregateID).WithSource("test")
	startEvt := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "workspace-dev",
	})).WithAggregate(aggregateID, 2).WithCorrelation(aggregateID).WithSource("test")
	_ = deps.Store.Append(context.Background(), aggregateID, 0, []event.Envelope{reqEvt, startEvt})

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_cancel_workflow",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s","reason":"test cancel"}`, aggregateID)),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var ir interventionResult
	_ = json.Unmarshal([]byte(result.Content[0].Text), &ir)
	if ir.Action != "cancelled" {
		t.Errorf("expected action cancelled, got %s", ir.Action)
	}
	if ir.Status != "cancelled" {
		t.Errorf("expected status cancelled, got %s", ir.Status)
	}

	// Verify event was persisted
	events, _ := deps.Store.Load(context.Background(), aggregateID)
	found := false
	for _, e := range events {
		if e.Type == event.WorkflowCancelled {
			found = true
		}
	}
	if !found {
		t.Error("WorkflowCancelled event not found in store")
	}
}

func TestToolCancelWorkflowAlreadyCompleted(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	aggregateID := "cancel-completed-1"
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "workspace-dev",
	})).WithAggregate(aggregateID, 1).WithCorrelation(aggregateID)
	completeEvt := event.New(event.WorkflowCompleted, 1, event.MustMarshal(event.WorkflowCompletedPayload{
		Result: "done",
	})).WithAggregate(aggregateID, 2).WithCorrelation(aggregateID)
	_ = deps.Store.Append(context.Background(), aggregateID, 0, []event.Envelope{reqEvt, completeEvt})

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_cancel_workflow",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s"}`, aggregateID)),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if !result.IsError {
		t.Error("expected error when cancelling completed workflow")
	}
}

func TestToolPauseResumeWorkflow(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	aggregateID := "pause-wf-1"
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "workspace-dev",
	})).WithAggregate(aggregateID, 1).WithCorrelation(aggregateID)
	startEvt := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "workspace-dev",
	})).WithAggregate(aggregateID, 2).WithCorrelation(aggregateID)
	_ = deps.Store.Append(context.Background(), aggregateID, 0, []event.Envelope{reqEvt, startEvt})

	// Pause
	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_pause_workflow",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s","reason":"investigating"}`, aggregateID)),
	}))
	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("pause error: %s", result.Content[0].Text)
	}

	// Verify paused
	var ir interventionResult
	_ = json.Unmarshal([]byte(result.Content[0].Text), &ir)
	if ir.Status != "paused" {
		t.Errorf("expected paused, got %s", ir.Status)
	}

	// Resume
	lines = serveLines(t, s, sendRequest(2, methodToolsCall, toolsCallParams{
		Name:      "rick_resume_workflow",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s","reason":"fixed"}`, aggregateID)),
	}))
	resp = parseResponse(t, lines[0])
	data, _ = json.Marshal(resp.Result)
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("resume error: %s", result.Content[0].Text)
	}

	_ = json.Unmarshal([]byte(result.Content[0].Text), &ir)
	if ir.Status != "running" {
		t.Errorf("expected running after resume, got %s", ir.Status)
	}
}

func TestToolInjectGuidance(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	aggregateID := "guide-wf-1"
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "workspace-dev",
	})).WithAggregate(aggregateID, 1).WithCorrelation(aggregateID)
	startEvt := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "workspace-dev",
	})).WithAggregate(aggregateID, 2).WithCorrelation(aggregateID)
	pauseEvt := event.New(event.WorkflowPaused, 1, event.MustMarshal(event.WorkflowPausedPayload{
		Reason: "test",
	})).WithAggregate(aggregateID, 3).WithCorrelation(aggregateID)
	_ = deps.Store.Append(context.Background(), aggregateID, 0, []event.Envelope{reqEvt, startEvt, pauseEvt})

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_inject_guidance",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s","content":"use sql.NullString","auto_resume":true}`, aggregateID)),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("guidance error: %s", result.Content[0].Text)
	}

	var gr guidanceResult
	_ = json.Unmarshal([]byte(result.Content[0].Text), &gr)
	if !gr.Resumed {
		t.Error("expected auto-resume")
	}

	// Verify both guidance and resume events persisted
	events, _ := deps.Store.Load(context.Background(), aggregateID)
	hasGuidance, hasResume := false, false
	for _, e := range events {
		if e.Type == event.OperatorGuidance {
			hasGuidance = true
		}
		if e.Type == event.WorkflowResumed {
			hasResume = true
		}
	}
	if !hasGuidance {
		t.Error("OperatorGuidance event not found")
	}
	if !hasResume {
		t.Error("WorkflowResumed event not found (auto-resume)")
	}
}

// --- Verdict Tool Tests ---

func TestToolWorkflowVerdicts(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	// Seed verdict via the projection.
	verdictEvt := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase:       "develop",
		SourcePhase: "review",
		Outcome:     event.VerdictFail,
		Summary:     "Missing error handling",
		Issues: []event.Issue{
			{Severity: "major", Category: "correctness", Description: "unhandled error", File: "main.go", Line: 42},
		},
	})).WithAggregate("vd-wf-1", 1).WithCorrelation("vd-wf-1")

	if err := deps.Verdicts.Handle(context.Background(), verdictEvt); err != nil {
		t.Fatal(err)
	}

	s := NewServer(deps, testLogger())
	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_workflow_verdicts",
		Arguments: json.RawMessage(`{"workflow_id":"vd-wf-1"}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var vr workflowVerdictsResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &vr); err != nil {
		t.Fatal(err)
	}
	if vr.Count != 1 {
		t.Fatalf("expected 1 verdict, got %d", vr.Count)
	}
	if vr.Verdicts[0].Outcome != "fail" {
		t.Errorf("expected outcome fail, got %s", vr.Verdicts[0].Outcome)
	}
	if vr.Verdicts[0].Phase != "develop" {
		t.Errorf("expected phase develop, got %s", vr.Verdicts[0].Phase)
	}
	if vr.Verdicts[0].SourcePhase != "review" {
		t.Errorf("expected source_phase review, got %s", vr.Verdicts[0].SourcePhase)
	}
	if len(vr.Verdicts[0].Issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(vr.Verdicts[0].Issues))
	}
	if vr.Verdicts[0].Issues[0].File != "main.go" {
		t.Errorf("expected file main.go, got %s", vr.Verdicts[0].Issues[0].File)
	}
}

func TestToolWorkflowVerdictsEmpty(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_workflow_verdicts",
		Arguments: json.RawMessage(`{"workflow_id":"unknown-wf"}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("expected success for unknown workflow, got error: %s", result.Content[0].Text)
	}

	var vr workflowVerdictsResult
	_ = json.Unmarshal([]byte(result.Content[0].Text), &vr)
	if vr.Count != 0 {
		t.Errorf("expected count 0, got %d", vr.Count)
	}
	if len(vr.Verdicts) != 0 {
		t.Errorf("expected empty verdicts slice, got %d", len(vr.Verdicts))
	}
}

// --- Persona Output Tool Tests ---

func seedPersonaOutput(t *testing.T, deps Deps, workflowID, persona, outputText string) {
	t.Helper()
	ctx := context.Background()
	aggregateID := workflowID + ":persona:" + persona

	aiEvt := event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
		Phase:      persona,
		Backend:    "claude",
		TokensUsed: 2500,
		DurationMS: 8000,
		Output:     json.RawMessage(`"` + outputText + `"`),
	})).WithAggregate(aggregateID, 1).WithCorrelation(workflowID)

	if err := deps.Store.Append(ctx, aggregateID, 0, []event.Envelope{aiEvt}); err != nil {
		t.Fatal(err)
	}

	completedEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:    persona,
		OutputRef:  string(aiEvt.ID),
		DurationMS: 8000,
	})).WithAggregate(aggregateID, 2).WithCorrelation(workflowID)

	if err := deps.Store.Append(ctx, aggregateID, 1, []event.Envelope{completedEvt}); err != nil {
		t.Fatal(err)
	}
}

func TestToolPersonaOutput(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	seedPersonaOutput(t, deps, "po-wf-1", "developer", "This is the AI output text")

	s := NewServer(deps, testLogger())
	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_persona_output",
		Arguments: json.RawMessage(`{"workflow_id":"po-wf-1","persona":"developer"}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var out personaOutputResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &out); err != nil {
		t.Fatal(err)
	}
	if out.Output != "This is the AI output text" {
		t.Errorf("expected output text, got %q", out.Output)
	}
	if out.Truncated {
		t.Error("expected truncated=false")
	}
	if out.Backend != "claude" {
		t.Errorf("expected backend claude, got %s", out.Backend)
	}
	if out.TokensUsed != 2500 {
		t.Errorf("expected 2500 tokens, got %d", out.TokensUsed)
	}
	if out.DurationMS != 8000 {
		t.Errorf("expected 8000ms, got %d", out.DurationMS)
	}
}

func TestToolPersonaOutputTruncation(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	longOutput := strings.Repeat("x", 200)
	seedPersonaOutput(t, deps, "po-trunc-1", "developer", longOutput)

	s := NewServer(deps, testLogger())
	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_persona_output",
		Arguments: json.RawMessage(`{"workflow_id":"po-trunc-1","persona":"developer","max_length":100}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var out personaOutputResult
	_ = json.Unmarshal([]byte(result.Content[0].Text), &out)
	if !out.Truncated {
		t.Error("expected truncated=true")
	}
	if len(out.Output) != 100 {
		t.Errorf("expected output length 100, got %d", len(out.Output))
	}
}

func TestToolPersonaOutputNotFound(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	// Store a PersonaCompleted with no OutputRef — simulates a handler that
	// never called the AI backend.
	ctx := context.Background()
	aggregateID := "po-nf-1:persona:researcher"
	noOutputEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:    "researcher",
		OutputRef:  "", // no output
		DurationMS: 100,
	})).WithAggregate(aggregateID, 1).WithCorrelation("po-nf-1")
	_ = deps.Store.Append(ctx, aggregateID, 0, []event.Envelope{noOutputEvt})

	s := NewServer(deps, testLogger())
	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_persona_output",
		Arguments: json.RawMessage(`{"workflow_id":"po-nf-1","persona":"researcher"}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if !result.IsError {
		t.Error("expected error for persona with no OutputRef")
	}
}

func TestMultipleRequestsSequential(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s,
		sendRequest(1, methodInitialize, initializeParams{
			ProtocolVersion: protocolVersion,
			ClientInfo:      entityInfo{Name: "test"},
		}),
		sendNotification(methodInitialized),
		sendRequest(2, methodPing, nil),
		sendRequest(3, methodToolsList, nil),
	)

	// initialize + ping + tools/list = 3 responses (notification has none)
	if len(lines) != 3 {
		t.Fatalf("expected 3 responses, got %d: %v", len(lines), lines)
	}

	// Verify IDs match
	r1 := parseResponse(t, lines[0])
	r2 := parseResponse(t, lines[1])
	r3 := parseResponse(t, lines[2])

	if string(r1.ID) != "1" {
		t.Errorf("expected id 1, got %s", r1.ID)
	}
	if string(r2.ID) != "2" {
		t.Errorf("expected id 2, got %s", r2.ID)
	}
	if string(r3.ID) != "3" {
		t.Errorf("expected id 3, got %s", r3.ID)
	}
}
