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

// TestInitializeNegotiatesProtocolVersion pins the MCP spec rule (Lifecycle
// §Version Negotiation): when the client requests a version the server
// supports, the server MUST echo that exact version — not a hardcoded older
// one. A modern Claude Code client that asks for 2025-06-18 and gets 2024-11-05
// back treats it as an unrequested downgrade, disconnects, and drops every
// rick_* tool — the silent no-op an operator sees as "the server is dark".
func TestInitializeNegotiatesProtocolVersion(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	cases := []struct {
		name      string
		requested string
		want      string
	}{
		{"current spec version echoed", "2025-06-18", "2025-06-18"},
		{"intermediate version echoed", "2025-03-26", "2025-03-26"},
		{"legacy version echoed", "2024-11-05", "2024-11-05"},
		{"unsupported newer falls back to latest", "9999-99-99", latestProtocolVersion},
		{"empty falls back to latest", "", latestProtocolVersion},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := serveLines(t, s,
				sendRequest(1, methodInitialize, initializeParams{
					ProtocolVersion: tc.requested,
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
			if result.ProtocolVersion != tc.want {
				t.Errorf("requested %q: expected echoed %q, got %q", tc.requested, tc.want, result.ProtocolVersion)
			}
		})
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

	// The MCP surface is deliberately capped at a small set of consolidated
	// facades. This is the regrowth guard: any new top-level tool must be a
	// conscious decision that updates this set, not a silent addition that
	// re-saturates the LLM's tool-selection context.
	wantTools := []string{
		// Workflow
		"rick_run_workflow", "rick_workflow_inspect", "rick_workflow_control",
		// Jobs
		"rick_consult", "rick_run", "rick_job_inspect", "rick_job_cancel",
		// Workspace / wave / diff
		"rick_workspace", "rick_wave_manager", "rick_diff_viewer",
		// Jira
		"rick_jira_read", "rick_jira_write", "rick_jira_search",
		"rick_jira_create", "rick_jira_manage_links",
		// Confluence / observability
		"rick_confluence", "rick_create_pr", "rick_project_sync",
	}

	got := make(map[string]bool, len(result.Tools))
	for _, tool := range result.Tools {
		got[tool.Name] = true
	}

	if len(result.Tools) != len(wantTools) {
		t.Errorf("tool surface drift: got %d tools, want %d", len(result.Tools), len(wantTools))
	}
	for _, name := range wantTools {
		if !got[name] {
			t.Errorf("expected tool %s not found in tools/list", name)
		}
	}

	// Tools folded into facades must NOT be registered standalone anymore.
	removed := []string{
		"rick_list_workflows", "rick_list_events", "rick_list_dead_letters",
		"rick_inject_guidance", "rick_approve_hint", "rick_reject_hint",
		"rick_plan_btu", "rick_search_workflows", "rick_retry_workflow",
		"rick_jobs", "rick_backends", "rick_github_pr_links",
		"rick_workspace_setup", "rick_workspace_cleanup", "rick_workspace_list",
		"rick_confluence_read", "rick_confluence_write",
	}
	for _, name := range removed {
		if got[name] {
			t.Errorf("tool %s should be folded into a facade, not registered standalone", name)
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

	// Verify the stored WorkflowRequested event has Isolate=true for code-producing DAGs.
	evts, err := deps.Store.Load(context.Background(), run.WorkflowID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	for _, e := range evts {
		if e.Type == event.WorkflowRequested {
			var payload event.WorkflowRequestedPayload
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			if !payload.Isolate {
				t.Error("workspace-dev DAG must set Isolate=true in WorkflowRequestedPayload")
			}
		}
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

// TestToolRunWorkflowDerivesRepoFromSource verifies that when source is
// "gh:owner/repo#N" but repo is not provided, the MCP tool derives repo from
// source. Regression: without this, the workspace handler sees an empty Repo
// and skips provisioning — causing duplicate PRs from the wrong branch.
func TestToolRunWorkflowDerivesRepoFromSource(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	// Register pr-feedback workflow so the DAG is available.
	deps.Engine.RegisterWorkflow(engine.PRFeedbackWorkflowDef())
	deps.SelectWorkflow = func(name string) (engine.WorkflowDef, error) {
		if name == "pr-feedback" {
			return engine.PRFeedbackWorkflowDef(), nil
		}
		return engine.WorkflowDef{}, fmt.Errorf("unknown workflow: %s", name)
	}

	s := NewServer(deps, testLogger())

	// Source is explicitly provided with gh:owner/repo#N format, but repo is NOT provided.
	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_run_workflow",
		Arguments: json.RawMessage(`{"prompt":"fix PR feedback","dag":"pr-feedback","source":"gh:hulilabs/ehr#830"}`),
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

	var run runWorkflowResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &run); err != nil {
		t.Fatal(err)
	}

	// Verify the stored WorkflowRequested event has Repo derived from source.
	evts, err := deps.Store.Load(context.Background(), run.WorkflowID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	for _, e := range evts {
		if e.Type == event.WorkflowRequested {
			var payload event.WorkflowRequestedPayload
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			if payload.Repo != "hulilabs/ehr" {
				t.Errorf("expected Repo=hulilabs/ehr derived from source, got %q", payload.Repo)
			}
			if !payload.Isolate {
				t.Error("pr-feedback DAG must set Isolate=true")
			}
		}
	}
}

// TestToolRunWorkflowDefaultsGithubDevForIssueSource locks in the auto-select
// rule for GitHub issue references: when source is "gh:owner/repo#N" and no
// dag is provided (and no pr_number forces a PR flow), rick_run_workflow
// must default to github-dev. Regression: previously fell through to
// workspace-dev, which lacks github-context and produced rick/<corr8>
// branches instead of issue-<N> branches.
func TestToolRunWorkflowDefaultsGithubDevForIssueSource(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	deps.Engine.RegisterWorkflow(engine.GithubDevWorkflowDef())
	deps.SelectWorkflow = func(name string) (engine.WorkflowDef, error) {
		if name == "github-dev" {
			return engine.GithubDevWorkflowDef(), nil
		}
		return engine.WorkflowDef{}, fmt.Errorf("unknown workflow: %s", name)
	}

	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_run_workflow",
		Arguments: json.RawMessage(`{"prompt":"Implement issue","source":"gh:hulilabs/huli#641"}`),
	}))

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
	var run runWorkflowResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &run); err != nil {
		t.Fatal(err)
	}
	if run.DAG != "github-dev" {
		t.Errorf("expected dag github-dev for gh:* issue source, got %q", run.DAG)
	}
}

// TestToolRunWorkflowKeepsWorkspaceDevForPRSource guards the pr_number
// escape hatch: when the caller passes a PR number (intending a PR flow),
// we must NOT silently pick github-dev — that handler would reject the PR
// reference at runtime. The caller is expected to provide an explicit DAG
// (pr-review / pr-feedback) for PR work; absent that, workspace-dev stays
// the default so the failure mode is "missing DAG" not "wrong DAG".
func TestToolRunWorkflowKeepsWorkspaceDevForPRSource(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_run_workflow",
		Arguments: json.RawMessage(`{"prompt":"review pr","source":"gh:hulilabs/huli#820","pr_number":820,"repo":"hulilabs/huli"}`),
	}))

	resp := parseResponse(t, lines[0])
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	// workspace-dev is registered via testDeps; should succeed with default.
	if result.IsError {
		t.Fatalf("tool returned error: %s", result.Content[0].Text)
	}
	var run runWorkflowResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &run); err != nil {
		t.Fatal(err)
	}
	if run.DAG != "workspace-dev" {
		t.Errorf("expected dag workspace-dev when pr_number is set, got %q", run.DAG)
	}
}

