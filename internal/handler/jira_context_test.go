package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/jira"
)

// --- extractRepoFromRaw ---

func TestExtractRepoFromLabels(t *testing.T) {
	raw := &jira.RawIssue{
		Fields: map[string]json.RawMessage{
			"labels": json.RawMessage(`["bug", "repo:practice-web", "priority:high"]`),
		},
	}
	got := extractRepoFromRaw(raw)
	if got != "practice-web" {
		t.Errorf("want 'practice-web', got %q", got)
	}
}

func TestExtractRepoFromMicroserviceField(t *testing.T) {
	raw := &jira.RawIssue{
		Fields: map[string]json.RawMessage{
			"customfield_11538": json.RawMessage(`{"self":"https://jira/rest/api/2/customFieldOption/10223","value":"myapp","id":"10223"}`),
		},
	}
	got := extractRepoFromRaw(raw)
	if got != "myapp" {
		t.Errorf("want 'myapp', got %q", got)
	}
}

func TestExtractRepoMicroserviceOverLabels(t *testing.T) {
	raw := &jira.RawIssue{
		Fields: map[string]json.RawMessage{
			"labels":            json.RawMessage(`["repo:from-label"]`),
			"customfield_11538": json.RawMessage(`{"value":"myapp"}`),
		},
	}
	got := extractRepoFromRaw(raw)
	if got != "myapp" {
		t.Errorf("microservice field should take priority over labels: want 'myapp', got %q", got)
	}
}

func TestExtractRepoMicroserviceOverComponents(t *testing.T) {
	raw := &jira.RawIssue{
		Fields: map[string]json.RawMessage{
			"customfield_11538": json.RawMessage(`{"value":"myapp"}`),
			"components":        json.RawMessage(`[{"name":"api-gateway"}]`),
		},
	}
	got := extractRepoFromRaw(raw)
	if got != "myapp" {
		t.Errorf("microservice field should take priority over components: want 'myapp', got %q", got)
	}
}

func TestExtractRepoFromComponents(t *testing.T) {
	raw := &jira.RawIssue{
		Fields: map[string]json.RawMessage{
			"components": json.RawMessage(`[{"name":"api-gateway"},{"name":"shared-lib"}]`),
		},
	}
	got := extractRepoFromRaw(raw)
	if got != "api-gateway" {
		t.Errorf("want 'api-gateway', got %q", got)
	}
}

func TestExtractRepoLabelsOverComponents(t *testing.T) {
	raw := &jira.RawIssue{
		Fields: map[string]json.RawMessage{
			"labels":     json.RawMessage(`["repo:from-label"]`),
			"components": json.RawMessage(`[{"name":"from-component"}]`),
		},
	}
	got := extractRepoFromRaw(raw)
	if got != "from-label" {
		t.Errorf("labels should take priority over components: want 'from-label', got %q", got)
	}
}

func TestExtractRepoEmpty(t *testing.T) {
	raw := &jira.RawIssue{
		Fields: map[string]json.RawMessage{
			"labels": json.RawMessage(`["bug", "priority:high"]`),
		},
	}
	got := extractRepoFromRaw(raw)
	if got != "" {
		t.Errorf("want empty, got %q", got)
	}
}

// --- buildTicketEnrichment ---

func TestBuildTicketEnrichment_IncludesFields(t *testing.T) {
	raw := &jira.RawIssue{
		Fields: map[string]json.RawMessage{
			"summary": json.RawMessage(`"Fix login bug"`),
			"status":  json.RawMessage(`{"name":"In Progress"}`),
			"labels":  json.RawMessage(`["repo:auth-service"]`),
		},
	}
	enrichment := buildTicketEnrichment("PROJ-123", raw)

	if enrichment.Source != "jira-context" {
		t.Errorf("source=%q, want jira-context", enrichment.Source)
	}
	if enrichment.Kind != "ticket" {
		t.Errorf("kind=%q, want ticket", enrichment.Kind)
	}
	if !strings.Contains(enrichment.Summary, "PROJ-123") {
		t.Error("summary missing ticket key")
	}
	if !strings.Contains(enrichment.Summary, "Fix login bug") {
		t.Error("summary missing issue summary")
	}
	if !strings.Contains(enrichment.Summary, "In Progress") {
		t.Error("summary missing status")
	}
	if !strings.Contains(enrichment.Summary, "auth-service") {
		t.Error("summary missing repo")
	}
	// Repo must also be in Items so workspace handler can find it.
	if len(enrichment.Items) == 0 {
		t.Fatal("expected Items with repo entry")
	}
	if enrichment.Items[0].Name != "repo" || enrichment.Items[0].Reason != "auth-service" {
		t.Errorf("Items[0] = {%q, %q}, want {repo, auth-service}", enrichment.Items[0].Name, enrichment.Items[0].Reason)
	}
}

func TestBuildTicketEnrichment_NoRepo_NoItems(t *testing.T) {
	raw := &jira.RawIssue{
		Fields: map[string]json.RawMessage{
			"summary": json.RawMessage(`"No repo ticket"`),
		},
	}
	enrichment := buildTicketEnrichment("PROJ-999", raw)
	if len(enrichment.Items) != 0 {
		t.Errorf("expected no Items when no repo, got %d", len(enrichment.Items))
	}
}

// --- jiraContextMockStore ---

type jiraContextMockStore struct {
	correlationEvents map[string][]event.Envelope
}

func newJiraContextMockStore() *jiraContextMockStore {
	return &jiraContextMockStore{correlationEvents: make(map[string][]event.Envelope)}
}

func (s *jiraContextMockStore) LoadByCorrelation(_ context.Context, correlationID string) ([]event.Envelope, error) {
	return s.correlationEvents[correlationID], nil
}

