package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"github.com/marconn/rick-event-driven-development/internal/adf"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/jira"
)

// QAJiraWriterHandler writes AI-generated QA steps back to the Jira ticket's
// QA Steps custom field. Fires after qa-analyzer completes.
type QAJiraWriterHandler struct {
	store eventstore.Store
	jira  *jira.Client
}

// NewQAJiraWriter creates a QAJiraWriterHandler from the shared Deps.
func NewQAJiraWriter(d Deps) *QAJiraWriterHandler {
	return &QAJiraWriterHandler{
		store: d.Store,
		jira:  d.Jira,
	}
}

func (h *QAJiraWriterHandler) Name() string { return "qa-jira-writer" }

func (h *QAJiraWriterHandler) Subscribes() []event.Type { return nil }

func (h *QAJiraWriterHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	events, err := h.store.LoadByCorrelation(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("qa-jira-writer: load correlation chain: %w", err)
	}

	ticketKey, qaOutput := h.extractInputs(events)
	if ticketKey == "" {
		return nil, fmt.Errorf("qa-jira-writer: no ticket key found in workflow params")
	}
	if qaOutput == "" {
		return nil, fmt.Errorf("qa-jira-writer: no qa-analyze output found in correlation chain")
	}

	if err := h.writeQASteps(ctx, ticketKey, qaOutput); err != nil {
		return nil, fmt.Errorf("qa-jira-writer: write QA steps to %s: %w", ticketKey, err)
	}

	enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
		Source:  "qa-jira-writer",
		Kind:    "jira-update",
		Summary: fmt.Sprintf("QA Steps written to Jira ticket %s", ticketKey),
	})).WithSource("handler:qa-jira-writer")

	return []event.Envelope{enrichEvt}, nil
}

// extractInputs scans the correlation chain for the ticket key and qa-analyze AI output.
func (h *QAJiraWriterHandler) extractInputs(events []event.Envelope) (ticketKey, qaOutput string) {
	for _, e := range events {
		switch e.Type {
		case event.WorkflowRequested:
			var p event.WorkflowRequestedPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				ticketKey = p.Ticket
			}

		case event.AIResponseReceived:
			var p event.AIResponsePayload
			if err := json.Unmarshal(e.Payload, &p); err == nil && p.Phase == "qa-analyze" {
				qaOutput = unmarshalOutput(p.Output, p.Structured)
			}
		}
	}
	return ticketKey, qaOutput
}

// writeQASteps PUTs the QA steps text to the Jira ticket's custom field.
func (h *QAJiraWriterHandler) writeQASteps(ctx context.Context, ticketKey, qaSteps string) error {
	if h.jira == nil {
		return fmt.Errorf("JIRA_URL, JIRA_EMAIL, JIRA_TOKEN env vars must be set")
	}

	fieldID := os.Getenv("JIRA_QA_STEPS_FIELD")
	if fieldID == "" {
		fieldID = "customfield_10037"
	}

	adfDoc := adf.FromMarkdown(qaSteps)

	return h.jira.UpdateField(ctx, ticketKey, fieldID, adfDoc)
}

