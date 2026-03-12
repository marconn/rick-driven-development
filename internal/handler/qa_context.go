package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/jira"
)

// maxDiffBytes caps the PR diff injected into the prompt to prevent overflow.
const maxDiffBytes = 50 * 1024

// ticketPattern matches PROJ-\d+ style Jira keys anywhere in a string.
var ticketPattern = regexp.MustCompile(`[A-Z][A-Z0-9]+-\d+`)

// QAContextHandler fetches Jira ticket details and PR diff context for the
// qa-analyzer AI persona. Fires on workflow.started.jira-qa-steps.
type QAContextHandler struct {
	store eventstore.Store
	jira  *jira.Client
}

// NewQAContext creates a QAContextHandler from the shared Deps.
func NewQAContext(d Deps) *QAContextHandler {
	return &QAContextHandler{
		store: d.Store,
		jira:  d.Jira,
	}
}

func (h *QAContextHandler) Name() string { return "qa-context" }

func (h *QAContextHandler) Subscribes() []event.Type { return nil }

func (h *QAContextHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	params, err := h.loadWorkflowRequested(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("qa-context: load params: %w", err)
	}

	ticketKey := params.Ticket
	if ticketKey == "" {
		// Try extracting from Source field.
		if m := ticketPattern.FindString(params.Source); m != "" {
			ticketKey = m
		}
	}
	if ticketKey == "" {
		return nil, fmt.Errorf("qa-context: no ticket key found in workflow params")
	}

	// Fetch Jira ticket context.
	ticketEnrichment := h.fetchTicketContext(ctx, ticketKey)
	ticketEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(ticketEnrichment)).
		WithSource("handler:qa-context")

	// Find and fetch PR context.
	prEnrichment := h.fetchPRContext(ctx, params, ticketKey)
	prEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(prEnrichment)).
		WithSource("handler:qa-context")

	return []event.Envelope{ticketEvt, prEvt}, nil
}

