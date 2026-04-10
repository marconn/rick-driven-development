package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/persona"
)

// ---------------------------------------------------------------------------
// PRConsolidatorHandler construction
// ---------------------------------------------------------------------------

func TestNewPRConsolidator(t *testing.T) {
	h := NewPRConsolidator(testDeps())
	if h.Name() != "pr-consolidator" {
		t.Errorf("want name 'pr-consolidator', got %q", h.Name())
	}

	// DAG-based dispatch — Subscribes returns nil.
	subs := h.Subscribes()
	if subs != nil {
		t.Errorf("want nil Subscribes (DAG-based dispatch), got %v", subs)
	}
}

// ---------------------------------------------------------------------------
// extractConsolidatorInputs
// ---------------------------------------------------------------------------

func TestExtractConsolidatorInputs(t *testing.T) {
	corrID := "corr-consolidate-1"

	securityOutput, _ := json.Marshal("Security review output")
	testingOutput, _ := json.Marshal("Testing review output")
	perfOutput, _ := json.Marshal("Performance review output")

	events := []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt:     "Review this PR",
			WorkflowID: "pr-review",
			Source:     "gh:owner/repo#42",
		})).WithCorrelation(corrID),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:   "pr-category-review",
			Backend: "claude",
			Output:  json.RawMessage(securityOutput),
		})).WithCorrelation(corrID).WithSource("handler:pr-security"),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:   "pr-category-review",
			Backend: "claude",
			Output:  json.RawMessage(testingOutput),
		})).WithCorrelation(corrID).WithSource("handler:pr-testing"),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:   "pr-category-review",
			Backend: "claude",
			Output:  json.RawMessage(perfOutput),
		})).WithCorrelation(corrID).WithSource("handler:pr-performance"),
	}

	params, handlerOutputs := extractConsolidatorInputs(events)

	if params.Source != "gh:owner/repo#42" {
		t.Errorf("params.Source: want 'gh:owner/repo#42', got %q", params.Source)
	}
	if params.Prompt != "Review this PR" {
		t.Errorf("params.Prompt: want 'Review this PR', got %q", params.Prompt)
	}
	if handlerOutputs["pr-security"] != "Security review output" {
		t.Errorf("pr-security output: want 'Security review output', got %q", handlerOutputs["pr-security"])
	}
	if handlerOutputs["pr-testing"] != "Testing review output" {
		t.Errorf("pr-testing output: want 'Testing review output', got %q", handlerOutputs["pr-testing"])
	}
	if handlerOutputs["pr-performance"] != "Performance review output" {
		t.Errorf("pr-performance output: want 'Performance review output', got %q", handlerOutputs["pr-performance"])
	}
}

func TestExtractConsolidatorInputsFallbackToPhase(t *testing.T) {
	// Events without Source fall back to Phase as key.
	output, _ := json.Marshal("fallback output")
	events := []event.Envelope{
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:   "review",
			Backend: "claude",
			Output:  json.RawMessage(output),
		})),
	}

	_, handlerOutputs := extractConsolidatorInputs(events)
	if handlerOutputs["review"] != "fallback output" {
		t.Errorf("fallback: want 'fallback output', got %q", handlerOutputs["review"])
	}
}

// ---------------------------------------------------------------------------
// buildConsolidationPrompt
// ---------------------------------------------------------------------------

func TestBuildConsolidationPrompt(t *testing.T) {
	params := event.WorkflowRequestedPayload{
		Prompt: "Review PR for security changes",
		Source: "gh:owner/repo#10",
	}
	handlerOutputs := map[string]string{
		"pr-security":    "No security issues found.",
		"pr-testing":     "Missing unit tests.",
		"pr-performance": "N+1 query detected.",
	}

	prompt := buildConsolidationPrompt(params, handlerOutputs)

	if !strings.Contains(prompt, "Review PR for security changes") {
		t.Error("prompt should contain task description")
	}
	if !strings.Contains(prompt, "gh:owner/repo#10") {
		t.Error("prompt should contain source reference")
	}
	if !strings.Contains(prompt, "No security issues found") {
		t.Error("prompt should contain security review output")
	}
	if !strings.Contains(prompt, "Missing unit tests") {
		t.Error("prompt should contain testing review output")
	}
	if !strings.Contains(prompt, "N+1 query detected") {
		t.Error("prompt should contain performance review output")
	}
	if !strings.Contains(prompt, "Security Review") {
		t.Error("prompt should label each section")
	}
}

func TestBuildConsolidationPromptMissingOutputs(t *testing.T) {
	params := event.WorkflowRequestedPayload{
		Source: "gh:owner/repo#5",
	}
	// Simulate most personas not yet having output.
	handlerOutputs := map[string]string{
		"pr-security": "Looks clean.",
	}

	prompt := buildConsolidationPrompt(params, handlerOutputs)

	if !strings.Contains(prompt, "(no output)") {
		t.Error("prompt should show '(no output)' for missing handlers")
	}
}

// ---------------------------------------------------------------------------
// PRConsolidatorHandler.callAI — uses mock backend
// ---------------------------------------------------------------------------

func TestPRConsolidatorCallAI(t *testing.T) {
	mb := &mockBackend{
		name: "claude",
		response: &backend.Response{
			Output:   "## Consolidated Review\n\nREQUEST CHANGES",
			Duration: time.Second,
		},
	}
	reg := persona.DefaultRegistry()
	h := &PRConsolidatorHandler{
		backend:  mb,
		store:    newMockStore(),
		registry: reg,
		builder:  persona.NewPromptBuilder(),
	}

	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-ai-test")
	params := event.WorkflowRequestedPayload{
		Source: "gh:owner/repo#1",
		Prompt: "test",
	}
	handlerOutputs := map[string]string{
		"pr-security": "ok",
		"pr-testing":  "ok",
	}

	output, err := h.callAI(context.Background(), env, params, handlerOutputs)
	if err != nil {
		t.Fatalf("callAI: %v", err)
	}
	if !strings.Contains(output, "Consolidated Review") {
		t.Errorf("want consolidated review in output, got %q", output)
	}
	// Verify the prompt was built and sent.
	if !strings.Contains(mb.lastReq.UserPrompt, "gh:owner/repo#1") {
		t.Error("user prompt should contain source reference")
	}
}

