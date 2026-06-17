package jiraplanner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/jira"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// respondEmptySearch answers the idempotency precheck (POST /search/jql) with
// zero results so createJiraIssues proceeds to the create path. Returns true
// when it handled the request. Tests asserting create behavior use this so the
// new dedup search doesn't intercept their create handlers.
func respondEmptySearch(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasSuffix(r.URL.Path, "/search/jql") {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{"total": 0, "issues": []any{}}) //nolint:errcheck
	return true
}

// --- buildEpicDescription ---

func TestBuildEpicDescription_AllFields(t *testing.T) {
	plan := &ProjectPlan{
		Goal:     "Build payment module",
		EpicDesc: "Full description here.",
		Risks: []Risk{
			{Description: "Integration risk", Probability: "media", Mitigation: "Add retries"},
		},
		Dependencies: []Dep{
			{Name: "payments-service", Description: "needs v2 API"},
		},
	}

	desc := buildEpicDescription(plan)

	if !strings.Contains(desc, "Build payment module") {
		t.Error("description missing goal")
	}
	if !strings.Contains(desc, "Full description here.") {
		t.Error("description missing EpicDesc")
	}
	if !strings.Contains(desc, "Integration risk") {
		t.Error("description missing risk")
	}
	if !strings.Contains(desc, "payments-service") {
		t.Error("description missing dependency")
	}
}

func TestBuildEpicDescription_EmptyPlan(t *testing.T) {
	// Should not panic on empty plan.
	desc := buildEpicDescription(&ProjectPlan{})
	if desc != "" {
		t.Errorf("empty plan should produce empty description, got %q", desc)
	}
}

func TestBuildEpicDescription_GoalOnly(t *testing.T) {
	plan := &ProjectPlan{Goal: "Migrate database"}
	desc := buildEpicDescription(plan)
	if !strings.Contains(desc, "Migrate database") {
		t.Errorf("description should contain goal, got %q", desc)
	}
}

// --- TaskCreatorHandler.Handle ---

func TestTaskCreator_Handle_NoPlanInState(t *testing.T) {
	state := NewPlanningState()
	creator := NewTaskCreator(nil, state, discardLogger())

	env := event.Envelope{CorrelationID: "corr-1"}

	_, err := creator.Handle(context.Background(), env)
	if err == nil {
		t.Fatal("expected error when no plan in state, got nil")
	}
	if !strings.Contains(err.Error(), "no project plan") {
		t.Errorf("error should mention 'no project plan': %v", err)
	}
}

// --- TaskCreatorHandler integration via httptest Jira server ---

func newTestCreator(t *testing.T) (*TaskCreatorHandler, *[]string) {
	t.Helper()
	var createdKeys []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if respondEmptySearch(w, r) {
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck

		fields := body["fields"].(map[string]any)
		issuetype := fields["issuetype"].(map[string]any)["name"].(string)

		var key string
		if issuetype == "Epic" {
			key = "TEST-100"
		} else {
			key = "TEST-" + strings.Repeat("1", len(createdKeys)+1)
		}
		createdKeys = append(createdKeys, key)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"key": key}) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	j := jira.NewClient(srv.URL, "u", "t").WithProject("TEST").WithTeamID("10571")
	state := NewPlanningState()
	creator := NewTaskCreator(j, state, discardLogger())
	return creator, &createdKeys
}

func TestTaskCreator_Handle_CreatesEpicAndTasks(t *testing.T) {
	creator, createdKeys := newTestCreator(t)

	plan := &ProjectPlan{
		Goal:      "Build feature",
		EpicTitle: "Feature Epic",
		EpicDesc:  "Epic description",
		Tasks: []JiraTask{
			{Title: "Task A", Description: "Do A", Priority: 1, StoryPoints: 3},
			{Title: "Task B", Description: "Do B", Priority: 2, StoryPoints: 5},
		},
	}

	state := creator.state
	wd := state.Get("corr-epic")
	wd.mu.Lock()
	wd.Plan = plan
	wd.mu.Unlock()

	env := event.Envelope{CorrelationID: "corr-epic"}

	results, err := creator.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected enrichment event, got none")
	}
	// Epic + 2 tasks = 3 Jira API calls
	if len(*createdKeys) != 3 {
		t.Errorf("expected 3 Jira issues created, got %d: %v", len(*createdKeys), *createdKeys)
	}
	// Check enrichment summary mentions the epic key
	var payload event.ContextEnrichmentPayload
	if err := json.Unmarshal(results[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal enrichment: %v", err)
	}
	if !strings.Contains(payload.Summary, "TEST-100") {
		t.Errorf("summary should contain epic key, got: %q", payload.Summary)
	}
}

func TestTaskCreator_Handle_TasksSortedByPriority(t *testing.T) {
	var capturedSummaries []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if respondEmptySearch(w, r) {
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		fields := body["fields"].(map[string]any)
		capturedSummaries = append(capturedSummaries, fields["summary"].(string))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"key": "TEST-1"}) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	j := jira.NewClient(srv.URL, "u", "t").WithProject("TEST")
	state := NewPlanningState()
	creator := NewTaskCreator(j, state, discardLogger())

	plan := &ProjectPlan{
		EpicTitle: "Epic",
		Tasks: []JiraTask{
			{Title: "Low priority", Priority: 3, StoryPoints: 1},
			{Title: "Critical", Priority: 1, StoryPoints: 8},
			{Title: "Important", Priority: 2, StoryPoints: 3},
		},
	}
	wd := state.Get("sort-corr")
	wd.mu.Lock()
	wd.Plan = plan
	wd.mu.Unlock()

	env := event.Envelope{CorrelationID: "sort-corr"}
	if _, err := creator.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// capturedSummaries[0] = "Epic" (the epic itself)
	// Tasks should follow in priority order: Critical, Important, Low priority
	if len(capturedSummaries) < 4 {
		t.Fatalf("expected 4 API calls (1 epic + 3 tasks), got %d", len(capturedSummaries))
	}
	if capturedSummaries[1] != "Critical" {
		t.Errorf("first task should be Critical (priority=1), got %q", capturedSummaries[1])
	}
	if capturedSummaries[2] != "Important" {
		t.Errorf("second task should be Important (priority=2), got %q", capturedSummaries[2])
	}
	if capturedSummaries[3] != "Low priority" {
		t.Errorf("third task should be Low priority (priority=3), got %q", capturedSummaries[3])
	}
}

