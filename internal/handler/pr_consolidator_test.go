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

	archOutput, _ := json.Marshal("Architecture review output")
	reviewOutput, _ := json.Marshal("Code review output")
	qaOutput, _ := json.Marshal("QA output")

	events := []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt:     "Review this PR",
			WorkflowID: "pr-review",
			Source:     "gh:owner/repo#42",
		})).WithCorrelation(corrID),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:   "architect",
			Backend: "claude",
			Output:  json.RawMessage(archOutput),
		})).WithCorrelation(corrID),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:   "review",
			Backend: "claude",
			Output:  json.RawMessage(reviewOutput),
		})).WithCorrelation(corrID),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:   "qa",
			Backend: "claude",
			Output:  json.RawMessage(qaOutput),
		})).WithCorrelation(corrID),
	}

	params, phaseOutputs := extractConsolidatorInputs(events)

	if params.Source != "gh:owner/repo#42" {
		t.Errorf("params.Source: want 'gh:owner/repo#42', got %q", params.Source)
	}
	if params.Prompt != "Review this PR" {
		t.Errorf("params.Prompt: want 'Review this PR', got %q", params.Prompt)
	}
	if phaseOutputs["architect"] != "Architecture review output" {
		t.Errorf("architect output: want 'Architecture review output', got %q", phaseOutputs["architect"])
	}
	if phaseOutputs["review"] != "Code review output" {
		t.Errorf("review output: want 'Code review output', got %q", phaseOutputs["review"])
	}
	if phaseOutputs["qa"] != "QA output" {
		t.Errorf("qa output: want 'QA output', got %q", phaseOutputs["qa"])
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
	phaseOutputs := map[string]string{
		"architect": "Architecture looks good.",
		"review":    "Code has issues.",
		"qa":        "Tests pass.",
	}

	prompt := buildConsolidationPrompt(params, phaseOutputs)

	if !strings.Contains(prompt, "Review PR for security changes") {
		t.Error("prompt should contain task description")
	}
	if !strings.Contains(prompt, "gh:owner/repo#10") {
		t.Error("prompt should contain source reference")
	}
	if !strings.Contains(prompt, "Architecture looks good") {
		t.Error("prompt should contain architect output")
	}
	if !strings.Contains(prompt, "Code has issues") {
		t.Error("prompt should contain reviewer output")
	}
	if !strings.Contains(prompt, "Tests pass") {
		t.Error("prompt should contain QA output")
	}
	if !strings.Contains(prompt, "Architecture Review (pr-architect)") {
		t.Error("prompt should label each section")
	}
}

func TestBuildConsolidationPromptMissingOutputs(t *testing.T) {
	params := event.WorkflowRequestedPayload{
		Source: "gh:owner/repo#5",
	}
	// Simulate one persona not yet having output.
	phaseOutputs := map[string]string{
		"architect": "Arch output",
	}

	prompt := buildConsolidationPrompt(params, phaseOutputs)

	if !strings.Contains(prompt, "(no output)") {
		t.Error("prompt should show '(no output)' for missing phases")
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
	phaseOutputs := map[string]string{
		"architect": "ok",
		"review":    "ok",
		"qa":        "ok",
	}

	output, err := h.callAI(context.Background(), env, params, phaseOutputs)
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

