package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// --- Query Types ---

// WorkflowSummary mirrors the rick_list_workflows tool response.
type WorkflowSummary struct {
	AggregateID string `json:"aggregate_id"`
	WorkflowID  string `json:"workflow_id"`
	Status      string `json:"status"`
	FailReason  string `json:"fail_reason,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
}

// EventEntry mirrors the rick_list_events tool response.
type EventEntry struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Version       int    `json:"version"`
	Timestamp     string `json:"timestamp"`
	Source        string `json:"source"`
	CorrelationID string `json:"correlation_id,omitempty"`
	AggregateID   string `json:"aggregate_id,omitempty"`
}

// PendingHint represents a hint awaiting operator review.
type PendingHint struct {
	Persona       string   `json:"persona"`
	Confidence    float64  `json:"confidence"`
	Plan          string   `json:"plan"`
	Blockers      []string `json:"blockers,omitempty"`
	TokenEstimate int      `json:"token_estimate"`
	EventID       string   `json:"event_id"`
}

// WorkflowDetail mirrors the rick_workflow_status tool response.
type WorkflowDetail struct {
	ID                string          `json:"id"`
	Status            string          `json:"status"`
	WorkflowID        string          `json:"workflow_id"`
	Version           int             `json:"version"`
	TokensUsed        int             `json:"tokens_used"`
	CompletedPersonas map[string]bool `json:"completed_personas"`
	FeedbackCount     map[string]int  `json:"feedback_count"`
	PendingHints      []PendingHint   `json:"pending_hints,omitempty"`
}

// PhaseEntry mirrors the rick_phase_timeline tool response.
type PhaseEntry struct {
	Phase       string `json:"phase"`
	Status      string `json:"status"`
	Iterations  int    `json:"iterations"`
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
}

// TokenUsage mirrors the rick_token_usage tool response.
type TokenUsage struct {
	WorkflowID string         `json:"workflow_id"`
	Total      int            `json:"total"`
	ByPhase    map[string]int `json:"by_phase"`
	ByBackend  map[string]int `json:"by_backend"`
}

// DeadLetterEntry mirrors the rick_list_dead_letters tool response.
type DeadLetterEntry struct {
	ID       string `json:"id"`
	EventID  string `json:"event_id"`
	Handler  string `json:"handler"`
	Error    string `json:"error"`
	Attempts int    `json:"attempts"`
	FailedAt string `json:"failed_at"`
}

// ActionResult is returned by operator intervention methods.
type ActionResult struct {
	WorkflowID string `json:"workflow_id"`
	Action     string `json:"action"`
	Status     string `json:"status"`
	Resumed    bool   `json:"resumed,omitempty"`
}

// VerdictIssue represents a single finding from a review verdict.
type VerdictIssue struct {
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	Description string `json:"description"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
}

// VerdictRecord represents a single review verdict.
type VerdictRecord struct {
	Phase       string         `json:"phase"`
	SourcePhase string         `json:"source_phase"`
	Outcome     string         `json:"outcome"`
	Summary     string         `json:"summary"`
	Issues      []VerdictIssue `json:"issues,omitempty"`
}

// PersonaOutput holds the AI response output for a persona.
type PersonaOutput struct {
	WorkflowID string `json:"workflow_id"`
	Persona    string `json:"persona"`
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated"`
	Backend    string `json:"backend"`
	TokensUsed int    `json:"tokens_used"`
	DurationMS int64  `json:"duration_ms"`
}

// --- Wails-bound Query Methods ---

// ListWorkflows returns all tracked workflows.
func (a *App) ListWorkflows() ([]WorkflowSummary, error) {
	raw, err := a.mcpClient.CallTool(a.ctx, "rick_list_workflows", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}

	var result struct {
		Workflows []WorkflowSummary `json:"workflows"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("list workflows: unmarshal: %w", err)
	}
	return result.Workflows, nil
}

// ListEvents returns events, optionally filtered by workflow ID.
func (a *App) ListEvents(workflowID string, limit int) ([]EventEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	args := map[string]any{"limit": limit}
	if workflowID != "" {
		args["workflow_id"] = workflowID
	}

	raw, err := a.mcpClient.CallTool(a.ctx, "rick_list_events", args)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}

	var result struct {
		Events []EventEntry `json:"events"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("list events: unmarshal: %w", err)
	}
	return result.Events, nil
}

// WorkflowStatus returns detailed status for a single workflow.
func (a *App) WorkflowStatus(workflowID string) (*WorkflowDetail, error) {
	raw, err := a.mcpClient.CallTool(a.ctx, "rick_workflow_status", map[string]any{
		"workflow_id": workflowID,
	})
	if err != nil {
		return nil, fmt.Errorf("workflow status: %w", err)
	}

	var detail WorkflowDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil, fmt.Errorf("workflow status: unmarshal: %w", err)
	}
	return &detail, nil
}

