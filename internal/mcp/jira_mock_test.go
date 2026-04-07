package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/jira"
)

// mockJiraServer creates a fake Jira REST API server that returns hardcoded responses.
// It handles the endpoints used by the MCP Jira tools.
type mockJiraServer struct {
	mux    *http.ServeMux
	server *httptest.Server
}

func newMockJiraServer(t *testing.T) (*mockJiraServer, *jira.Client) {
	t.Helper()
	mux := http.NewServeMux()

	srv := &mockJiraServer{mux: mux}
	srv.server = httptest.NewServer(mux)
	t.Cleanup(srv.server.Close)

	client := jira.NewClient(srv.server.URL, "test@example.com", "token")
	return srv, client
}

// handleJSON registers a route that returns the given JSON response.
func (m *mockJiraServer) handleJSON(pattern string, code int, body any) {
	m.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})
}

// issueJSON builds a Jira issue response body for mock HTTP handlers.
func issueJSON(key, summary, status string) map[string]any {
	return map[string]any{
		"key": key,
		"fields": map[string]any{
			"summary":     summary,
			"description": nil,
			"status":      map[string]any{"name": status},
			"labels":      []string{},
			"components":  []any{},
			"issueLinks":  []any{},
		},
	}
}

// --- Tests with mock Jira ---

func TestToolJiraRead_WithMockServer(t *testing.T) {
	mockSrv, client := newMockJiraServer(t)

	// FetchIssue and FetchIssueLinks both hit the same endpoint —
	// register once with a handler that returns issue + empty links.
	mockSrv.mux.HandleFunc("/rest/api/3/issue/PROJ-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(issueJSON("PROJ-1", "Add clinic endpoint", "In Development"))
	})

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_read", map[string]any{"ticket": "PROJ-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if rm["key"] != "PROJ-1" {
		t.Errorf("expected key PROJ-1, got %v", rm["key"])
	}
	if rm["summary"] != "Add clinic endpoint" {
		t.Errorf("expected summary, got %v", rm["summary"])
	}
}

func TestToolJiraReadQASteps_WithMockServer(t *testing.T) {
	mockSrv, client := newMockJiraServer(t)

	// Custom field returns ADF document with QA steps text.
	mockSrv.mux.HandleFunc("/rest/api/3/issue/PROJ-9", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key": "PROJ-9",
			"fields": map[string]any{
				"customfield_10037": map[string]any{
					"version": 1,
					"type":    "doc",
					"content": []map[string]any{
						{
							"type": "paragraph",
							"content": []map[string]any{
								{"type": "text", "text": "Step 1: open the page"},
							},
						},
						{
							"type": "paragraph",
							"content": []map[string]any{
								{"type": "text", "text": "Step 2: click submit"},
							},
						},
					},
				},
			},
		})
	})

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_read_qa_steps", map[string]any{"ticket": "PROJ-9"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if rm["ticket"] != "PROJ-9" {
		t.Errorf("expected ticket PROJ-9, got %v", rm["ticket"])
	}
	if rm["field_id"] != "customfield_10037" {
		t.Errorf("expected field_id customfield_10037, got %v", rm["field_id"])
	}
	if rm["present"] != true {
		t.Errorf("expected present=true, got %v", rm["present"])
	}
	steps, _ := rm["qa_steps"].(string)
	if !strings.Contains(steps, "Step 1: open the page") || !strings.Contains(steps, "Step 2: click submit") {
		t.Errorf("expected qa_steps to contain both steps, got %q", steps)
	}
}

func TestToolJiraReadQASteps_FieldMissing(t *testing.T) {
	mockSrv, client := newMockJiraServer(t)

	// Issue with no QA Steps field set.
	mockSrv.mux.HandleFunc("/rest/api/3/issue/PROJ-10", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key":    "PROJ-10",
			"fields": map[string]any{},
		})
	})

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_read_qa_steps", map[string]any{"ticket": "PROJ-10"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rm, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if rm["present"] != false {
		t.Errorf("expected present=false, got %v", rm["present"])
	}
	if rm["qa_steps"] != "" {
		t.Errorf("expected empty qa_steps, got %v", rm["qa_steps"])
	}
}

