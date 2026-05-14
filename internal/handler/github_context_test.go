package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
	gh "github.com/marconn/rick-event-driven-development/internal/github"
)

// --- resolveGithubIssueRef ---

func TestResolveGithubIssueRef_FromSource(t *testing.T) {
	p := event.WorkflowRequestedPayload{Source: "gh:acme/widgets#42"}
	owner, repo, number, err := resolveGithubIssueRef(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "acme" || repo != "widgets" || number != 42 {
		t.Errorf("got (%q, %q, %d), want (acme, widgets, 42)", owner, repo, number)
	}
}

func TestResolveGithubIssueRef_FromRepoAndTicket(t *testing.T) {
	p := event.WorkflowRequestedPayload{Repo: "acme/widgets", Ticket: "101"}
	owner, repo, number, err := resolveGithubIssueRef(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "acme" || repo != "widgets" || number != 101 {
		t.Errorf("got (%q, %q, %d), want (acme, widgets, 101)", owner, repo, number)
	}
}

func TestResolveGithubIssueRef_FromTicketWithHash(t *testing.T) {
	p := event.WorkflowRequestedPayload{Repo: "acme/widgets", Ticket: "#7"}
	_, _, number, err := resolveGithubIssueRef(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if number != 7 {
		t.Errorf("number=%d, want 7", number)
	}
}

func TestResolveGithubIssueRef_SourceWinsOverRepoTicket(t *testing.T) {
	p := event.WorkflowRequestedPayload{
		Source: "gh:from-source/repo#1",
		Repo:   "other/other",
		Ticket: "999",
	}
	owner, repo, number, err := resolveGithubIssueRef(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "from-source" || repo != "repo" || number != 1 {
		t.Errorf("source should win: got (%q, %q, %d)", owner, repo, number)
	}
}

func TestResolveGithubIssueRef_Missing(t *testing.T) {
	_, _, _, err := resolveGithubIssueRef(event.WorkflowRequestedPayload{})
	if err == nil {
		t.Fatal("expected error when no reference, got nil")
	}
	if !strings.Contains(err.Error(), "no issue reference") {
		t.Errorf("error should mention 'no issue reference': %v", err)
	}
}

func TestResolveGithubIssueRef_RepoWithoutSlash(t *testing.T) {
	p := event.WorkflowRequestedPayload{Repo: "nobadges", Ticket: "5"}
	_, _, _, err := resolveGithubIssueRef(p)
	if err == nil {
		t.Fatal("expected error for malformed repo, got nil")
	}
}

func TestResolveGithubIssueRef_NonNumericTicket(t *testing.T) {
	p := event.WorkflowRequestedPayload{Repo: "acme/widgets", Ticket: "PROJ-123"}
	_, _, _, err := resolveGithubIssueRef(p)
	if err == nil {
		t.Fatal("expected error for non-numeric ticket, got nil")
	}
}

// --- buildGithubIssueEnrichment ---

func TestBuildGithubIssueEnrichment_IncludesFields(t *testing.T) {
	issue := &gh.Issue{
		Number:  42,
		Title:   "Login flow broken on Safari",
		State:   "open",
		HTMLURL: "https://github.com/acme/widgets/issues/42",
		User:    gh.User{Login: "alice"},
		Body:    "Steps to reproduce:\n1. Open Safari\n2. Click login",
		Labels:  []gh.IssueLabel{{Name: "bug"}, {Name: "ui"}},
	}
	enrichment := buildGithubIssueEnrichment("acme", "widgets", 42, issue)

	if enrichment.Source != "github-context" {
		t.Errorf("source=%q, want github-context", enrichment.Source)
	}
	if enrichment.Kind != "issue" {
		t.Errorf("kind=%q, want issue", enrichment.Kind)
	}
	for _, want := range []string{"acme/widgets#42", "Login flow broken on Safari", "@alice", "open", "bug, ui", "Steps to reproduce"} {
		if !strings.Contains(enrichment.Summary, want) {
			t.Errorf("summary missing %q; got:\n%s", want, enrichment.Summary)
		}
	}
	// Enrichment must carry both `repo` and `ticket` so the workspace handler
	// can provision a branch for github-dev workflows (which have no
	// caller-supplied ticket — only source=gh:owner/repo#N).
	// Regression: https://… "ticket or branch is required" on github-dev.
	items := map[string]string{}
	for _, it := range enrichment.Items {
		items[it.Name] = it.Reason
	}
	if items["repo"] != "acme/widgets" {
		t.Errorf("items[repo]=%q, want acme/widgets", items["repo"])
	}
	if items["ticket"] != "issue-42" {
		t.Errorf("items[ticket]=%q, want issue-42", items["ticket"])
	}
}

func TestBuildGithubIssueEnrichment_EmptyBody(t *testing.T) {
	issue := &gh.Issue{Number: 1, Title: "no body", Body: "   "}
	enrichment := buildGithubIssueEnrichment("o", "r", 1, issue)
	if strings.Contains(enrichment.Summary, "**Description**") {
		t.Error("empty body should not render a Description section")
	}
}

// --- GithubContextHandler.Handle ---

func TestGithubContextHandler_Handle_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/acme/widgets/issues/42") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"number": 42,
			"title": "Fix login",
			"state": "open",
			"body": "Steps to reproduce",
			"user": {"login":"alice"},
			"labels": [{"name":"bug"}]
		}`))
	}))
	t.Cleanup(srv.Close)

	store := newJiraContextMockStore() // reused mock (interface-only, no Jira coupling)
	payload, _ := json.Marshal(event.WorkflowRequestedPayload{
		Source:     "gh:acme/widgets#42",
		WorkflowID: "github-dev",
	})
	store.correlationEvents["corr-1"] = []event.Envelope{
		{Type: event.WorkflowRequested, CorrelationID: "corr-1", Payload: payload},
	}

	h := &GithubContextHandler{store: store, github: gh.NewClientWithBase(srv.URL, "tok")}

	results, err := h.Handle(context.Background(), event.Envelope{CorrelationID: "corr-1"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 enrichment event, got %d", len(results))
	}

	var enrichment event.ContextEnrichmentPayload
	if err := json.Unmarshal(results[0].Payload, &enrichment); err != nil {
		t.Fatalf("unmarshal enrichment: %v", err)
	}
	if enrichment.Kind != "issue" {
		t.Errorf("kind=%q, want issue", enrichment.Kind)
	}
	if !strings.Contains(enrichment.Summary, "Fix login") {
		t.Error("summary missing issue title")
	}
	foundRepo := false
	for _, item := range enrichment.Items {
		if item.Name == "repo" && item.Reason == "acme/widgets" {
			foundRepo = true
		}
	}
	if !foundRepo {
		t.Errorf("missing repo item, got %+v", enrichment.Items)
	}
}

func TestGithubContextHandler_Handle_RejectsPullRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// "pull_request" key is how GitHub marks issues that are PRs.
		_, _ = w.Write([]byte(`{
			"number": 42,
			"title": "PR not issue",
			"pull_request": {}
		}`))
	}))
	t.Cleanup(srv.Close)

	store := newJiraContextMockStore()
	payload, _ := json.Marshal(event.WorkflowRequestedPayload{
		Source: "gh:acme/widgets#42",
	})
	store.correlationEvents["corr-pr"] = []event.Envelope{
		{Type: event.WorkflowRequested, CorrelationID: "corr-pr", Payload: payload},
	}

	h := &GithubContextHandler{store: store, github: gh.NewClientWithBase(srv.URL, "tok")}

	_, err := h.Handle(context.Background(), event.Envelope{CorrelationID: "corr-pr"})
	if err == nil {
		t.Fatal("expected error when target is a PR, got nil")
	}
	if !strings.Contains(err.Error(), "pull request") {
		t.Errorf("error should mention pull request: %v", err)
	}
}

func TestGithubContextHandler_Handle_NoReference(t *testing.T) {
	store := newJiraContextMockStore()
	payload, _ := json.Marshal(event.WorkflowRequestedPayload{WorkflowID: "github-dev"})
	store.correlationEvents["corr-x"] = []event.Envelope{
		{Type: event.WorkflowRequested, CorrelationID: "corr-x", Payload: payload},
	}

	h := &GithubContextHandler{store: store, github: gh.NewClient("tok")}

	_, err := h.Handle(context.Background(), event.Envelope{CorrelationID: "corr-x"})
	if err == nil {
		t.Fatal("expected error when no reference, got nil")
	}
	if !strings.Contains(err.Error(), "no issue reference") {
		t.Errorf("error should mention missing reference: %v", err)
	}
}

func TestGithubContextHandler_Handle_RefusesClosedIssue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"number": 891,
			"title": "BE: Day-4 hardening bundle",
			"state": "closed",
			"state_reason": "completed",
			"closed_at": "2025-11-12T18:33:21Z",
			"body": "shipped at PR #1086"
		}`))
	}))
	t.Cleanup(srv.Close)

	store := newJiraContextMockStore()
	payload, _ := json.Marshal(event.WorkflowRequestedPayload{
		Source: "gh:acme/widgets#891",
		Prompt: "Implement acme/widgets#891",
	})
	store.correlationEvents["corr-closed"] = []event.Envelope{
		{Type: event.WorkflowRequested, CorrelationID: "corr-closed", Payload: payload},
	}

	h := &GithubContextHandler{store: store, github: gh.NewClientWithBase(srv.URL, "tok")}

	_, err := h.Handle(context.Background(), event.Envelope{CorrelationID: "corr-closed"})
	if err == nil {
		t.Fatal("expected error for closed issue, got nil")
	}
	for _, want := range []string{"closed", "state_reason=\"completed\"", "allow-closed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q; got: %v", want, err)
		}
	}
}