// PhaseTimeline returns phase timing for a workflow.
func (a *App) PhaseTimeline(workflowID string) ([]PhaseEntry, error) {
	raw, err := a.mcpClient.CallTool(a.ctx, "rick_phase_timeline", map[string]any{
		"workflow_id": workflowID,
	})
	if err != nil {
		return nil, fmt.Errorf("phase timeline: %w", err)
	}

	var result struct {
		Phases []PhaseEntry `json:"phases"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("phase timeline: unmarshal: %w", err)
	}
	return result.Phases, nil
}

// TokenUsageForWorkflow returns token consumption for a workflow.
func (a *App) TokenUsageForWorkflow(workflowID string) (*TokenUsage, error) {
	raw, err := a.mcpClient.CallTool(a.ctx, "rick_token_usage", map[string]any{
		"workflow_id": workflowID,
	})
	if err != nil {
		return nil, fmt.Errorf("token usage: %w", err)
	}

	var usage TokenUsage
	if err := json.Unmarshal(raw, &usage); err != nil {
		return nil, fmt.Errorf("token usage: unmarshal: %w", err)
	}
	return &usage, nil
}

// ListDeadLetters returns all dead letter entries.
func (a *App) ListDeadLetters() ([]DeadLetterEntry, error) {
	raw, err := a.mcpClient.CallTool(a.ctx, "rick_list_dead_letters", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("list dead letters: %w", err)
	}

	var result struct {
		DeadLetters []DeadLetterEntry `json:"dead_letters"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("list dead letters: unmarshal: %w", err)
	}
	return result.DeadLetters, nil
}

// --- Wails-bound Action Methods ---

// PauseWorkflow pauses a running workflow.
func (a *App) PauseWorkflow(workflowID string, reason string) (*ActionResult, error) {
	return a.intervention(a.ctx, "rick_pause_workflow", workflowID, reason, "paused")
}

// CancelWorkflow cancels a running workflow.
func (a *App) CancelWorkflow(workflowID string, reason string) (*ActionResult, error) {
	return a.intervention(a.ctx, "rick_cancel_workflow", workflowID, reason, "cancelled")
}

// ResumeWorkflow resumes a paused workflow.
func (a *App) ResumeWorkflow(workflowID string, reason string) (*ActionResult, error) {
	return a.intervention(a.ctx, "rick_resume_workflow", workflowID, reason, "resumed")
}

func (a *App) intervention(ctx context.Context, tool, workflowID, reason, action string) (*ActionResult, error) {
	args := map[string]any{
		"workflow_id": workflowID,
	}
	if reason != "" {
		args["reason"] = reason
	}

	raw, err := a.mcpClient.CallTool(ctx, tool, args)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}

	var result ActionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("%s: unmarshal: %w", action, err)
	}
	return &result, nil
}

// ApproveHint approves a pending hint, triggering full persona execution.
func (a *App) ApproveHint(workflowID string, persona string, guidance string) (*ActionResult, error) {
	args := map[string]any{
		"workflow_id": workflowID,
		"persona":     persona,
	}
	if guidance != "" {
		args["guidance"] = guidance
	}

	raw, err := a.mcpClient.CallTool(a.ctx, "rick_approve_hint", args)
	if err != nil {
		return nil, fmt.Errorf("approve hint: %w", err)
	}

	var result ActionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("approve hint: unmarshal: %w", err)
	}
	return &result, nil
}

// RejectHint rejects a pending hint. Action: "skip" (mark complete) or "fail" (fail workflow).
func (a *App) RejectHint(workflowID string, persona string, reason string, action string) (*ActionResult, error) {
	args := map[string]any{
		"workflow_id": workflowID,
		"persona":     persona,
	}
	if reason != "" {
		args["reason"] = reason
	}
	if action != "" {
		args["action"] = action
	}

	raw, err := a.mcpClient.CallTool(a.ctx, "rick_reject_hint", args)
	if err != nil {
		return nil, fmt.Errorf("reject hint: %w", err)
	}

	var result ActionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("reject hint: unmarshal: %w", err)
	}
	return &result, nil
}

// WorkflowVerdicts returns review verdicts for a workflow.
func (a *App) WorkflowVerdicts(workflowID string) ([]VerdictRecord, error) {
	raw, err := a.mcpClient.CallTool(a.ctx, "rick_workflow_verdicts", map[string]any{
		"workflow_id": workflowID,
	})
	if err != nil {
		return nil, fmt.Errorf("workflow verdicts: %w", err)
	}

	var result struct {
		Verdicts []VerdictRecord `json:"verdicts"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("workflow verdicts: unmarshal: %w", err)
	}
	return result.Verdicts, nil
}

// PersonaOutput returns the AI response output for a specific persona.
func (a *App) PersonaOutput(workflowID, persona string) (*PersonaOutput, error) {
	raw, err := a.mcpClient.CallTool(a.ctx, "rick_persona_output", map[string]any{
		"workflow_id": workflowID,
		"persona":     persona,
	})
	if err != nil {
		return nil, fmt.Errorf("persona output: %w", err)
	}

	var output PersonaOutput
	if err := json.Unmarshal(raw, &output); err != nil {
		return nil, fmt.Errorf("persona output: unmarshal: %w", err)
	}
	return &output, nil
}

// InjectGuidance injects operator guidance into a workflow.
func (a *App) InjectGuidance(workflowID string, content string, target string) (*ActionResult, error) {
	args := map[string]any{
		"workflow_id": workflowID,
		"content":     content,
		"auto_resume": true,
	}
	if target != "" {
		args["target"] = target
	}

	raw, err := a.mcpClient.CallTool(a.ctx, "rick_inject_guidance", args)
	if err != nil {
		return nil, fmt.Errorf("inject guidance: %w", err)
	}

	var result ActionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("inject guidance: unmarshal: %w", err)
	}
	return &result, nil
}