// loadWorkflowRequested reads WorkflowRequested from the correlation chain.
func (h *QAContextHandler) loadWorkflowRequested(ctx context.Context, correlationID string) (event.WorkflowRequestedPayload, error) {
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

// fetchTicketContext fetches the Jira ticket and builds a ticket enrichment.
func (h *QAContextHandler) fetchTicketContext(ctx context.Context, ticketKey string) event.ContextEnrichmentPayload {
	if h.jira == nil {
		return event.ContextEnrichmentPayload{
			Source:  "qa-context",
			Kind:    "jira-ticket",
			Summary: fmt.Sprintf("Could not fetch Jira ticket %s: JIRA_URL/JIRA_EMAIL/JIRA_TOKEN not configured", ticketKey),
		}
	}

	issue, err := h.jira.FetchIssue(ctx, ticketKey)
	if err != nil {
		return event.ContextEnrichmentPayload{
			Source:  "qa-context",
			Kind:    "jira-ticket",
			Summary: fmt.Sprintf("Could not fetch Jira ticket %s: %v", ticketKey, err),
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Jira Ticket: %s\n\n", ticketKey)
	fmt.Fprintf(&b, "**Summary**: %s\n", issue.Fields.Summary)
	fmt.Fprintf(&b, "**Status**: %s\n\n", issue.Fields.Status.Name)

	if desc := jira.ADFToPlainText(issue.Fields.Description); desc != "" {
		fmt.Fprintf(&b, "**Description**:\n%s\n\n", desc)
	}

	ac := jira.ADFToPlainText(issue.Fields.AcceptanceCriteria10035)
	if ac == "" {
		ac = jira.ADFToPlainText(issue.Fields.AcceptanceCriteria10036)
	}
	if ac != "" {
		fmt.Fprintf(&b, "**Acceptance Criteria**:\n%s\n\n", ac)
	}

	return event.ContextEnrichmentPayload{
		Source:  "qa-context",
		Kind:    "jira-ticket",
		Summary: strings.TrimSpace(b.String()),
	}
}

// fetchPRContext finds the PR associated with the ticket and fetches diff/files.
func (h *QAContextHandler) fetchPRContext(ctx context.Context, params event.WorkflowRequestedPayload, ticketKey string) event.ContextEnrichmentPayload {
	fullRepo, prNumber, err := h.resolvePR(ctx, params, ticketKey)
	if err != nil || prNumber == "" {
		return event.ContextEnrichmentPayload{
			Source:  "qa-context",
			Kind:    "pr-context",
			Summary: fmt.Sprintf("No PR found for ticket %s. QA scenarios will be based on ticket context only.", ticketKey),
		}
	}

	diff := h.fetchDiff(ctx, fullRepo, prNumber)
	files := h.fetchFileList(ctx, fullRepo, prNumber)
	repoType := detectRepoType(files)
	prURL := fmt.Sprintf("https://github.com/%s/pull/%s", fullRepo, prNumber)

	var b strings.Builder
	fmt.Fprintf(&b, "## PR Context\n\n")
	fmt.Fprintf(&b, "**PR**: %s\n", prURL)
	fmt.Fprintf(&b, "**Repo**: %s\n", fullRepo)
	fmt.Fprintf(&b, "**Repo Type**: %s\n\n", repoType)

	if len(files) > 0 {
		fmt.Fprintf(&b, "**Changed Files**:\n")
		for _, f := range files {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\n")
	}

	if diff != "" {
		fmt.Fprintf(&b, "**Diff**:\n```\n%s\n```\n", diff)
	}

	return event.ContextEnrichmentPayload{
		Source:  "qa-context",
		Kind:    "pr-context",
		Summary: strings.TrimSpace(b.String()),
	}
}

// resolvePR finds the PR from explicit source, repo param, or Jira ticket search.
func (h *QAContextHandler) resolvePR(ctx context.Context, params event.WorkflowRequestedPayload, ticketKey string) (fullRepo, prNumber string, err error) {
	// 1. Explicit source: gh:owner/repo#N
	if strings.HasPrefix(params.Source, "gh:") {
		return parsePRSource(params.Source)
	}

	// 2. Repo provided — search for PR by ticket key.
	repo := params.Repo
	if repo == "" {
		repo = h.resolveRepoFromJira(ctx, ticketKey)
	}
	if repo == "" {
		return "", "", fmt.Errorf("no repo found")
	}

	// Search for PR matching the ticket key.
	prNum := h.searchPRByTicket(ctx, repo, ticketKey)
	if prNum == "" {
		return repo, "", nil
	}

	return repo, prNum, nil
}

// resolveRepoFromJira extracts a repo reference from Jira ticket labels.
// Looks for labels matching "repo:<name>" pattern.
func (h *QAContextHandler) resolveRepoFromJira(ctx context.Context, ticketKey string) string {
	if h.jira == nil {
		return ""
	}

	issue, err := h.jira.FetchIssue(ctx, ticketKey)
	if err != nil {
		return ""
	}

	for _, label := range issue.Fields.Labels {
		if repo, ok := strings.CutPrefix(label, "repo:"); ok {
			return repo
		}
	}

	if ms := issue.MicroserviceName(); ms != "" {
		return ms
	}

	components := issue.ComponentNames()
	if len(components) > 0 {
		return components[0]
	}

	return ""
}

// searchPRByTicket uses `gh pr list` to find a PR matching the ticket key.
func (h *QAContextHandler) searchPRByTicket(ctx context.Context, fullRepo, ticketKey string) string {
	cmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--repo", fullRepo,
		"--search", ticketKey,
		"--json", "number",
		"--limit", "1")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	var prs []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(out, &prs); err != nil || len(prs) == 0 {
		return ""
	}

	return fmt.Sprintf("%d", prs[0].Number)
}

// fetchDiff gets the PR diff via `gh pr diff`, truncated to maxDiffBytes.
func (h *QAContextHandler) fetchDiff(ctx context.Context, fullRepo, prNumber string) string {
	cmd := exec.CommandContext(ctx, "gh", "pr", "diff", prNumber,
		"--repo", fullRepo)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	diff := string(out)
	if len(diff) > maxDiffBytes {
		diff = diff[:maxDiffBytes] + "\n... (truncated)"
	}
	return diff
}

// fetchFileList gets the list of changed files from the PR.
func (h *QAContextHandler) fetchFileList(ctx context.Context, fullRepo, prNumber string) []string {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", prNumber,
		"--repo", fullRepo,
		"--json", "files")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var result struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil
	}

	var files []string
	for _, f := range result.Files {
		files = append(files, f.Path)
	}
	return files
}

// detectRepoType classifies the repository based on file extensions in the PR.
func detectRepoType(files []string) string {
	var backendCount, frontendCount int

	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f))
		switch ext {
		case ".go", ".proto", ".java", ".py", ".rb", ".rs", ".sql":
			backendCount++
		case ".vue", ".tsx", ".jsx", ".svelte", ".css", ".scss", ".less", ".html":
			frontendCount++
		case ".ts", ".js":
			// Ambiguous — check path for hints.
			lower := strings.ToLower(f)
			if strings.Contains(lower, "frontend") || strings.Contains(lower, "src/components") ||
				strings.Contains(lower, "src/views") || strings.Contains(lower, "src/pages") {
				frontendCount++
			} else {
				backendCount++
			}
		}
	}

	if backendCount > 0 && frontendCount > 0 {
		return "fullstack"
	}
	if frontendCount > 0 {
		return "frontend"
	}
	return "backend"
}