func TestGithubContextHandler_Handle_AllowClosedOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"number": 891,
			"title": "BE: Day-4 hardening bundle",
			"state": "closed",
			"state_reason": "completed",
			"closed_at": "2025-11-12T18:33:21Z",
			"body": "shipped at PR #1086"
		}`))
	}))
	t.Cleanup(srv.Close)

	store := newJiraContextMockStore()
	payload, _ := json.Marshal(event.WorkflowRequestedPayload{
		Source: "gh:acme/widgets#891",
		Prompt: "Re-implement acme/widgets#891 in a fresh branch — allow-closed",
	})
	store.correlationEvents["corr-allow"] = []event.Envelope{
		{Type: event.WorkflowRequested, CorrelationID: "corr-allow", Payload: payload},
	}

	h := &GithubContextHandler{store: store, github: gh.NewClientWithBase(srv.URL, "tok")}

	results, err := h.Handle(context.Background(), event.Envelope{CorrelationID: "corr-allow"})
	if err != nil {
		t.Fatalf("override should allow closed issue: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 enrichment event, got %d", len(results))
	}

	var enrichment event.ContextEnrichmentPayload
	if err := json.Unmarshal(results[0].Payload, &enrichment); err != nil {
		t.Fatalf("unmarshal enrichment: %v", err)
	}
	for _, want := range []string{"State reason", "completed", "Closed at", "2025-11-12"} {
		if !strings.Contains(enrichment.Summary, want) {
			t.Errorf("override summary missing %q; got:\n%s", want, enrichment.Summary)
		}
	}
}

func TestGithubContextHandler_Handle_OpenIssueIgnoresOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"number": 7,
			"title": "Still open",
			"state": "open",
			"body": "work to do"
		}`))
	}))
	t.Cleanup(srv.Close)

	store := newJiraContextMockStore()
	payload, _ := json.Marshal(event.WorkflowRequestedPayload{
		Source: "gh:acme/widgets#7",
		Prompt: "Implement acme/widgets#7",
	})
	store.correlationEvents["corr-open"] = []event.Envelope{
		{Type: event.WorkflowRequested, CorrelationID: "corr-open", Payload: payload},
	}

	h := &GithubContextHandler{store: store, github: gh.NewClientWithBase(srv.URL, "tok")}

	results, err := h.Handle(context.Background(), event.Envelope{CorrelationID: "corr-open"})
	if err != nil {
		t.Fatalf("open issue should pass unconditionally: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 enrichment event, got %d", len(results))
	}
	var enrichment event.ContextEnrichmentPayload
	_ = json.Unmarshal(results[0].Payload, &enrichment)
	if strings.Contains(enrichment.Summary, "State reason") {
		t.Errorf("open issue should not render state reason; got:\n%s", enrichment.Summary)
	}
}