func TestToolJiraReadQASteps_CustomFieldID(t *testing.T) {
	mockSrv, client := newMockJiraServer(t)

	mockSrv.mux.HandleFunc("/rest/api/3/issue/PROJ-11", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key": "PROJ-11",
			"fields": map[string]any{
				"customfield_99999": "plain string steps",
			},
		})
	})

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_read_qa_steps", map[string]any{
		"ticket":   "PROJ-11",
		"field_id": "customfield_99999",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rm, _ := result.(map[string]any)
	if rm["field_id"] != "customfield_99999" {
		t.Errorf("expected field_id customfield_99999, got %v", rm["field_id"])
	}
	if rm["qa_steps"] != "plain string steps" {
		t.Errorf("expected plain string steps, got %v", rm["qa_steps"])
	}
}

func TestToolJiraWrite_WithMockServer(t *testing.T) {
	mockSrv, client := newMockJiraServer(t)
	mockSrv.handleJSON("/rest/api/3/issue/PROJ-1", http.StatusNoContent, nil)

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_write", map[string]any{
		"ticket":     "PROJ-1",
		"field_name": "story_points",
		"value":      5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if rm["updated"] != true {
		t.Errorf("expected updated=true")
	}
}

func TestToolJiraTransition_WithMockServer(t *testing.T) {
	mockSrv, client := newMockJiraServer(t)

	// GET returns available transitions, POST executes the transition.
	// The Jira client matches by t.To.Name, so we must nest the status in "to".
	mockSrv.mux.HandleFunc("/rest/api/3/issue/PROJ-1/transitions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"transitions": []map[string]any{
					{"id": "11", "to": map[string]any{"name": "IN DEVELOPMENT"}},
					{"id": "21", "to": map[string]any{"name": "Done"}},
				},
			})
			return
		}
		// POST: do the transition
		w.WriteHeader(http.StatusNoContent)
	})

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_transition", map[string]any{
		"ticket": "PROJ-1",
		"status": "IN DEVELOPMENT",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if rm["transitioned"] != true {
		t.Errorf("expected transitioned=true")
	}
}

func TestToolJiraComment_WithMockServer(t *testing.T) {
	mockSrv, client := newMockJiraServer(t)
	mockSrv.handleJSON("/rest/api/3/issue/PROJ-1/comment", http.StatusCreated,
		map[string]any{"id": "10001"})

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_comment", map[string]any{
		"ticket":  "PROJ-1",
		"comment": "This is a test comment",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if rm["commented"] != true {
		t.Errorf("expected commented=true")
	}
}

func TestToolJiraSearch_WithMockServer(t *testing.T) {
	mockSrv, client := newMockJiraServer(t)
	mockSrv.handleJSON("/rest/api/3/search/jql", http.StatusOK, map[string]any{
		"total": 2,
		"issues": []map[string]any{
			{"key": "PROJ-1", "fields": map[string]any{"summary": "Task one"}},
			{"key": "PROJ-2", "fields": map[string]any{"summary": "Task two"}},
		},
	})

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_search", map[string]any{
		"jql":   "project = PROJ",
		"limit": 10,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if rm["count"] != 2 {
		t.Errorf("expected count=2, got %v", rm["count"])
	}
	if rm["total"] != 2 {
		t.Errorf("expected total=2, got %v", rm["total"])
	}
}

func TestToolJiraEpicIssues_WithMockServer(t *testing.T) {
	mockSrv, client := newMockJiraServer(t)
	mockSrv.handleJSON("/rest/api/3/search/jql", http.StatusOK, map[string]any{
		"total": 1,
		"issues": []map[string]any{
			{
				"key": "PROJ-5",
				"fields": map[string]any{
					"summary":         "Child task",
					"status":          map[string]any{"name": "To Do"},
					"labels":          []string{"repo:myapp"},
					"customfield_10004": 3.0,
					"issuetype":       map[string]any{"name": "Task"},
				},
			},
		},
	})

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_epic_issues", map[string]any{
		"epic": "PROJ-EPIC",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if rm["count"] != 1 {
		t.Errorf("expected count=1, got %v", rm["count"])
	}
}

func TestToolJiraCreate_WithMockServer(t *testing.T) {
	mockSrv, client := newMockJiraServer(t)
	mockSrv.handleJSON("/rest/api/3/issue", http.StatusCreated,
		map[string]any{"key": "PROJ-NEW"})

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_create", map[string]any{
		"summary":    "New feature",
		"issue_type": "Story",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if rm["key"] != "PROJ-NEW" {
		t.Errorf("expected key PROJ-NEW, got %v", rm["key"])
	}
	if rm["created"] != true {
		t.Errorf("expected created=true")
	}
}

