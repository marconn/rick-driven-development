package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/jira"
)

// ---------------------------------------------------------------------------
// detectRepoType — pure function table-driven tests
// ---------------------------------------------------------------------------

func TestDetectRepoType(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  string
	}{
		{
			name:  "go-grpc: pure .go files",
			files: []string{"main.go", "internal/handler/server.go", "api/proto/service.proto"},
			want:  "backend",
		},
		{
			name:  "go-gin: go files with SQL",
			files: []string{"main.go", "db/migrations/001_init.sql", "internal/handler.go"},
			want:  "backend",
		},
		{
			name:  "go-echo: go and proto files",
			files: []string{"internal/service.go", "api/types.proto"},
			want:  "backend",
		},
		{
			name:  "plain go: just go files",
			files: []string{"pkg/lib.go", "cmd/main.go"},
			want:  "backend",
		},
		{
			name:  "react: tsx and jsx files",
			files: []string{"src/components/App.tsx", "src/pages/Home.jsx", "src/app.css"},
			want:  "frontend",
		},
		{
			name:  "nextjs: tsx in pages",
			files: []string{"src/pages/index.tsx", "src/components/Header.tsx"},
			want:  "frontend",
		},
		{
			name:  "nuxtjs: vue files",
			files: []string{"pages/index.vue", "components/Nav.vue"},
			want:  "frontend",
		},
		{
			name:  "vue-vite: .vue files",
			files: []string{"src/views/Home.vue", "src/App.vue"},
			want:  "frontend",
		},
		{
			name:  "vue-webpack: vue with scss",
			files: []string{"src/components/Button.vue", "src/assets/style.scss"},
			want:  "frontend",
		},
		{
			name:  "empty: no files",
			files: []string{},
			want:  "backend",
		},
		{
			name:  "fullstack: go + vue",
			files: []string{"internal/api/server.go", "frontend/src/App.vue"},
			want:  "fullstack",
		},
		{
			name:  "fullstack: go + tsx",
			files: []string{"cmd/server/main.go", "web/src/components/App.tsx"},
			want:  "fullstack",
		},
		{
			name:  "ts in frontend path counts as frontend",
			files: []string{"frontend/src/components/Widget.ts"},
			want:  "frontend",
		},
		{
			name:  "ts in non-frontend path counts as backend",
			files: []string{"scripts/build.ts"},
			want:  "backend",
		},
		{
			name:  "svelte file: frontend",
			files: []string{"src/lib/Component.svelte"},
			want:  "frontend",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := detectRepoType(tc.files)
			if got != tc.want {
				t.Errorf("detectRepoType(%v) = %q, want %q", tc.files, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// QAContextHandler.Handle — mock store seeded with events
// ---------------------------------------------------------------------------

func TestQAContextHandlerHandleNoTicket(t *testing.T) {
	store := newMockStore()
	corrID := "corr-qa-noticket"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt:     "analyze this PR",
			WorkflowID: "jira-qa-steps",
			// No Ticket, no Source with ticket pattern
		})).WithCorrelation(corrID),
	}

	h := &QAContextHandler{
		store: store,
		jira:  nil, // no Jira client
	}

	env := event.New(event.WorkflowStartedFor("jira-qa-steps"), 1, nil).WithCorrelation(corrID)
	_, err := h.Handle(context.Background(), env)
	if err == nil {
		t.Fatal("expected error when no ticket key found")
	}
	if !strings.Contains(err.Error(), "no ticket key") {
		t.Errorf("error should mention 'no ticket key', got: %v", err)
	}
}

func TestQAContextHandlerHandleWithTicketInSource(t *testing.T) {
	// When Ticket is empty but Source contains a ticket pattern, it should extract it.
	store := newMockStore()
	corrID := "corr-qa-source"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt:     "run qa",
			WorkflowID: "jira-qa-steps",
			Source:     "jira:PROJ-456",
		})).WithCorrelation(corrID),
	}

	// nil Jira client — fetchTicketContext is non-fatal.
	h := &QAContextHandler{
		store: store,
		jira:  nil,
	}

	env := event.New(event.WorkflowStartedFor("jira-qa-steps"), 1, nil).WithCorrelation(corrID)
	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Should emit two ContextEnrichment events: one for ticket, one for PR context.
	if len(results) != 2 {
		t.Fatalf("want 2 enrichment events, got %d", len(results))
	}
	for _, r := range results {
		if r.Type != event.ContextEnrichment {
			t.Errorf("want ContextEnrichment, got %s", r.Type)
		}
	}
}

