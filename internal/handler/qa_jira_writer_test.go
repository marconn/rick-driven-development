package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/jira"
)

// ---------------------------------------------------------------------------
// extractInputs — pure function scan of event envelopes
// ---------------------------------------------------------------------------

func TestExtractInputsEmpty(t *testing.T) {
	h := &QAJiraWriterHandler{}
	ticketKey, qaOutput := h.extractInputs(nil)
	if ticketKey != "" {
		t.Errorf("want empty ticketKey, got %q", ticketKey)
	}
	if qaOutput != "" {
		t.Errorf("want empty qaOutput, got %q", qaOutput)
	}
}

func TestExtractInputsTicketFromWorkflowRequested(t *testing.T) {
	h := &QAJiraWriterHandler{}
	events := []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Ticket: "PROJ-42",
		})),
	}
	ticketKey, _ := h.extractInputs(events)
	if ticketKey != "PROJ-42" {
		t.Errorf("want ticketKey 'PROJ-42', got %q", ticketKey)
	}
}

func TestExtractInputsQAOutputFromAIResponse(t *testing.T) {
	h := &QAJiraWriterHandler{}
	qaText := "1. Test login flow\n2. Verify token expiry"
	rawOutput, _ := json.Marshal(qaText)

	events := []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Ticket: "PROJ-100",
		})),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:      "qa-analyze",
			Backend:    "claude",
			Structured: false,
			Output:     json.RawMessage(rawOutput),
		})),
	}

	ticketKey, qaOutput := h.extractInputs(events)
	if ticketKey != "PROJ-100" {
		t.Errorf("want ticketKey 'PROJ-100', got %q", ticketKey)
	}
	if qaOutput != qaText {
		t.Errorf("want qaOutput %q, got %q", qaText, qaOutput)
	}
}

func TestExtractInputsIgnoresOtherPhases(t *testing.T) {
	// AIResponseReceived from a different phase should not be picked up as qa output.
	h := &QAJiraWriterHandler{}
	otherOutput, _ := json.Marshal("some other output")

	events := []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Ticket: "PROJ-200",
		})),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:  "research",
			Output: json.RawMessage(otherOutput),
		})),
	}

	_, qaOutput := h.extractInputs(events)
	if qaOutput != "" {
		t.Errorf("want empty qaOutput for non-qa-analyze phase, got %q", qaOutput)
	}
}

func TestExtractInputsBothPresent(t *testing.T) {
	// Both WorkflowRequested (ticket) and AIResponseReceived (qa output) present.
	h := &QAJiraWriterHandler{}
	qaText := "Scenario 1: Happy path\nScenario 2: Error path"
	rawOutput, _ := json.Marshal(qaText)

	events := []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "run QA analysis",
			Ticket: "STORY-99",
		})),
		// Some other event that should be ignored.
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
			Path: "/tmp/workspace",
		})),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:      "qa-analyze",
			Structured: false,
			Output:     json.RawMessage(rawOutput),
		})),
	}

	ticketKey, qaOutput := h.extractInputs(events)
	if ticketKey != "STORY-99" {
		t.Errorf("want ticketKey 'STORY-99', got %q", ticketKey)
	}
	if qaOutput != qaText {
		t.Errorf("want qa output text, got %q", qaOutput)
	}
}

// ---------------------------------------------------------------------------
// writeQASteps — nil Jira client, custom field ID, default field ID
// ---------------------------------------------------------------------------

func TestWriteQAStepsNoJiraClient(t *testing.T) {
	// When h.jira is nil, writeQASteps should return an error about env vars.
	h := &QAJiraWriterHandler{
		store: newMockStore(),
		jira:  nil,
	}
	err := h.writeQASteps(context.Background(), "PROJ-1", "QA steps here")
	if err == nil {
		t.Fatal("expected error when Jira client is nil")
	}
	if !strings.Contains(err.Error(), "JIRA_URL") {
		t.Errorf("error should mention JIRA_URL, got: %v", err)
	}
}

func TestWriteQAStepsDefaultFieldID(t *testing.T) {
	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		bodyBytes, _ := json.Marshal(body)
		capturedBody = string(bodyBytes)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("JIRA_QA_STEPS_FIELD", "") // clear custom field, should use default

	h := &QAJiraWriterHandler{
		store: newMockStore(),
		jira:  jira.NewClient(srv.URL, "user@example.com", "token123"),
	}
	err := h.writeQASteps(context.Background(), "PROJ-1", "# QA Steps\n\n1. Test login")
	if err != nil {
		t.Fatalf("writeQASteps: %v", err)
	}

	// The default field ID "customfield_10037" should appear in the body.
	if !strings.Contains(capturedBody, "customfield_10037") {
		t.Errorf("expected default field ID 'customfield_10037' in request body, got: %s", capturedBody)
	}
}