func TestHasAllowClosedOverride(t *testing.T) {
	cases := []struct {
		prompt string
		want   bool
	}{
		{"", false},
		{"Implement #42", false},
		{"Implement #42 — allow-closed", true},
		{"allow-closed", true},
		{"allow_closed", false},   // underscore variant must NOT match — typo guard
		{"Allow-Closed", false},   // case-sensitive — operator must type it as documented
		{"allowclosed", false},    // missing hyphen
	}
	for _, c := range cases {
		if got := hasAllowClosedOverride(c.prompt); got != c.want {
			t.Errorf("hasAllowClosedOverride(%q) = %v, want %v", c.prompt, got, c.want)
		}
	}
}

func TestGithubContextHandler_Handle_NilGithubClient(t *testing.T) {
	store := newJiraContextMockStore()
	payload, _ := json.Marshal(event.WorkflowRequestedPayload{Source: "gh:acme/widgets#7"})
	store.correlationEvents["corr-nil"] = []event.Envelope{
		{Type: event.WorkflowRequested, CorrelationID: "corr-nil", Payload: payload},
	}

	h := &GithubContextHandler{store: store, github: nil}

	_, err := h.Handle(context.Background(), event.Envelope{CorrelationID: "corr-nil"})
	if err == nil {
		t.Fatal("expected error when github client is nil, got nil")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error should mention 'not configured': %v", err)
	}
}