func TestQAContextHandlerHandleWithExplicitTicket(t *testing.T) {
	store := newMockStore()
	corrID := "corr-qa-explicit"

	// Set up a fake Jira server.
	issueJSON := `{
		"key": "MYPROJ-99",
		"fields": {
			"summary": "Add login feature",
			"status": {"name": "Done"},
			"description": null,
			"customfield_10035": null,
			"customfield_10036": null
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "MYPROJ-99") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, issueJSON)
	}))
	defer srv.Close()

	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt:     "qa steps",
			WorkflowID: "jira-qa-steps",
			Ticket:     "MYPROJ-99",
		})).WithCorrelation(corrID),
	}

	h := &QAContextHandler{
		store: store,
		jira:  jira.NewClient(srv.URL, "user@example.com", "token"),
	}

	env := event.New(event.WorkflowStartedFor("jira-qa-steps"), 1, nil).WithCorrelation(corrID)
	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 events, got %d", len(results))
	}

	// First event should be the ticket enrichment.
	var ticketPayload event.ContextEnrichmentPayload
	if err := json.Unmarshal(results[0].Payload, &ticketPayload); err != nil {
		t.Fatalf("unmarshal ticket enrichment: %v", err)
	}
	if ticketPayload.Kind != "jira-ticket" {
		t.Errorf("want kind 'jira-ticket', got %q", ticketPayload.Kind)
	}
	if !strings.Contains(ticketPayload.Summary, "MYPROJ-99") {
		t.Errorf("ticket enrichment should contain ticket key, got: %q", ticketPayload.Summary)
	}
	if !strings.Contains(ticketPayload.Summary, "Add login feature") {
		t.Errorf("ticket enrichment should contain summary, got: %q", ticketPayload.Summary)
	}
}

// ---------------------------------------------------------------------------
// resolvePR
// ---------------------------------------------------------------------------

func TestResolvePRExplicitSource(t *testing.T) {
	h := &QAContextHandler{
		store: newMockStore(),
		jira:  nil,
	}

	params := event.WorkflowRequestedPayload{
		Source: "gh:owner/my-repo#42",
	}

	repo, prNum, err := h.resolvePR(context.Background(), params, "PROJ-1")
	if err != nil {
		t.Fatalf("resolvePR: %v", err)
	}
	if repo != "owner/my-repo" {
		t.Errorf("want repo 'owner/my-repo', got %q", repo)
	}
	if prNum != "42" {
		t.Errorf("want prNumber '42', got %q", prNum)
	}
}

func TestResolvePRRepoParam(t *testing.T) {
	// Repo param provided but no gh CLI available — searchPRByTicket returns empty.
	h := &QAContextHandler{
		store: newMockStore(),
		jira:  nil,
	}

	params := event.WorkflowRequestedPayload{
		Repo: "owner/my-service",
	}

	repo, prNum, err := h.resolvePR(context.Background(), params, "PROJ-99")
	if err != nil {
		t.Fatalf("resolvePR with repo param: %v", err)
	}
	if repo != "owner/my-service" {
		t.Errorf("want repo 'owner/my-service', got %q", repo)
	}
	// prNum is empty because gh CLI is not available in unit tests.
	_ = prNum
}

func TestResolvePRNoRepoAndNoJira(t *testing.T) {
	// No source, no repo, nil Jira client → error.
	h := &QAContextHandler{
		store: newMockStore(),
		jira:  nil,
	}

	params := event.WorkflowRequestedPayload{}

	_, _, err := h.resolvePR(context.Background(), params, "PROJ-1")
	if err == nil {
		t.Fatal("want error when no repo found")
	}
}

// ---------------------------------------------------------------------------
// fetchTicketContext — nil Jira client, error, and success paths
// ---------------------------------------------------------------------------

func TestFetchTicketContextNilJira(t *testing.T) {
	// When Jira client is nil, fetchTicketContext is non-fatal and returns an
	// enrichment with an error note.
	h := &QAContextHandler{
		store: newMockStore(),
		jira:  nil,
	}

	result := h.fetchTicketContext(context.Background(), "PROJ-1")
	if result.Source != "qa-context" {
		t.Errorf("want source 'qa-context', got %q", result.Source)
	}
	if result.Kind != "jira-ticket" {
		t.Errorf("want kind 'jira-ticket', got %q", result.Kind)
	}
	// Should contain the error note, not panic.
	if result.Summary == "" {
		t.Error("summary should not be empty for failed fetch")
	}
}

func TestFetchTicketContextHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := &QAContextHandler{
		store: newMockStore(),
		jira:  jira.NewClient(srv.URL, "user@example.com", "token"),
	}

	result := h.fetchTicketContext(context.Background(), "PROJ-500")
	// Non-fatal: should return enrichment with error note.
	if !strings.Contains(result.Summary, "PROJ-500") {
		t.Errorf("error summary should reference ticket key, got: %q", result.Summary)
	}
}

func TestFetchTicketContextSuccess(t *testing.T) {
	issueJSON := `{
		"key": "PROJ-777",
		"fields": {
			"summary": "Implement OAuth",
			"status": {"name": "In Progress"},
			"description": "Add OAuth2 flow to the application.",
			"customfield_10035": "User can log in with Google",
			"customfield_10036": null
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "PROJ-777") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, issueJSON)
	}))
	defer srv.Close()

	h := &QAContextHandler{
		store: newMockStore(),
		jira:  jira.NewClient(srv.URL, "user@example.com", "token"),
	}

	result := h.fetchTicketContext(context.Background(), "PROJ-777")
	if result.Kind != "jira-ticket" {
		t.Errorf("want kind 'jira-ticket', got %q", result.Kind)
	}
	if !strings.Contains(result.Summary, "Implement OAuth") {
		t.Errorf("summary should contain issue summary, got: %q", result.Summary)
	}
	if !strings.Contains(result.Summary, "In Progress") {
		t.Errorf("summary should contain status, got: %q", result.Summary)
	}
	if !strings.Contains(result.Summary, "User can log in with Google") {
		t.Errorf("summary should contain acceptance criteria, got: %q", result.Summary)
	}
}

// ---------------------------------------------------------------------------
// QAContextHandler construction
// ---------------------------------------------------------------------------

func TestNewQAContext(t *testing.T) {
	h := NewQAContext(testDeps())
	if h.Name() != "qa-context" {
		t.Errorf("want name 'qa-context', got %q", h.Name())
	}

	// Subscribes returns nil for DAG-dispatched handlers — subscriptions are
	// derived from workflow Graph definitions at runtime.
	subs := h.Subscribes()
	if subs != nil {
		t.Errorf("want nil subscriptions for DAG-dispatched handler, got %v", subs)
	}
}
