package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
)

// seedWorkflow starts a workflow via rick_run_workflow and waits for the engine
// to transition it to WorkflowStarted. Returns the workflow ID.
func seedWorkflow(t *testing.T, s *Server, prompt, dag string) string {
	t.Helper()

	result, err := callTool(t, s, "rick_run_workflow", map[string]any{
		"prompt": prompt,
		"dag":    dag,
	})
	if err != nil {
		t.Fatalf("rick_run_workflow: %v", err)
	}
	wf := result.(runWorkflowResult)
	return wf.WorkflowID
}

// waitForWorkflowStatus polls the workflow aggregate until it reaches the target
// status or times out.
func waitForWorkflowStatus(t *testing.T, s *Server, workflowID string, want engine.WorkflowStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		result, err := callTool(t, s, "rick_workflow_status", map[string]any{
			"workflow_id": workflowID,
		})
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if status, ok := result.(workflowStatusResult); ok {
			if status.Status == string(want) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- toolRetryWorkflow tests ---

func TestToolRetryWorkflow_FailedWorkflow(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	// Start the engine so the workflow goes through lifecycle.
	deps.Engine.Start()
	defer deps.Engine.Stop()

	s := NewServer(deps, testLogger())
	defer s.Close()

	wfID := seedWorkflow(t, s, "build a service", "workspace-dev")

	// Wait for the workflow to start.
	waitForWorkflowStatus(t, s, wfID, engine.StatusRunning, 2*time.Second)

	// Manually append a WorkflowFailed event to make it retryable.
	ctx := context.Background()
	events, err := deps.Store.Load(ctx, wfID)
	if err != nil || len(events) == 0 {
		t.Fatal("could not load workflow events")
	}

	failEvt := event.New(event.WorkflowFailed, 1, event.MustMarshal(event.WorkflowFailedPayload{
		Reason: "test failure",
	})).
		WithAggregate(wfID, len(events)+1).
		WithCorrelation(wfID).
		WithSource("test")

	if appendErr := deps.Store.Append(ctx, wfID, len(events), []event.Envelope{failEvt}); appendErr != nil {
		t.Fatalf("append fail event: %v", appendErr)
	}

	// Now retry the failed workflow.
	result, err := callTool(t, s, "rick_retry_workflow", map[string]any{
		"workflow_id": wfID,
	})
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}

	rm, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if rm["original_workflow_id"] != wfID {
		t.Errorf("expected original_workflow_id=%s, got %v", wfID, rm["original_workflow_id"])
	}
	if rm["status"] != "started" {
		t.Errorf("expected status=started, got %v", rm["status"])
	}
	retryID, _ := rm["retry_workflow_id"].(string)
	if retryID == "" || retryID == wfID {
		t.Error("expected a new workflow ID for retry")
	}
}

func TestToolRetryWorkflow_CancelledWorkflow(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Engine.Start()
	defer deps.Engine.Stop()

	s := NewServer(deps, testLogger())
	defer s.Close()

	wfID := seedWorkflow(t, s, "implement a feature", "workspace-dev")
	waitForWorkflowStatus(t, s, wfID, engine.StatusRunning, 2*time.Second)

	// Cancel the workflow first.
	_, err := callTool(t, s, "rick_cancel_workflow", map[string]any{
		"workflow_id": wfID,
		"reason":      "testing retry from cancelled",
	})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Retry the cancelled workflow.
	result, err := callTool(t, s, "rick_retry_workflow", map[string]any{
		"workflow_id": wfID,
	})
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}

	rm := result.(map[string]any)
	if rm["status"] != "started" {
		t.Errorf("expected status=started, got %v", rm["status"])
	}
}