func TestToolWorkflowInspect_Status(t *testing.T) {
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
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s"}`, aggregateID)),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var inspect struct {
		Status workflowStatusResult `json:"status"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &inspect); err != nil {
		t.Fatal(err)
	}
	status := inspect.Status
	if status.Status != "requested" {
		t.Errorf("expected status requested, got %s", status.Status)
	}
	if status.WorkflowID != "workspace-dev" {
		t.Errorf("expected workflow_id workspace-dev, got %s", status.WorkflowID)
	}
}

// TestToolWorkflowStatus_SurfacesFailureDetail locks the 2026-04-20
// docs-only silent-stall fix: when a workflow ends in status=="failed",
// rick_workflow_status must return a structured failure block carrying
// FailureKind, Backend, and Stderr — not just status:"failed" with no
// cause visible. The operator in the report had to grep the raw event
// stream to see what went wrong; this test guarantees the actionable
// signal is now one MCP call away.
func TestToolWorkflowStatus_SurfacesFailureDetail(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	aggregateID := "wf-failure-detail"
	ctx := context.Background()

	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "docs-only task", WorkflowID: "workspace-dev", Source: "test",
	})).WithAggregate(aggregateID, 1).WithCorrelation(aggregateID).WithSource("test")
	startEvt := event.New(event.WorkflowStartedFor("workspace-dev"), 1,
		event.MustMarshal(event.WorkflowStartedPayload{WorkflowID: "workspace-dev"})).
		WithAggregate(aggregateID, 2).WithCorrelation(aggregateID).WithSource("engine")
	// WorkflowFailed with the new diagnostic fields populated.
	failEvt := event.New(event.WorkflowFailed, 1, event.MustMarshal(event.WorkflowFailedPayload{
		Reason:      "persona developer failed: handler developer: backend: claude: backend: idle timeout exceeded (stall=2m0s)",
		Persona:     "developer",
		FailureKind: event.FailureKindIdleTimeout,
		Backend:     "claude",
		Stderr:      "YOLO mode is enabled. All tool calls will be automatically approved.",
	})).WithAggregate(aggregateID, 3).WithCorrelation(aggregateID).WithSource("engine:aggregate")

	if err := deps.Store.Append(ctx, aggregateID, 0, []event.Envelope{reqEvt, startEvt, failEvt}); err != nil {
		t.Fatal(err)
	}

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s"}`, aggregateID)),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var inspect struct {
		Status workflowStatusResult `json:"status"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &inspect); err != nil {
		t.Fatal(err)
	}
	status := inspect.Status
	if status.Status != "failed" {
		t.Fatalf("status = %q; want failed", status.Status)
	}
	if status.Failure == nil {
		t.Fatal("Failure block missing — rick_workflow_status must surface failure detail so operators don't have to grep raw events")
	}
	if status.Failure.FailureKind != "idle_timeout" {
		t.Errorf("Failure.FailureKind = %q; want idle_timeout", status.Failure.FailureKind)
	}
	if status.Failure.Backend != "claude" {
		t.Errorf("Failure.Backend = %q; want claude", status.Failure.Backend)
	}
	if status.Failure.Phase != "developer" {
		t.Errorf("Failure.Phase = %q; want developer", status.Failure.Phase)
	}
	if status.Failure.Stderr == "" {
		t.Error("Failure.Stderr empty — the captured tail must reach the MCP caller")
	}
	if status.Failure.Reason == "" {
		t.Error("Failure.Reason empty — human-readable summary must be present")
	}
}

func TestToolWorkflowStatusNotFound(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(`{"workflow_id":"nonexistent"}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	// rick_workflow_inspect reports a missing workflow under
	// result["errors"]["status"] rather than as a tool-level error.
	if result.IsError {
		t.Fatalf("inspect should not be a tool error, got: %s", result.Content[0].Text)
	}
	var inspect struct {
		Errors map[string]string `json:"errors"`
	}
	_ = json.Unmarshal([]byte(result.Content[0].Text), &inspect)
	if inspect.Errors["status"] == "" {
		t.Error("expected status panel error for nonexistent workflow")
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
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(`{"include":["list"]}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var inspect struct {
		List listWorkflowsResult `json:"list"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &inspect); err != nil {
		t.Fatal(err)
	}
	list := inspect.List
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
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s","include":["events"]}`, aggregateID)),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var inspect struct {
		Events listEventsResult `json:"events"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &inspect); err != nil {
		t.Fatal(err)
	}
	if inspect.Events.Count != 2 {
		t.Errorf("expected 2 events, got %d", inspect.Events.Count)
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
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(`{"include":["events"],"limit":10}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var inspect struct {
		Events listEventsResult `json:"events"`
	}
	_ = json.Unmarshal([]byte(result.Content[0].Text), &inspect)
	if inspect.Events.Count != 2 {
		t.Errorf("expected 2 events from global stream, got %d", inspect.Events.Count)
	}
}

func TestToolTokenUsage(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	// Seed token usage
	aiEvt := event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
		Persona: "researcher", Backend: "claude", TokensUsed: 1500, DurationMS: 3000,
	})).WithAggregate("tk-wf-1", 1).WithCorrelation("c-1")
	_ = deps.Tokens.Handle(context.Background(), aiEvt)

	s := NewServer(deps, testLogger())
	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(`{"workflow_id":"tk-wf-1","include":["tokens"]}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var inspect struct {
		Tokens tokenUsageResult `json:"tokens"`
	}
	_ = json.Unmarshal([]byte(result.Content[0].Text), &inspect)
	usage := inspect.Tokens
	if usage.Total != 1500 {
		t.Errorf("expected 1500 tokens, got %d", usage.Total)
	}
	if usage.ByPhase["researcher"] != 1500 {
		t.Errorf("expected 1500 for researcher, got %d", usage.ByPhase["researcher"])
	}
}

func TestToolTokenUsageNotTracked(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())

	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(`{"workflow_id":"nonexistent","include":["tokens"]}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Error("expected success with zero usage for unknown workflow")
	}

	var inspect struct {
		Tokens tokenUsageResult `json:"tokens"`
	}
	_ = json.Unmarshal([]byte(result.Content[0].Text), &inspect)
	usage := inspect.Tokens
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
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(`{"workflow_id":"tl-wf-1","include":["timeline"]}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var inspect struct {
		Timeline phaseTimelineResult `json:"timeline"`
	}
	_ = json.Unmarshal([]byte(result.Content[0].Text), &inspect)
	timeline := inspect.Timeline
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

// TestToolPhaseTimeline_SurfacesFailureDiagnostics verifies that the
// phase_timeline tool exposes the new failure_kind/error/stderr fields
// when a persona failed. Complements the projection-level test — this
// checks the MCP serialization path end-to-end.
func TestToolPhaseTimeline_SurfacesFailureDiagnostics(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	failedEvt := event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
		Persona:     "developer",
		Error:       "claude: backend: idle timeout exceeded (stall=2m0s) (after 2m0s)",
		FailureKind: event.FailureKindIdleTimeout,
		Stderr:      "last gasps from the subprocess",
		DurationMS:  180_000,
	})).WithAggregate("tl-failed:persona:developer", 1).WithCorrelation("tl-failed")

	_ = deps.Timelines.Handle(context.Background(), failedEvt)

	s := NewServer(deps, testLogger())
	lines := serveLines(t, s, sendRequest(1, methodToolsCall, toolsCallParams{
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(`{"workflow_id":"tl-failed","include":["timeline"]}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var inspect struct {
		Timeline phaseTimelineResult `json:"timeline"`
	}
	_ = json.Unmarshal([]byte(result.Content[0].Text), &inspect)
	timeline := inspect.Timeline
	if len(timeline.Phases) != 1 {
		t.Fatalf("expected 1 phase, got %d", len(timeline.Phases))
	}
	phase := timeline.Phases[0]
	if phase.Status != "failed" {
		t.Errorf("Status = %q; want failed", phase.Status)
	}
	if phase.FailureKind != string(event.FailureKindIdleTimeout) {
		t.Errorf("FailureKind = %q; want idle_timeout", phase.FailureKind)
	}
	if !strings.Contains(phase.Error, "idle timeout") {
		t.Errorf("Error missing root cause: %q", phase.Error)
	}
	if !strings.Contains(phase.Stderr, "last gasps") {
		t.Errorf("Stderr not surfaced to MCP response: %q", phase.Stderr)
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
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(`{"include":["dead_letters"]}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var inspect struct {
		DeadLetters listDeadLettersResult `json:"dead_letters"`
	}
	_ = json.Unmarshal([]byte(result.Content[0].Text), &inspect)
	dls := inspect.DeadLetters
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
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(`{"include":["dead_letters"]}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var inspect struct {
		DeadLetters listDeadLettersResult `json:"dead_letters"`
	}
	_ = json.Unmarshal([]byte(result.Content[0].Text), &inspect)
	if inspect.DeadLetters.Count != 0 {
		t.Errorf("expected 0 dead letters, got %d", inspect.DeadLetters.Count)
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
		Name:      "rick_workflow_control",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s","action":"cancel","reason":"test cancel"}`, aggregateID)),
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
		Name:      "rick_workflow_control",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s","action":"cancel"}`, aggregateID)),
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
		Name:      "rick_workflow_control",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s","action":"pause","reason":"investigating"}`, aggregateID)),
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
		Name:      "rick_workflow_control",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s","action":"resume","reason":"fixed"}`, aggregateID)),
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
		Name:      "rick_workflow_control",
		Arguments: json.RawMessage(fmt.Sprintf(`{"action":"inject_guidance","workflow_id":"%s","content":"use sql.NullString","auto_resume":true}`, aggregateID)),
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
		Persona:     "developer",
		SourcePersona: "reviewer",
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
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(`{"workflow_id":"vd-wf-1","include":["verdicts"]}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var inspect struct {
		Verdicts workflowVerdictsResult `json:"verdicts"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &inspect); err != nil {
		t.Fatal(err)
	}
	vr := inspect.Verdicts
	if vr.Count != 1 {
		t.Fatalf("expected 1 verdict, got %d", vr.Count)
	}
	if vr.Verdicts[0].Outcome != "fail" {
		t.Errorf("expected outcome fail, got %s", vr.Verdicts[0].Outcome)
	}
	if vr.Verdicts[0].Phase != "developer" {
		t.Errorf("expected phase developer, got %s", vr.Verdicts[0].Phase)
	}
	if vr.Verdicts[0].SourcePhase != "reviewer" {
		t.Errorf("expected source_phase reviewer, got %s", vr.Verdicts[0].SourcePhase)
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
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(`{"workflow_id":"unknown-wf","include":["verdicts"]}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("expected success for unknown workflow, got error: %s", result.Content[0].Text)
	}

	var inspect struct {
		Verdicts workflowVerdictsResult `json:"verdicts"`
	}
	_ = json.Unmarshal([]byte(result.Content[0].Text), &inspect)
	vr := inspect.Verdicts
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
		Persona:    persona,
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
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(`{"workflow_id":"po-wf-1","include":["persona_output"],"persona":"developer"}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var inspect struct {
		PersonaOutput personaOutputResult `json:"persona_output"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &inspect); err != nil {
		t.Fatal(err)
	}
	out := inspect.PersonaOutput
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
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(`{"workflow_id":"po-trunc-1","include":["persona_output"],"persona":"developer","max_length":100}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var inspect struct {
		PersonaOutput personaOutputResult `json:"persona_output"`
	}
	_ = json.Unmarshal([]byte(result.Content[0].Text), &inspect)
	out := inspect.PersonaOutput
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
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(`{"workflow_id":"po-nf-1","include":["persona_output"],"persona":"researcher"}`),
	}))

	resp := parseResponse(t, lines[0])
	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	// rick_workflow_inspect surfaces a per-panel failure (persona with no
	// OutputRef) under result["errors"]["persona_output"] instead of a
	// tool-level error.
	if result.IsError {
		t.Fatalf("inspect should not be a tool error, got: %s", result.Content[0].Text)
	}
	var inspect struct {
		Errors map[string]string `json:"errors"`
	}
	_ = json.Unmarshal([]byte(result.Content[0].Text), &inspect)
	if inspect.Errors["persona_output"] == "" {
		t.Error("expected persona_output panel error for persona with no OutputRef")
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
