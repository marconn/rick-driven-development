package jiraplanner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/confluence"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// --- isNumeric ---

func TestIsNumeric(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"1994031125", true},
		{"0", true},
		{"abc", false},
		{"12abc", false},
		{"", false},
		{" 123", false},
	}
	for _, tt := range tests {
		if got := isNumeric(tt.input); got != tt.want {
			t.Errorf("isNumeric(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// --- PageReaderHandler.extractPageID ---

// readerMockStore is a minimal mock store that returns pre-loaded events
// for LoadByCorrelation. Only the method needed by PageReaderHandler is
// implemented; all others return zero values.
type readerMockStore struct {
	eventstore.Store // embed to satisfy interface; unused methods will panic if called
	events           map[string][]event.Envelope
}

func newReaderMockStore() *readerMockStore {
	return &readerMockStore{events: make(map[string][]event.Envelope)}
}

func (s *readerMockStore) LoadByCorrelation(_ context.Context, correlationID string) ([]event.Envelope, error) {
	return s.events[correlationID], nil
}

func (s *readerMockStore) Append(_ context.Context, _ string, _ int, _ []event.Envelope) error { return nil }
func (s *readerMockStore) Load(_ context.Context, _ string) ([]event.Envelope, error)          { return nil, nil }
func (s *readerMockStore) LoadFrom(_ context.Context, _ string, _ int) ([]event.Envelope, error) {
	return nil, nil
}
func (s *readerMockStore) LoadAll(_ context.Context, _ int64, _ int) ([]eventstore.PositionedEvent, error) {
	return nil, nil
}
func (s *readerMockStore) LoadEvent(_ context.Context, _ string) (*event.Envelope, error) {
	return nil, nil
}
func (s *readerMockStore) SaveSnapshot(_ context.Context, _ eventstore.Snapshot) error { return nil }
func (s *readerMockStore) LoadSnapshot(_ context.Context, _ string) (*eventstore.Snapshot, error) {
	return nil, nil
}
func (s *readerMockStore) RecordDeadLetter(_ context.Context, _ eventstore.DeadLetter) error {
	return nil
}
func (s *readerMockStore) LoadDeadLetters(_ context.Context) ([]eventstore.DeadLetter, error) {
	return nil, nil
}
func (s *readerMockStore) DeleteDeadLetter(_ context.Context, _ string) error    { return nil }
func (s *readerMockStore) SaveTags(_ context.Context, _ string, _ map[string]string) error {
	return nil
}
func (s *readerMockStore) LoadByTag(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}
func (s *readerMockStore) Close() error { return nil }

func makeWorkflowRequestedEnv(correlationID, source, prompt string) event.Envelope {
	payload, _ := json.Marshal(event.WorkflowRequestedPayload{
		Source: source,
		Prompt: prompt,
	})
	return event.Envelope{
		Type:          event.WorkflowRequested,
		CorrelationID: correlationID,
		Payload:       payload,
	}
}

func TestPageReader_ExtractPageID_ConfluencePrefix(t *testing.T) {
	store := newReaderMockStore()
	store.events["corr-1"] = []event.Envelope{
		makeWorkflowRequestedEnv("corr-1", "confluence:1994031125", ""),
	}
	r := NewPageReader(nil, store, NewPlanningState(), discardLogger())

	env := event.Envelope{CorrelationID: "corr-1"}
	id, err := r.extractPageID(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "1994031125" {
		t.Errorf("id=%q, want 1994031125", id)
	}
}

func TestPageReader_ExtractPageID_NumericSource(t *testing.T) {
	store := newReaderMockStore()
	store.events["corr-2"] = []event.Envelope{
		makeWorkflowRequestedEnv("corr-2", "1994031125", ""),
	}
	r := NewPageReader(nil, store, NewPlanningState(), discardLogger())

	env := event.Envelope{CorrelationID: "corr-2"}
	id, err := r.extractPageID(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "1994031125" {
		t.Errorf("id=%q, want 1994031125", id)
	}
}

func TestPageReader_ExtractPageID_NotFound(t *testing.T) {
	store := newReaderMockStore()
	store.events["corr-3"] = []event.Envelope{
		makeWorkflowRequestedEnv("corr-3", "no-page-here", "no url either"),
	}
	r := NewPageReader(nil, store, NewPlanningState(), discardLogger())

	env := event.Envelope{CorrelationID: "corr-3"}
	_, err := r.extractPageID(context.Background(), env)
	if err == nil {
		t.Fatal("expected error when no page ID can be found")
	}
}

// --- Confluence ReadPage via httptest ---

func TestConfluence_ReadPage_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/12345") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("expand") == "" {
			t.Error("expand param missing")
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Basic ") {
			t.Errorf("expected Basic auth, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "12345",
			"title": "My Page",
			"body": {"storage": {"value": "<p>Hello <b>world</b></p>"}}
		}`))
	}))
	t.Cleanup(srv.Close)

	c := confluence.NewClient(srv.URL, "user@example.com", "token")
	page, err := c.ReadPage(context.Background(), "12345")
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if page.ID != "12345" {
		t.Errorf("ID=%q, want 12345", page.ID)
	}
	if page.Title != "My Page" {
		t.Errorf("Title=%q, want 'My Page'", page.Title)
	}
	if !strings.Contains(page.Body, "Hello") {
		t.Errorf("Body missing content: %q", page.Body)
	}
}

func TestConfluence_ReadPage_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"page not found"}`)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	c := confluence.NewClient(srv.URL, "u", "t")
	_, err := c.ReadPage(context.Background(), "99999")
	if err == nil {
		t.Fatal("expected error for HTTP 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention 404: %v", err)
	}
}

func TestPageReader_Handle_NilConfluence(t *testing.T) {
	store := newReaderMockStore()
	store.events["corr-nil"] = []event.Envelope{
		makeWorkflowRequestedEnv("corr-nil", "confluence:123", ""),
	}
	r := NewPageReader(nil, store, NewPlanningState(), discardLogger())

	env := event.Envelope{CorrelationID: "corr-nil"}
	_, err := r.Handle(context.Background(), env)
	if err == nil {
		t.Fatal("expected error when confluence client is nil")
	}
}