func TestToolRetryWorkflow_RunningWorkflow(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Engine.Start()
	defer deps.Engine.Stop()

	s := NewServer(deps, testLogger())
	defer s.Close()

	wfID := seedWorkflow(t, s, "task", "workspace-dev")
	waitForWorkflowStatus(t, s, wfID, engine.StatusRunning, 2*time.Second)

	// Retry a running workflow should fail.
	_, err := callTool(t, s, "rick_retry_workflow", map[string]any{
		"workflow_id": wfID,
	})
	if err == nil {
		t.Fatal("expected error when retrying a running workflow")
	}
	if !strings.Contains(err.Error(), "can only retry") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- toolWorkflowOutput tests ---

func TestToolWorkflowOutput_EmptyWorkflow(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()

	s := NewServer(deps, testLogger())
	defer s.Close()

	wfID := seedWorkflow(t, s, "test task", "workspace-dev")

	// Workflow just started — no persona outputs yet.
	result, err := callTool(t, s, "rick_workflow_output", map[string]any{
		"workflow_id": wfID,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if rm["workflow_id"] != wfID {
		t.Errorf("expected workflow_id=%s, got %v", wfID, rm["workflow_id"])
	}
	count, _ := rm["count"].(int)
	if count != 0 {
		t.Errorf("expected count=0 for new workflow, got %d", count)
	}
}

// --- toolWorkflowVerdicts tests ---

func TestToolWorkflowVerdicts_NewWorkflow(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	wfID := seedWorkflow(t, s, "write code", "workspace-dev")

	result, err := callTool(t, s, "rick_workflow_verdicts", map[string]any{
		"workflow_id": wfID,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return empty verdicts (workflow just started).
	// The result type varies — just verify it doesn't error.
	_ = result
}

// --- toolPersonaOutput tests ---

func TestToolPersonaOutput_NotFound(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	// Ask for output for a persona that hasn't run yet.
	_, err := callTool(t, s, "rick_persona_output", map[string]any{
		"workflow_id": "nonexistent-wf",
		"persona":     "developer",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent workflow")
	}
}

func TestToolPersonaOutput_MissingArgs(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_persona_output", map[string]any{
		"workflow_id": "some-id",
	})
	if err == nil {
		t.Fatal("expected error for missing persona")
	}
}

// --- toolDiff tests ---

func TestToolDiff_MissingID(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_diff", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing workflow_id")
	}
}

func TestToolDiff_NoWorkspace(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	wfID := seedWorkflow(t, s, "test", "workspace-dev")

	// Workflow doesn't have a workspace — should fail.
	_, err := callTool(t, s, "rick_diff", map[string]any{
		"workflow_id": wfID,
	})
	if err == nil {
		t.Fatal("expected error for workflow without workspace")
	}
	if !strings.Contains(err.Error(), "no workspace found") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- toolCreatePR tests ---

func TestToolCreatePR_MissingID(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_create_pr", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing workflow_id")
	}
}

func TestToolCreatePR_NoWorkspace(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	wfID := seedWorkflow(t, s, "test", "workspace-dev")

	_, err := callTool(t, s, "rick_create_pr", map[string]any{
		"workflow_id": wfID,
	})
	if err == nil {
		t.Fatal("expected error for workflow without workspace")
	}
	if !strings.Contains(err.Error(), "no workspace found") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- toolProjectSync tests ---

func TestToolProjectSync_MissingEpic(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_project_sync", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing epic")
	}
}

func TestToolProjectSync_NoJira(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_project_sync", map[string]any{
		"epic": "PROJ-EPIC",
	})
	if err == nil {
		t.Fatal("expected error when Jira not configured")
	}
	if !strings.Contains(err.Error(), "Jira client not configured") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- toolWaveCleanup tests ---

func TestToolWaveCleanup_MissingEpic(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_wave_cleanup", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing epic")
	}
	if !strings.Contains(err.Error(), "epic is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolWaveCleanup_NoHulipathNoJira(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	// wave_cleanup needs both Jira (for computeWavePlan) and RICK_REPOS_PATH (for cleanup).
	// Without Jira, should fail on Jira check first.
	_, err := callTool(t, s, "rick_wave_cleanup", map[string]any{
		"epic": "PROJ-EPIC",
	})
	if err == nil {
		t.Fatal("expected error when Jira not configured")
	}
	if !strings.Contains(err.Error(), "Jira client not configured") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- Invalid JSON args ---

func TestTools_InvalidJSON(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	tools := []string{
		"rick_run_workflow",
		"rick_workflow_status",
		"rick_cancel_workflow",
		"rick_pause_workflow",
		"rick_resume_workflow",
		"rick_inject_guidance",
		"rick_jira_read",
		"rick_wave_plan",
		"rick_workspace_cleanup",
	}

	for _, toolName := range tools {
		t.Run(toolName, func(t *testing.T) {
			tool := s.tools[toolName]
			if tool.Handler == nil {
				t.Fatalf("tool %s not registered", toolName)
			}
			_, err := tool.Handler(context.Background(), json.RawMessage(`{bad json`))
			if err == nil {
				t.Errorf("expected error for invalid JSON in %s", toolName)
			}
		})
	}
}
