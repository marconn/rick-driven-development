package jirapoller

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/jira"
)

type stubRegistry struct {
	known map[string]engine.WorkflowDef
}

func (s *stubRegistry) GetWorkflowDef(id string) (engine.WorkflowDef, bool) {
	def, ok := s.known[id]
	return def, ok
}

// TestPoller_PollSkipsWhenWorkflowNotRegistered locks in the publisher-side
// guard for docs/bugs/jira-dev-stuck-in-requested.md. When the configured
// JIRA_POLL_WORKFLOW points at a DAG the engine doesn't know (typo,
// env-drift, plugin not loaded), the poller must short-circuit BEFORE
// hitting the Jira API and BEFORE publishing any WorkflowRequested. The
// short-circuit prevents two bad outcomes: wasted Jira API calls, and a
// stream of fail-fast WorkflowFailed events the operator would have to
// chase down in the tracker.
func TestPoller_PollSkipsWhenWorkflowNotRegistered(t *testing.T) {
	var jiraCalls atomic.Int32
	jiraSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		jiraCalls.Add(1)
		t.Errorf("unexpected Jira API call when workflow_id is unknown — short-circuit broken")
	}))
	defer jiraSrv.Close()

	bus := eventbus.NewChannelBus()
	defer func() { _ = bus.Close() }()

	var publishCount atomic.Int32
	bus.SubscribeAll(func(_ context.Context, _ event.Envelope) error {
		publishCount.Add(1)
		return nil
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	jiraClient := jira.NewClient(jiraSrv.URL, "test@example.com", "tok")

	registry := &stubRegistry{known: map[string]engine.WorkflowDef{
		"jira-dev": {ID: "jira-dev", Required: []string{"developer"}, MaxIterations: 1},
	}}

	p := &Poller{
		cfg: Config{
			JQL:          "project = HULI",
			WorkflowID:   "jira-dev-typo", // not in registry
			MaxResults:   50,
			PollInterval: 60 * 1e9,
			Logger:       logger,
		},
		jira:     jiraClient,
		bus:      bus,
		registry: registry,
		logger:   logger,
	}

	p.poll(context.Background())

	if jiraCalls.Load() != 0 {
		t.Errorf("expected 0 Jira API calls, got %d", jiraCalls.Load())
	}
	if publishCount.Load() != 0 {
		t.Errorf("expected 0 events published, got %d", publishCount.Load())
	}
}

// TestPoller_PollProceedsWhenWorkflowRegistered verifies the registry
// guard is a no-op on the happy path. With the workflow registered, the
// poller must reach the Jira search call (we observe by counting hits).
func TestPoller_PollProceedsWhenWorkflowRegistered(t *testing.T) {
	var jiraCalls atomic.Int32
	jiraSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		jiraCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":0,"issues":[]}`))
	}))
	defer jiraSrv.Close()

	bus := eventbus.NewChannelBus()
	defer func() { _ = bus.Close() }()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	jiraClient := jira.NewClient(jiraSrv.URL, "test@example.com", "tok")

	registry := &stubRegistry{known: map[string]engine.WorkflowDef{
		"jira-dev": {ID: "jira-dev", Required: []string{"developer"}, MaxIterations: 1},
	}}

	p := &Poller{
		cfg: Config{
			JQL:          "project = HULI",
			WorkflowID:   "jira-dev",
			MaxResults:   50,
			PollInterval: 60 * 1e9,
			Logger:       logger,
		},
		jira:     jiraClient,
		bus:      bus,
		registry: registry,
		logger:   logger,
	}

	p.poll(context.Background())

	if jiraCalls.Load() == 0 {
		t.Errorf("expected at least 1 Jira API call when workflow is registered, got 0 — guard incorrectly short-circuiting happy path")
	}
}

// TestPoller_PollNilRegistry_BackCompat verifies that passing nil registry
// keeps the legacy behavior (no validation) so existing test setups still
// work. Production wiring always passes a real registry.
func TestPoller_PollNilRegistry_BackCompat(t *testing.T) {
	var jiraCalls atomic.Int32
	jiraSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		jiraCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":0,"issues":[]}`))
	}))
	defer jiraSrv.Close()

	bus := eventbus.NewChannelBus()
	defer func() { _ = bus.Close() }()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	jiraClient := jira.NewClient(jiraSrv.URL, "test@example.com", "tok")

	p := &Poller{
		cfg:    Config{JQL: "project = HULI", WorkflowID: "anything", MaxResults: 50, PollInterval: 60 * 1e9, Logger: logger},
		jira:   jiraClient,
		bus:    bus,
		logger: logger,
		// registry intentionally nil
	}

	p.poll(context.Background())

	if jiraCalls.Load() == 0 {
		t.Error("nil registry must skip validation; Jira call should have happened")
	}
}