func (s *jiraContextMockStore) Append(context.Context, string, int, []event.Envelope) error              { return nil }
func (s *jiraContextMockStore) Load(context.Context, string) ([]event.Envelope, error)                   { return nil, nil }
func (s *jiraContextMockStore) LoadFrom(context.Context, string, int) ([]event.Envelope, error)          { return nil, nil }
func (s *jiraContextMockStore) LoadAll(context.Context, int64, int) ([]eventstore.PositionedEvent, error) { return nil, nil }
func (s *jiraContextMockStore) LoadEvent(context.Context, string) (*event.Envelope, error)               { return nil, nil }
func (s *jiraContextMockStore) SaveSnapshot(context.Context, eventstore.Snapshot) error                  { return nil }
func (s *jiraContextMockStore) LoadSnapshot(context.Context, string) (*eventstore.Snapshot, error)       { return nil, nil }
func (s *jiraContextMockStore) RecordDeadLetter(context.Context, eventstore.DeadLetter) error            { return nil }
func (s *jiraContextMockStore) LoadDeadLetters(context.Context) ([]eventstore.DeadLetter, error)         { return nil, nil }
func (s *jiraContextMockStore) DeleteDeadLetter(context.Context, string) error                           { return nil }
func (s *jiraContextMockStore) SaveTags(context.Context, string, map[string]string) error                { return nil }
func (s *jiraContextMockStore) LoadByTag(context.Context, string, string) ([]string, error)              { return nil, nil }
func (s *jiraContextMockStore) Close() error                                                             { return nil }

// --- JiraContextHandler.Handle ---

func TestJiraContextHandler_Handle_Success(t *testing.T) {
	// Mock Jira server returning a raw issue
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/PROJ-123") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"key": "PROJ-123",
			"fields": {
				"summary": "Fix the login flow",
				"status": {"name": "In Progress"},
				"labels": ["repo:auth-service"],
				"components": [{"name": "backend"}]
			}
		}`))
	}))
	t.Cleanup(srv.Close)

	store := newJiraContextMockStore()
	payload, _ := json.Marshal(event.WorkflowRequestedPayload{
		Ticket:     "PROJ-123",
		WorkflowID: "jira-dev",
	})
	store.correlationEvents["corr-1"] = []event.Envelope{
		{Type: event.WorkflowRequested, CorrelationID: "corr-1", Payload: payload},
	}

	jiraClient := jira.NewClient(srv.URL, "a@b.com", "tok")

	h := &JiraContextHandler{store: store, jira: jiraClient}

	env := event.Envelope{CorrelationID: "corr-1"}
	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected enrichment event, got none")
	}

	var enrichment event.ContextEnrichmentPayload
	if err := json.Unmarshal(results[0].Payload, &enrichment); err != nil {
		t.Fatalf("unmarshal enrichment: %v", err)
	}
	if enrichment.Kind != "ticket" {
		t.Errorf("kind=%q, want ticket", enrichment.Kind)
	}
	if !strings.Contains(enrichment.Summary, "Fix the login flow") {
		t.Error("summary missing issue summary text")
	}
	// Repo must propagate as an Item for the workspace handler.
	found := false
	for _, item := range enrichment.Items {
		if item.Name == "repo" && item.Reason == "auth-service" {
			found = true
		}
	}
	if !found {
		t.Errorf("enrichment.Items missing repo item, got %+v", enrichment.Items)
	}
}

func TestJiraContextHandler_Handle_TicketFromSource(t *testing.T) {
	// When ticket is empty but source=jira:KEY, should extract ticket from source
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"PROJ-456","fields":{"summary":"Test"}}`)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	store := newJiraContextMockStore()
	payload, _ := json.Marshal(event.WorkflowRequestedPayload{
		Source:     "jira:PROJ-456",
		WorkflowID: "jira-dev",
	})
	store.correlationEvents["corr-2"] = []event.Envelope{
		{Type: event.WorkflowRequested, CorrelationID: "corr-2", Payload: payload},
	}

	h := &JiraContextHandler{store: store, jira: jira.NewClient(srv.URL, "a@b.com", "tok")}

	env := event.Envelope{CorrelationID: "corr-2"}
	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected enrichment event")
	}
}

func TestJiraContextHandler_Handle_NoTicket(t *testing.T) {
	store := newJiraContextMockStore()
	payload, _ := json.Marshal(event.WorkflowRequestedPayload{
		WorkflowID: "jira-dev",
	})
	store.correlationEvents["corr-3"] = []event.Envelope{
		{Type: event.WorkflowRequested, CorrelationID: "corr-3", Payload: payload},
	}

	h := &JiraContextHandler{store: store, jira: jira.NewClient("http://unused", "a", "b")}

	env := event.Envelope{CorrelationID: "corr-3"}
	_, err := h.Handle(context.Background(), env)
	if err == nil {
		t.Fatal("expected error when no ticket, got nil")
	}
	if !strings.Contains(err.Error(), "no ticket") {
		t.Errorf("error should mention 'no ticket': %v", err)
	}
}

func TestJiraContextHandler_Handle_NilJiraClient(t *testing.T) {
	store := newJiraContextMockStore()
	payload, _ := json.Marshal(event.WorkflowRequestedPayload{
		Ticket:     "PROJ-789",
		WorkflowID: "jira-dev",
	})
	store.correlationEvents["corr-4"] = []event.Envelope{
		{Type: event.WorkflowRequested, CorrelationID: "corr-4", Payload: payload},
	}

	h := &JiraContextHandler{store: store, jira: nil}

	env := event.Envelope{CorrelationID: "corr-4"}
	_, err := h.Handle(context.Background(), env)
	if err == nil {
		t.Fatal("expected error when jira client is nil, got nil")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error should mention 'not configured': %v", err)
	}
}
