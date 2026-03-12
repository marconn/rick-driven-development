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
// extractHuliTicket
// ---------------------------------------------------------------------------

func TestExtractHuliTicketFromTitle(t *testing.T) {
	ticket := extractHuliTicket("PROJ-123: Fix login bug", "", "feature/some-branch")
	if ticket != "PROJ-123" {
		t.Errorf("want PROJ-123, got %q", ticket)
	}
}

func TestExtractHuliTicketFromBranch(t *testing.T) {
	ticket := extractHuliTicket("Fix login bug", "", "feature/PROJ-456-fix-login")
	if ticket != "PROJ-456" {
		t.Errorf("want PROJ-456, got %q", ticket)
	}
}

func TestExtractHuliTicketFromBody(t *testing.T) {
	ticket := extractHuliTicket("Fix login bug", "This PR resolves PROJ-789.", "feature/no-ticket")
	if ticket != "PROJ-789" {
		t.Errorf("want PROJ-789, got %q", ticket)
	}
}

func TestExtractHuliTicketTitleWinsOverBranch(t *testing.T) {
	// Title is checked first — it should win over branch.
	ticket := extractHuliTicket("PROJ-100: Title ticket", "body", "feature/PROJ-200-branch")
	if ticket != "PROJ-100" {
		t.Errorf("want PROJ-100 (title wins), got %q", ticket)
	}
}

func TestExtractHuliTicketNotFound(t *testing.T) {
	ticket := extractHuliTicket("Fix login bug", "No ticket reference.", "feature/some-branch")
	if ticket != "" {
		t.Errorf("want empty string, got %q", ticket)
	}
}

// ---------------------------------------------------------------------------
// jira.ADFToPlainText — validate ADF conversion used in buildJiraEnrichmentSummary
// ---------------------------------------------------------------------------

func TestJiraADFToPlainTextString(t *testing.T) {
	result := jira.ADFToPlainText("plain string")
	if result != "plain string" {
		t.Errorf("want %q, got %q", "plain string", result)
	}
}

func TestJiraADFToPlainTextNil(t *testing.T) {
	result := jira.ADFToPlainText(nil)
	if result != "" {
		t.Errorf("want empty string for nil, got %q", result)
	}
}

func TestJiraADFToPlainTextADFObject(t *testing.T) {
	// Simulate a simple ADF text node structure (as parsed from JSON).
	adfDoc := map[string]any{
		"type": "doc",
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": "Hello world",
					},
				},
			},
		},
	}
	result := jira.ADFToPlainText(adfDoc)
	if !strings.Contains(result, "Hello world") {
		t.Errorf("want 'Hello world' in result, got %q", result)
	}
}

// ---------------------------------------------------------------------------
// PRJiraContextHandler construction
// ---------------------------------------------------------------------------

func TestNewPRJiraContext(t *testing.T) {
	h := NewPRJiraContext(testDeps())
	if h.Name() != "pr-jira-context" {
		t.Errorf("want name 'pr-jira-context', got %q", h.Name())
	}

	// Subscribes returns nil for DAG-dispatched handlers.
	subs := h.Subscribes()
	if subs != nil {
		t.Errorf("want nil subscriptions for DAG-dispatched handler, got %v", subs)
	}
}

// ---------------------------------------------------------------------------
// PRJiraContextHandler — Jira API fetch via jira.Client
// ---------------------------------------------------------------------------

