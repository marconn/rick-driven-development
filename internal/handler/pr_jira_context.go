package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/jira"
)

// jiraTicketPattern matches PROJECT-\d+ style Jira keys anywhere in a string.
var jiraTicketPattern = regexp.MustCompile(`[A-Z][A-Z0-9_]+-\d+`)

// PRJiraContextHandler extracts the Jira ticket reference from the PR
// (title, body, branch), fetches the Jira issue, and emits ContextEnrichment
// so downstream AI personas have ticket context. Missing ticket is non-fatal.
type PRJiraContextHandler struct {
	store eventstore.Store
	jira  *jira.Client
}

// NewPRJiraContext creates a PRJiraContextHandler from the shared Deps.
func NewPRJiraContext(d Deps) *PRJiraContextHandler {
	return &PRJiraContextHandler{
		store: d.Store,
		jira:  d.Jira,
	}
}

// Name returns the unique handler identifier.
func (h *PRJiraContextHandler) Name() string { return "pr-jira-context" }

// Subscribes returns empty — DAG-based dispatch handles subscriptions.
func (h *PRJiraContextHandler) Subscribes() []event.Type { return nil }

// Handle extracts a Jira ticket from the PR, fetches Jira context, and emits
// a ContextEnrichment event. If no Jira ticket is found, emits enrichment with
// a summary noting the absence — never returns an error for missing tickets.
func (h *PRJiraContextHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	params, err := h.loadWorkflowRequested(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("pr-jira-context: load params: %w", err)
	}

	fullRepo, prNumber, err := parsePRSource(params.Source)
	if err != nil {
		return nil, fmt.Errorf("pr-jira-context: parse source %q: %w", params.Source, err)
	}

	prData, err := fetchPRDetails(ctx, fullRepo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("pr-jira-context: fetch PR details: %w", err)
	}

	ticketKey := extractHuliTicket(prData.Title, prData.Body, prData.HeadRefName)

	enrichment, err := h.buildEnrichment(ctx, ticketKey, prData)
	if err != nil {
		return nil, fmt.Errorf("pr-jira-context: build enrichment: %w", err)
	}

	enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(enrichment)).
		WithSource("handler:pr-jira-context")

	return []event.Envelope{enrichEvt}, nil
}

// loadWorkflowRequested reads WorkflowRequested from the correlation chain.
func (h *PRJiraContextHandler) loadWorkflowRequested(ctx context.Context, correlationID string) (event.WorkflowRequestedPayload, error) {
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

// prDetails holds fields returned from `gh pr view`.
type prDetails struct {
	Title       string `json:"title"`
	Body        string `json:"body"`
	HeadRefName string `json:"headRefName"`
}

// fetchPRDetails runs `gh pr view` to get PR metadata.
func fetchPRDetails(ctx context.Context, fullRepo, prNumber string) (prDetails, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", prNumber,
		"--repo", fullRepo,
		"--json", "title,body,headRefName")
	out, err := cmd.Output()
	if err != nil {
		return prDetails{}, fmt.Errorf("gh pr view %s: %w", prNumber, err)
	}

	var d prDetails
	if err := json.Unmarshal(out, &d); err != nil {
		return prDetails{}, fmt.Errorf("unmarshal PR details: %w", err)
	}
	return d, nil
}

// extractHuliTicket searches title, body, and branch name for a Jira ticket key.
// Returns the first match found, or empty string if none.
func extractHuliTicket(title, body, branch string) string {
	for _, s := range []string{title, branch, body} {
		if m := jiraTicketPattern.FindString(s); m != "" {
			return m
		}
	}
	return ""
}

// buildEnrichment fetches Jira data (if ticket found) and builds ContextEnrichmentPayload.
func (h *PRJiraContextHandler) buildEnrichment(ctx context.Context, ticketKey string, pr prDetails) (event.ContextEnrichmentPayload, error) {
	if ticketKey == "" {
		return event.ContextEnrichmentPayload{
			Source:  "pr-jira-context",
			Kind:    "jira-ticket",
			Summary: "No Jira ticket found in PR title, body, or branch name.",
		}, nil
	}

	if h.jira == nil {
		return event.ContextEnrichmentPayload{
			Source:  "pr-jira-context",
			Kind:    "jira-ticket",
			Summary: fmt.Sprintf("Jira ticket %s found in PR but JIRA_URL/JIRA_EMAIL/JIRA_TOKEN not configured.", ticketKey),
		}, nil
	}

	issue, err := h.jira.FetchIssue(ctx, ticketKey)
	if err != nil {
		// Non-fatal: include partial context with error note rather than failing workflow.
		return event.ContextEnrichmentPayload{
			Source:  "pr-jira-context",
			Kind:    "jira-ticket",
			Summary: fmt.Sprintf("Jira ticket %s found in PR but could not be fetched: %v", ticketKey, err),
		}, nil
	}

	summary := buildJiraEnrichmentSummary(ticketKey, issue, pr)

	return event.ContextEnrichmentPayload{
		Source:  "pr-jira-context",
		Kind:    "jira-ticket",
		Summary: summary,
	}, nil
}

// buildJiraEnrichmentSummary renders the Jira issue data into a human-readable
// markdown summary suitable for injection into AI prompts.
func buildJiraEnrichmentSummary(key string, issue *jira.Issue, pr prDetails) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Jira Ticket: %s\n\n", key)
	fmt.Fprintf(&b, "**Summary**: %s\n", issue.Fields.Summary)
	fmt.Fprintf(&b, "**Status**: %s\n\n", issue.Fields.Status.Name)

	if desc := jira.ADFToPlainText(issue.Fields.Description); desc != "" {
		fmt.Fprintf(&b, "**Description**:\n%s\n\n", desc)
	}

	// Try acceptance criteria from either custom field.
	ac := jira.ADFToPlainText(issue.Fields.AcceptanceCriteria10035)
	if ac == "" {
		ac = jira.ADFToPlainText(issue.Fields.AcceptanceCriteria10036)
	}
	if ac != "" {
		fmt.Fprintf(&b, "**Acceptance Criteria**:\n%s\n\n", ac)
	}

	fmt.Fprintf(&b, "**PR Branch**: %s\n", pr.HeadRefName)

	return strings.TrimSpace(b.String())
}