// TestTaskCreator_Handle_IdempotentSkipWhenEpicExists is the regression test
// for boot RecoveryScan re-dispatch: an epic carrying this correlation's marker
// already exists in Jira. createJiraIssues must NOT create a duplicate epic — it
// returns an enrichment referencing the existing epic key instead.
func TestTaskCreator_Handle_IdempotentSkipWhenEpicExists(t *testing.T) {
	var createCalls int
	var searchedJQL string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/search/jql") {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
			searchedJQL, _ = body["jql"].(string)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// Pretend an epic with the marker already exists.
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"total": 1,
				"issues": []map[string]any{
					{"key": "TEST-999", "fields": map[string]any{"summary": "Existing Epic"}},
				},
			})
			return
		}
		// Any create call here is a bug.
		createCalls++
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"key": "TEST-NEW"}) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	j := jira.NewClient(srv.URL, "u", "t").WithProject("TEST")
	state := NewPlanningState()
	creator := NewTaskCreator(j, state, discardLogger())

	plan := &ProjectPlan{
		EpicTitle: "Feature Epic",
		Tasks:     []JiraTask{{Title: "Task A", Priority: 1}},
	}
	wd := state.Get("corr-dup")
	wd.mu.Lock()
	wd.Plan = plan
	wd.mu.Unlock()

	env := event.Envelope{CorrelationID: "corr-dup"}
	results, err := creator.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if createCalls != 0 {
		t.Errorf("expected zero create calls (idempotent skip), got %d", createCalls)
	}
	if !strings.Contains(searchedJQL, correlationMarker("corr-dup")) {
		t.Errorf("search JQL should key on the correlation marker %q, got %q", correlationMarker("corr-dup"), searchedJQL)
	}
	if len(results) == 0 {
		t.Fatal("expected enrichment event referencing existing epic")
	}
	var payload event.ContextEnrichmentPayload
	if err := json.Unmarshal(results[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal enrichment: %v", err)
	}
	if !strings.Contains(payload.Summary, "TEST-999") {
		t.Errorf("summary should reference existing epic key TEST-999, got %q", payload.Summary)
	}
}

// TestTaskCreator_Handle_EmbedsMarkerInEpicDescription verifies the created
// epic description carries the correlation marker so a later re-dispatch can
// find it via JQL text search.
func TestTaskCreator_Handle_EmbedsMarkerInEpicDescription(t *testing.T) {
	var epicDescText string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/search/jql") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"total": 0, "issues": []any{}}) //nolint:errcheck
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		fields := body["fields"].(map[string]any)
		if fields["issuetype"].(map[string]any)["name"].(string) == "Epic" {
			// description is ADF; stringify the whole field to substring-match.
			raw, _ := json.Marshal(fields["description"])
			epicDescText = string(raw)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"key": "TEST-100"}) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	j := jira.NewClient(srv.URL, "u", "t").WithProject("TEST")
	state := NewPlanningState()
	creator := NewTaskCreator(j, state, discardLogger())

	plan := &ProjectPlan{EpicTitle: "E", Goal: "G", Tasks: []JiraTask{{Title: "T", Priority: 1}}}
	wd := state.Get("corr-mark")
	wd.mu.Lock()
	wd.Plan = plan
	wd.mu.Unlock()

	env := event.Envelope{CorrelationID: "corr-mark"}
	if _, err := creator.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// The marker is a single unbroken alphanumeric token (Lucene tokenizes on
	// `:`/`-`), so "corr-mark" embeds as "rickcorrcorrmark".
	want := correlationMarker("corr-mark")
	if want != "rickcorrcorrmark" {
		t.Fatalf("marker form changed unexpectedly: %q", want)
	}
	if !strings.Contains(epicDescText, want) {
		t.Errorf("epic description should embed marker %q, got %q", want, epicDescText)
	}
}

func TestTaskCreator_Handle_ContinuesOnTaskFailure(t *testing.T) {
	// If one task fails, the others should still be created.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if respondEmptySearch(w, r) {
			return
		}
		callCount++
		if callCount == 2 {
			// Fail the first task
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"something went wrong"}`)) //nolint:errcheck
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"key": "TEST-OK"}) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	j := jira.NewClient(srv.URL, "u", "t").WithProject("TEST")
	state := NewPlanningState()
	creator := NewTaskCreator(j, state, discardLogger())

	plan := &ProjectPlan{
		EpicTitle: "E",
		Tasks: []JiraTask{
			{Title: "Will fail", Priority: 1, StoryPoints: 1},
			{Title: "Will succeed", Priority: 2, StoryPoints: 1},
		},
	}
	wd := state.Get("partial-corr")
	wd.mu.Lock()
	wd.Plan = plan
	wd.mu.Unlock()

	env := event.Envelope{CorrelationID: "partial-corr"}
	results, err := creator.Handle(context.Background(), env)
	// Should not return error -- partial success is acceptable
	if err != nil {
		t.Fatalf("Handle returned error on partial failure: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected enrichment event even with partial failure")
	}
}