func TestWriteQAStepsCustomFieldID(t *testing.T) {
	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		bodyBytes, _ := json.Marshal(body)
		capturedBody = string(bodyBytes)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("JIRA_QA_STEPS_FIELD", "customfield_99999")

	h := &QAJiraWriterHandler{
		store: newMockStore(),
		jira:  jira.NewClient(srv.URL, "user@example.com", "token123"),
	}
	err := h.writeQASteps(context.Background(), "PROJ-2", "Step 1: Verify feature")
	if err != nil {
		t.Fatalf("writeQASteps: %v", err)
	}

	if !strings.Contains(capturedBody, "customfield_99999") {
		t.Errorf("expected custom field ID in body, got: %s", capturedBody)
	}
	// Default field should NOT appear.
	if strings.Contains(capturedBody, "customfield_10037") {
		t.Errorf("default field should not appear when custom field is set, got: %s", capturedBody)
	}
}

func TestWriteQAStepsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	t.Setenv("JIRA_QA_STEPS_FIELD", "")

	h := &QAJiraWriterHandler{
		store: newMockStore(),
		jira:  jira.NewClient(srv.URL, "user@example.com", "token"),
	}
	err := h.writeQASteps(context.Background(), "PROJ-403", "steps")
	if err == nil {
		t.Fatal("expected error for HTTP 403 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention status code 403, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// QAJiraWriterHandler.Handle — end-to-end with mock store
// ---------------------------------------------------------------------------

func TestQAJiraWriterHandleSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	}))
	defer srv.Close()

	t.Setenv("JIRA_QA_STEPS_FIELD", "")

	store := newMockStore()
	corrID := "corr-qa-writer"
	qaText := "1. Test login flow\n2. Verify dashboard access"
	rawOutput, _ := json.Marshal(qaText)

	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Ticket:     "PROJ-55",
			WorkflowID: "jira-qa-steps",
		})).WithCorrelation(corrID),
		event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
			Phase:      "qa-analyze",
			Structured: false,
			Output:     json.RawMessage(rawOutput),
		})).WithCorrelation(corrID),
	}

	h := &QAJiraWriterHandler{
		store: store,
		jira:  jira.NewClient(srv.URL, "user@example.com", "token"),
	}

	triggerEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "qa-analyzer",
	})).WithCorrelation(corrID)

	results, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("want 1 result event, got %d", len(results))
	}
	if results[0].Type != event.ContextEnrichment {
		t.Errorf("want ContextEnrichment event, got %s", results[0].Type)
	}

	var payload event.ContextEnrichmentPayload
	if err := json.Unmarshal(results[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Kind != "jira-update" {
		t.Errorf("want kind 'jira-update', got %q", payload.Kind)
	}
	if !strings.Contains(payload.Summary, "PROJ-55") {
		t.Errorf("summary should mention ticket key, got %q", payload.Summary)
	}
}

func TestQAJiraWriterHandleMissingTicket(t *testing.T) {
	store := newMockStore()
	corrID := "corr-qa-writer-noticket"

	// No WorkflowRequested with ticket.
	store.correlationEvents[corrID] = []event.Envelope{}

	h := &QAJiraWriterHandler{
		store: store,
		jira:  nil,
	}

	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID)
	_, err := h.Handle(context.Background(), triggerEvt)
	if err == nil {
		t.Fatal("expected error when no ticket key found")
	}
	if !strings.Contains(err.Error(), "no ticket key") {
		t.Errorf("error should mention 'no ticket key', got: %v", err)
	}
}

func TestQAJiraWriterHandleMissingQAOutput(t *testing.T) {
	store := newMockStore()
	corrID := "corr-qa-writer-noqa"

	// WorkflowRequested present but no AIResponseReceived for qa-analyze.
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Ticket: "PROJ-10",
		})).WithCorrelation(corrID),
	}

	h := &QAJiraWriterHandler{
		store: store,
		jira:  nil,
	}

	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corrID)
	_, err := h.Handle(context.Background(), triggerEvt)
	if err == nil {
		t.Fatal("expected error when no qa-analyze output found")
	}
	if !strings.Contains(err.Error(), "no qa-analyze output") {
		t.Errorf("error should mention 'no qa-analyze output', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// QAJiraWriterHandler construction
// ---------------------------------------------------------------------------

func TestNewQAJiraWriter(t *testing.T) {
	h := NewQAJiraWriter(testDeps())
	if h.Name() != "qa-jira-writer" {
		t.Errorf("want name 'qa-jira-writer', got %q", h.Name())
	}
	// Subscribes returns nil for DAG-dispatched handlers — subscriptions are
	// derived from workflow Graph definitions at runtime.
	subs := h.Subscribes()
	if subs != nil {
		t.Errorf("want nil subscriptions for DAG-dispatched handler, got %v", subs)
	}
}