func TestToolJiraLink_WithMockServer(t *testing.T) {
	mockSrv, client := newMockJiraServer(t)
	mockSrv.handleJSON("/rest/api/3/issueLink", http.StatusCreated, nil)

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_link", map[string]any{
		"from_ticket": "PROJ-1",
		"to_ticket":   "PROJ-2",
		"link_type":   "Blocks",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rm, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if rm["linked"] != true {
		t.Errorf("expected linked=true")
	}
}

// --- computeWavePlan tests ---

// mockComputeWavePlanJira returns a full mock server for computeWavePlan.
// epicChildren: PROJ-1 (no deps), PROJ-2 (blocked by PROJ-1), PROJ-3 (no deps)
func mockComputeWavePlanJira(t *testing.T) (*jira.Client, func()) {
	t.Helper()
	callCount := 0
	mux := http.NewServeMux()

	// /rest/api/3/search/jql — returns epic children
	mux.HandleFunc("/rest/api/3/search/jql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total": 3,
			"issues": []map[string]any{
				{
					"key": "PROJ-1",
					"fields": map[string]any{
						"summary":         "Task A (no deps)",
						"status":          map[string]any{"name": "To Do"},
						"labels":          []string{},
						"customfield_10004": 3.0,
						"issuetype":       map[string]any{"name": "Task"},
					},
				},
				{
					"key": "PROJ-2",
					"fields": map[string]any{
						"summary":         "Task B (blocked by PROJ-1)",
						"status":          map[string]any{"name": "To Do"},
						"labels":          []string{},
						"customfield_10004": 5.0,
						"issuetype":       map[string]any{"name": "Task"},
					},
				},
				{
					"key": "PROJ-3",
					"fields": map[string]any{
						"summary":         "Task C (no deps)",
						"status":          map[string]any{"name": "To Do"},
						"labels":          []string{},
						"customfield_10004": 2.0,
						"issuetype":       map[string]any{"name": "Task"},
					},
				},
			},
		})
	})

	// /rest/api/3/issue/{key} for link fetching
	mux.HandleFunc("/rest/api/3/issue/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
		// Only return links for PROJ-2.
		var links []map[string]any
		if key == "PROJ-2" {
			links = []map[string]any{
				{
					"type": map[string]any{"name": "Blocks"},
					"inwardIssue": map[string]any{"key": "PROJ-1"},
				},
			}
		}
		_ = callCount
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key":    key,
			"fields": map[string]any{"issueLinks": links},
		})
	})

	srv := httptest.NewServer(mux)
	client := jira.NewClient(srv.URL, "test@example.com", "token")
	return client, srv.Close
}

func TestComputeWavePlan_SimpleDAG(t *testing.T) {
	client, stop := mockComputeWavePlanJira(t)
	defer stop()

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := s.computeWavePlan(context.Background(), "PROJ-EPIC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Epic != "PROJ-EPIC" {
		t.Errorf("expected epic PROJ-EPIC, got %q", result.Epic)
	}
	if len(result.Waves) < 2 {
		t.Errorf("expected at least 2 waves (PROJ-1+PROJ-3 in wave 1, PROJ-2 in wave 2), got %d", len(result.Waves))
	}
	if result.TotalPoints != 10.0 {
		t.Errorf("expected total_points=10, got %.1f", result.TotalPoints)
	}

	// Wave 1 should contain PROJ-1 and PROJ-3 (independent).
	wave1 := result.Waves[0]
	if wave1.Wave != 1 {
		t.Errorf("expected first wave=1, got %d", wave1.Wave)
	}
	if len(wave1.Tickets) != 2 {
		t.Errorf("expected 2 tickets in wave 1, got %d", len(wave1.Tickets))
	}

	// Wave 2 should contain PROJ-2 (depends on PROJ-1).
	wave2 := result.Waves[1]
	if len(wave2.Tickets) != 1 || wave2.Tickets[0].Key != "PROJ-2" {
		t.Errorf("expected PROJ-2 in wave 2, got %+v", wave2.Tickets)
	}
}

