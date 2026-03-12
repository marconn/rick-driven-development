package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/jira"
)

// JiraContextHandler fetches full Jira ticket details and emits context
// enrichment for downstream personas. Used in the jira-dev workflow.
type JiraContextHandler struct {
	store eventstore.Store
	jira  *jira.Client
}

// NewJiraContext creates a JiraContextHandler.
func NewJiraContext(d Deps) *JiraContextHandler {
	return &JiraContextHandler{
		store: d.Store,
		jira:  d.Jira,
	}
}

func (h *JiraContextHandler) Name() string            { return "jira-context" }
func (h *JiraContextHandler) Subscribes() []event.Type { return nil }

func (h *JiraContextHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	params, err := h.loadWorkflowRequested(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("jira-context: load params: %w", err)
	}

	ticket := params.Ticket
	if ticket == "" {
		if after, ok := strings.CutPrefix(params.Source, "jira:"); ok {
			ticket = after
		}
	}
	if ticket == "" {
		return nil, fmt.Errorf("jira-context: no ticket in workflow payload (set ticket or source=jira:KEY)")
	}

	if h.jira == nil {
		return nil, fmt.Errorf("jira-context: JIRA_URL/JIRA_EMAIL/JIRA_TOKEN not configured")
	}

	raw, err := h.jira.FetchRawIssue(ctx, ticket)
	if err != nil {
		return nil, fmt.Errorf("jira-context: fetch %s: %w", ticket, err)
	}

	enrichment := buildTicketEnrichment(ticket, raw)
	enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(enrichment)).
		WithSource("handler:jira-context")

	return []event.Envelope{enrichEvt}, nil
}

func (h *JiraContextHandler) loadWorkflowRequested(ctx context.Context, correlationID string) (event.WorkflowRequestedPayload, error) {
	if correlationID == "" {
		return event.WorkflowRequestedPayload{}, nil
	}

	events, err := h.store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return event.WorkflowRequestedPayload{}, fmt.Errorf("load correlation chain: %w", err)
	}

	for _, e := range events {
		if e.Type != event.WorkflowRequested {
			continue
		}
		var p event.WorkflowRequestedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return event.WorkflowRequestedPayload{}, fmt.Errorf("unmarshal workflow requested: %w", err)
		}
		return p, nil
	}

	return event.WorkflowRequestedPayload{}, nil
}

func buildTicketEnrichment(ticket string, raw *jira.RawIssue) event.ContextEnrichmentPayload {
	summary := jira.ExtractTextField(raw.Fields["summary"])
	description := jira.ExtractTextField(raw.Fields["description"])

	var status string
	if statusRaw, ok := raw.Fields["status"]; ok {
		var s struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(statusRaw, &s) == nil {
			status = s.Name
		}
	}

	// Acceptance criteria from common custom fields.
	var acceptanceCriteria string
	for _, fieldID := range []string{"customfield_10035", "customfield_10036"} {
		if v, ok := raw.Fields[fieldID]; ok {
			if text := jira.ExtractTextField(v); text != "" {
				acceptanceCriteria = text
				break
			}
		}
	}

	// Repo from labels or components.
	repo := extractRepoFromRaw(raw)

	var sb strings.Builder
	fmt.Fprintf(&sb, "## Jira Ticket: %s\n\n", ticket)
	fmt.Fprintf(&sb, "**Summary**: %s\n", summary)
	if status != "" {
		fmt.Fprintf(&sb, "**Status**: %s\n\n", status)
	}
	if description != "" {
		fmt.Fprintf(&sb, "**Description**:\n%s\n\n", description)
	}
	if acceptanceCriteria != "" {
		fmt.Fprintf(&sb, "**Acceptance Criteria**:\n%s\n\n", acceptanceCriteria)
	}
	if repo != "" {
		fmt.Fprintf(&sb, "**Repo**: %s\n", repo)
	}

	enrichment := event.ContextEnrichmentPayload{
		Source:  "jira-context",
		Kind:    "ticket",
		Summary: strings.TrimSpace(sb.String()),
	}

	if repo != "" {
		enrichment.Items = []event.EnrichmentItem{
			{Name: "repo", Reason: repo},
		}
	}

	return enrichment
}

func extractRepoFromRaw(raw *jira.RawIssue) string {
	// Microservice custom field (customfield_11538): select field with {value: "myapp"}.
	if msRaw, ok := raw.Fields["customfield_11538"]; ok {
		var ms struct {
			Value string `json:"value"`
		}
		if json.Unmarshal(msRaw, &ms) == nil && ms.Value != "" {
			return ms.Value
		}
	}

	// Labels: repo:owner/name
	if labelsRaw, ok := raw.Fields["labels"]; ok {
		var labels []string
		if json.Unmarshal(labelsRaw, &labels) == nil {
			for _, l := range labels {
				if after, ok := strings.CutPrefix(l, "repo:"); ok {
					return after
				}
			}
		}
	}

	// Components: first component name.
	if compRaw, ok := raw.Fields["components"]; ok {
		var comps []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(compRaw, &comps) == nil && len(comps) > 0 {
			return comps[0].Name
		}
	}

	return ""
}