func TestPRJiraContextFetchJiraIssue(t *testing.T) {
	issueJSON := `{
		"key": "PROJ-123",
		"fields": {
			"summary": "Fix login timeout",
			"description": "Users experience timeout after 5 minutes.",
			"status": {"name": "In Progress"},
			"customfield_10035": null,
			"customfield_10036": null
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !strings.Contains(r.URL.Path, "PROJ-123") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Verify basic auth is set.
		user, pass, ok := r.BasicAuth()
		if !ok || user == "" || pass == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(issueJSON))
	}))
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test@example.com", "token123")
	issue, err := client.FetchIssue(context.Background(), "PROJ-123")
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	if issue.Fields.Summary != "Fix login timeout" {
		t.Errorf("want summary %q, got %q", "Fix login timeout", issue.Fields.Summary)
	}
	if issue.Fields.Status.Name != "In Progress" {
		t.Errorf("want status %q, got %q", "In Progress", issue.Fields.Status.Name)
	}
}

func TestPRJiraContextFetchJiraIssue404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test@example.com", "token123")
	_, err := client.FetchIssue(context.Background(), "PROJ-999")
	if err == nil {
		t.Fatal("want error for 404 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention 404, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// buildJiraEnrichmentSummary — with acceptance criteria fields
// ---------------------------------------------------------------------------

func TestBuildJiraEnrichmentSummaryWithAcceptanceCriteria10035(t *testing.T) {
	issue := &jira.Issue{}
	issue.Key = "PROJ-200"
	issue.Fields.Summary = "Add user authentication"
	issue.Fields.Status.Name = "In Development"
	issue.Fields.Description = "Implement JWT-based auth"
	issue.Fields.AcceptanceCriteria10035 = "User can log in with email and password"
	issue.Fields.AcceptanceCriteria10036 = nil

	pr := prDetails{HeadRefName: "feature/PROJ-200-auth"}

	summary := buildJiraEnrichmentSummary("PROJ-200", issue, pr)

	if !strings.Contains(summary, "PROJ-200") {
		t.Error("summary should contain ticket key")
	}
	if !strings.Contains(summary, "Add user authentication") {
		t.Error("summary should contain issue summary")
	}
	if !strings.Contains(summary, "Acceptance Criteria") {
		t.Error("summary should contain 'Acceptance Criteria' section")
	}
	if !strings.Contains(summary, "User can log in with email and password") {
		t.Error("summary should contain acceptance criteria text from customfield_10035")
	}
}

func TestBuildJiraEnrichmentSummaryWithAcceptanceCriteria10036Fallback(t *testing.T) {
	// customfield_10035 is nil, should fall back to 10036.
	issue := &jira.Issue{}
	issue.Key = "PROJ-201"
	issue.Fields.Summary = "Implement OAuth flow"
	issue.Fields.Status.Name = "In Progress"
	issue.Fields.AcceptanceCriteria10035 = nil
	issue.Fields.AcceptanceCriteria10036 = "User can log in via Google SSO"

	pr := prDetails{HeadRefName: "feature/PROJ-201-oauth"}

	summary := buildJiraEnrichmentSummary("PROJ-201", issue, pr)

	if !strings.Contains(summary, "Acceptance Criteria") {
		t.Error("summary should contain 'Acceptance Criteria' section")
	}
	if !strings.Contains(summary, "User can log in via Google SSO") {
		t.Error("summary should contain acceptance criteria from customfield_10036 fallback")
	}
}

func TestBuildJiraEnrichmentSummaryNoAcceptanceCriteria(t *testing.T) {
	// Neither field — no "Acceptance Criteria" section should appear.
	issue := &jira.Issue{}
	issue.Key = "PROJ-202"
	issue.Fields.Summary = "Fix typo"
	issue.Fields.Status.Name = "Done"
	issue.Fields.AcceptanceCriteria10035 = nil
	issue.Fields.AcceptanceCriteria10036 = nil

	pr := prDetails{HeadRefName: "fix/typo"}

	summary := buildJiraEnrichmentSummary("PROJ-202", issue, pr)

	if strings.Contains(summary, "Acceptance Criteria") {
		t.Error("summary should NOT contain 'Acceptance Criteria' when both fields are nil")
	}
}

// ---------------------------------------------------------------------------
// buildJiraEnrichmentSummary
// ---------------------------------------------------------------------------

func TestBuildJiraEnrichmentSummary(t *testing.T) {
	issue := &jira.Issue{}
	issue.Key = "PROJ-123"
	issue.Fields.Summary = "Fix login bug"
	issue.Fields.Status.Name = "In Progress"
	issue.Fields.Description = "Users experience timeout."

	pr := prDetails{HeadRefName: "feature/PROJ-123-fix-login"}

	summary := buildJiraEnrichmentSummary("PROJ-123", issue, pr)

	if !strings.Contains(summary, "PROJ-123") {
		t.Error("summary should contain ticket key")
	}
	if !strings.Contains(summary, "Fix login bug") {
		t.Error("summary should contain issue summary")
	}
	if !strings.Contains(summary, "In Progress") {
		t.Error("summary should contain status")
	}
	if !strings.Contains(summary, "feature/PROJ-123-fix-login") {
		t.Error("summary should contain PR branch name")
	}
}

// ---------------------------------------------------------------------------
// PRJiraContextHandler.Handle — no ticket found (non-fatal)
// ---------------------------------------------------------------------------

func TestPRJiraContextHandlerNoTicketEmitsEnrichment(t *testing.T) {
	// We can't easily mock `gh pr view` in unit tests, so we test the
	// buildEnrichment path directly for the no-ticket case.
	h := &PRJiraContextHandler{
		store: newMockStore(),
		jira:  nil,
	}

	enrichment, err := h.buildEnrichment(context.Background(), "", prDetails{
		Title:       "Fix login bug",
		Body:        "No ticket reference here.",
		HeadRefName: "feature/no-ticket",
	})
	if err != nil {
		t.Fatalf("buildEnrichment: %v", err)
	}

	if enrichment.Kind != "jira-ticket" {
		t.Errorf("want kind 'jira-ticket', got %q", enrichment.Kind)
	}
	if !strings.Contains(enrichment.Summary, "No Jira ticket") {
		t.Errorf("want 'No Jira ticket' in summary, got %q", enrichment.Summary)
	}
}

// ---------------------------------------------------------------------------
// buildEnrichment when FetchIssue fails — non-fatal, returns enrichment with error note
// ---------------------------------------------------------------------------

func TestBuildEnrichmentJiraFetchError(t *testing.T) {
	// Jira API returns 500 — buildEnrichment should NOT return an error.
	// It should return an enrichment payload with an error note in the Summary.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := &PRJiraContextHandler{
		store: newMockStore(),
		jira:  jira.NewClient(srv.URL, "user@example.com", "token"),
	}

	enrichment, err := h.buildEnrichment(context.Background(), "PROJ-500", prDetails{
		Title:       "Fix login bug",
		Body:        "PROJ-500 related",
		HeadRefName: "feature/PROJ-500-login",
	})
	// buildEnrichment must NOT propagate the Jira fetch error — it is non-fatal.
	if err != nil {
		t.Fatalf("buildEnrichment should not return error for Jira fetch failure, got: %v", err)
	}
	if enrichment.Kind != "jira-ticket" {
		t.Errorf("want kind 'jira-ticket', got %q", enrichment.Kind)
	}
	// Summary should mention the ticket and the fetch failure.
	if !strings.Contains(enrichment.Summary, "PROJ-500") {
		t.Errorf("summary should mention ticket key 'PROJ-500', got: %q", enrichment.Summary)
	}
	if !strings.Contains(enrichment.Summary, "could not be fetched") {
		t.Errorf("summary should mention fetch failure, got: %q", enrichment.Summary)
	}
}

// ---------------------------------------------------------------------------
// ContextEnrichment event payload marshaling
// ---------------------------------------------------------------------------

func TestContextEnrichmentPayloadMarshaling(t *testing.T) {
	payload := event.ContextEnrichmentPayload{
		Source:  "pr-jira-context",
		Kind:    "jira-ticket",
		Summary: "## Jira Ticket: PROJ-123\nStatus: In Progress",
	}
	raw := event.MustMarshal(payload)

	var decoded event.ContextEnrichmentPayload
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Source != "pr-jira-context" {
		t.Errorf("Source: want 'pr-jira-context', got %q", decoded.Source)
	}
	if decoded.Kind != "jira-ticket" {
		t.Errorf("Kind: want 'jira-ticket', got %q", decoded.Kind)
	}
}