func TestComputeWavePlan_AllIndependent(t *testing.T) {
	// All tasks with no deps → single wave.
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/search/jql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total": 2,
			"issues": []map[string]any{
				{"key": "PROJ-A", "fields": map[string]any{"summary": "A", "status": map[string]any{"name": "To Do"}, "labels": []string{}, "issuetype": map[string]any{"name": "Task"}}},
				{"key": "PROJ-B", "fields": map[string]any{"summary": "B", "status": map[string]any{"name": "To Do"}, "labels": []string{}, "issuetype": map[string]any{"name": "Task"}}},
			},
		})
	})
	// Issue links — no deps
	mux.HandleFunc("/rest/api/3/issue/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key":    "dummy",
			"fields": map[string]any{"issueLinks": []any{}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := s.computeWavePlan(context.Background(), "EPIC-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Waves) != 1 {
		t.Errorf("expected 1 wave for independent tasks, got %d", len(result.Waves))
	}
	if result.Parallelism != 2 {
		t.Errorf("expected parallelism=2, got %d", result.Parallelism)
	}
}

func TestComputeWavePlan_LinearChain(t *testing.T) {
	// A → B → C: 3 waves.
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/search/jql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issues": []map[string]any{
				{"key": "T-1", "fields": map[string]any{"summary": "A", "status": map[string]any{"name": "To Do"}, "labels": []string{}, "issuetype": map[string]any{"name": "Task"}}},
				{"key": "T-2", "fields": map[string]any{"summary": "B", "status": map[string]any{"name": "To Do"}, "labels": []string{}, "issuetype": map[string]any{"name": "Task"}}},
				{"key": "T-3", "fields": map[string]any{"summary": "C", "status": map[string]any{"name": "To Do"}, "labels": []string{}, "issuetype": map[string]any{"name": "Task"}}},
			},
		})
	})
	mux.HandleFunc("/rest/api/3/issue/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
		var links []map[string]any
		switch key {
		case "T-2":
			links = []map[string]any{{"type": map[string]any{"name": "Blocks"}, "inwardIssue": map[string]any{"key": "T-1"}}}
		case "T-3":
			links = []map[string]any{{"type": map[string]any{"name": "Blocks"}, "inwardIssue": map[string]any{"key": "T-2"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key":    key,
			"fields": map[string]any{"issueLinks": links},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := s.computeWavePlan(context.Background(), "EPIC-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Waves) != 3 {
		t.Errorf("expected 3 waves for chain A→B→C, got %d: %+v", len(result.Waves), result.Waves)
	}
}

func TestComputeWavePlan_CircularDependency(t *testing.T) {
	// A→B→A: circular, should not infinite loop — all assigned to wave 1.
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/search/jql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issues": []map[string]any{
				{"key": "C-1", "fields": map[string]any{"summary": "A", "status": map[string]any{"name": "To Do"}, "labels": []string{}, "issuetype": map[string]any{"name": "Task"}}},
				{"key": "C-2", "fields": map[string]any{"summary": "B", "status": map[string]any{"name": "To Do"}, "labels": []string{}, "issuetype": map[string]any{"name": "Task"}}},
			},
		})
	})
	mux.HandleFunc("/rest/api/3/issue/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
		var links []map[string]any
		switch key {
		case "C-1":
			// C-1 blocked by C-2
			links = []map[string]any{{"type": map[string]any{"name": "Blocks"}, "inwardIssue": map[string]any{"key": "C-2"}}}
		case "C-2":
			// C-2 blocked by C-1
			links = []map[string]any{{"type": map[string]any{"name": "Blocks"}, "inwardIssue": map[string]any{"key": "C-1"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key":    key,
			"fields": map[string]any{"issueLinks": links},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	// Should not hang or panic.
	result, err := s.computeWavePlan(context.Background(), "EPIC-CIRCULAR")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// All tickets must be assigned (circular resolved by fallback).
	totalTickets := 0
	for _, w := range result.Waves {
		totalTickets += len(w.Tickets)
	}
	if totalTickets != 2 {
		t.Errorf("expected 2 total tickets assigned, got %d", totalTickets)
	}
}

// --- toolPlanBTU: extractPageID validation ---

func TestToolPlanBTU_ExtractPageIDFromURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"12345", "12345"},
		{"https://wiki.example.com/wiki/spaces/ENG/pages/987654321/My+Page", "987654321"},
		{"https://example.atlassian.net/wiki/pages/42/Title", "42"},
	}
	for _, tc := range tests {
		got := extractPageID(tc.input)
		if got != tc.want {
			t.Errorf("extractPageID(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// Ensure the fmt import is used.
var _ = fmt.Sprintf
