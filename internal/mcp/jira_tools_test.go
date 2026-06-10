package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/jira"
)

// --- toolJiraRead ---

func TestToolJiraRead_InvalidJSON(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	tool := s.tools["rick_jira_read"]
	_, err := tool.Handler(t.Context(), json.RawMessage(`{invalid`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolJiraRead_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue/PROJ-ERR", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errorMessages":["Issue does not exist"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	_, err := callTool(t, s, "rick_jira_read", map[string]any{"ticket": "PROJ-ERR"})
	if err == nil {
		t.Fatal("expected error for API 404")
	}
	if !strings.Contains(err.Error(), "fetch issue") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolJiraRead_WithLinks(t *testing.T) {
	mux := http.NewServeMux()
	// FetchIssue + FetchIssueLinks both hit /rest/api/3/issue/{key}.
	mux.HandleFunc("/rest/api/3/issue/PROJ-99", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key": "PROJ-99",
			"fields": map[string]any{
				"summary":    "Ticket with links",
				"status":     map[string]any{"name": "To Do"},
				"labels":     []string{},
				"components": []any{},
				"issueLinks": []map[string]any{
					{
						"type":         map[string]any{"name": "Blocks", "outward": "blocks", "inward": "is blocked by"},
						"outwardIssue": map[string]any{"key": "PROJ-100"},
					},
				},
			},
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

	result, err := callTool(t, s, "rick_jira_read", map[string]any{"ticket": "PROJ-99"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rm := result.(map[string]any)
	if rm["key"] != "PROJ-99" {
		t.Errorf("expected key PROJ-99, got %v", rm["key"])
	}
	// Links should be present in the result.
	if rm["links"] == nil {
		t.Error("expected links field to be populated")
	}
}

func TestToolJiraRead_WithDescriptionAndAC(t *testing.T) {
	// Covers the desc != "" and ac != "" branches in toolJiraRead.
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue/PROJ-RICH", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key": "PROJ-RICH",
			"fields": map[string]any{
				"summary":     "Rich ticket with description",
				"description": "Plain text description",
				"status":      map[string]any{"name": "In Progress"},
				"labels":      []string{"backend"},
				"components":  []any{},
				// customfield_10035 = acceptance criteria
				"customfield_10035": "Given A when B then C",
				"customfield_10036": nil,
				"issueLinks":        []any{},
			},
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

	result, err := callTool(t, s, "rick_jira_read", map[string]any{"ticket": "PROJ-RICH"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rm := result.(map[string]any)
	if rm["description"] == nil {
		t.Error("expected description to be populated")
	}
	if rm["acceptance_criteria"] == nil {
		t.Error("expected acceptance_criteria to be populated")
	}
}

func TestToolJiraRead_LinksFetchError_SilentlyIgnored(t *testing.T) {
	// When FetchIssueLinks fails, toolJiraRead should still succeed
	// (links are omitted from the result, but no error is returned).
	mux := http.NewServeMux()
	callCount := 0
	mux.HandleFunc("/rest/api/3/issue/PROJ-77", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// First call: FetchIssue (no ?fields param). Second call: FetchIssueLinks (?fields=issuelinks).
		callCount++
		if callCount == 1 {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(issueJSON("PROJ-77", "Silent links error", "To Do"))
			return
		}
		// Simulate FetchIssueLinks failure.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errorMessages":["server error"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_read", map[string]any{"ticket": "PROJ-77"})
	if err != nil {
		t.Fatalf("expected no error even when links fetch fails, got: %v", err)
	}
	rm := result.(map[string]any)
	if rm["key"] != "PROJ-77" {
		t.Errorf("expected key PROJ-77, got %v", rm["key"])
	}
	// Links should be absent (nil), not an error.
	if _, hasLinks := rm["links"]; hasLinks {
		t.Error("expected links to be absent when FetchIssueLinks fails")
	}
}

// --- toolJiraCreate ---

func TestToolJiraCreate_InvalidJSON(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	tool := s.tools["rick_jira_create"]
	_, err := tool.Handler(t.Context(), json.RawMessage(`{bad`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolJiraCreate_MissingSummary(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_jira_create", map[string]any{
		"issue_type": "Task",
	})
	if err == nil {
		t.Fatal("expected error for missing summary")
	}
	if !strings.Contains(err.Error(), "summary is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolJiraCreate_NoClient(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_jira_create", map[string]any{
		"summary": "Test ticket",
	})
	if err == nil {
		t.Fatal("expected error when Jira not configured")
	}
	if !strings.Contains(err.Error(), "jira client not configured") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolJiraCreate_DefaultsToTask(t *testing.T) {
	t.Setenv("JIRA_PROJECT", "PROJ")
	t.Setenv("JIRA_TEAM_ID", "10571")

	var capturedBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue", func(w http.ResponseWriter, r *http.Request) {
		var buf [8192]byte
		n, _ := r.Body.Read(buf[:])
		capturedBody = buf[:n]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"key":"PROJ-99"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_create", map[string]any{
		"summary": "My new task",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rm := result.(map[string]any)
	if rm["key"] != "PROJ-99" {
		t.Errorf("expected key PROJ-99, got %v", rm["key"])
	}
	if rm["created"] != true {
		t.Errorf("expected created=true")
	}
	if !strings.Contains(string(capturedBody), `"Task"`) {
		t.Errorf("expected default issue type Task in body, got: %s", capturedBody)
	}
}

func TestToolJiraCreate_MissingProject(t *testing.T) {
	t.Setenv("JIRA_TEAM_ID", "10571")
	// JIRA_PROJECT intentionally unset.

	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"key":"PROJ-1"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	_, err := callTool(t, s, "rick_jira_create", map[string]any{
		"summary": "Needs project",
	})
	if err == nil {
		t.Fatal("expected error for missing project")
	}
	if !strings.Contains(err.Error(), "project is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolJiraCreate_MissingTeam(t *testing.T) {
	t.Setenv("JIRA_PROJECT", "PROJ")
	// JIRA_TEAM_ID intentionally unset.

	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"key":"PROJ-1"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	_, err := callTool(t, s, "rick_jira_create", map[string]any{
		"summary": "Needs team",
	})
	if err == nil {
		t.Fatal("expected error for missing team")
	}
	if !strings.Contains(err.Error(), "assigned_team is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolJiraCreate_WithAllOptions(t *testing.T) {
	t.Setenv("JIRA_PROJECT", "PROJ")
	t.Setenv("JIRA_TEAM_ID", "10571")

	var capturedBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue", func(w http.ResponseWriter, r *http.Request) {
		var buf [8192]byte
		n, _ := r.Body.Read(buf[:])
		capturedBody = buf[:n]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"key":"OTHER-10"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_create", map[string]any{
		"summary":      "Bug fix",
		"issue_type":   "Bug",
		"project":      "OTHER",
		"description":  "Steps to reproduce",
		"epic_key":     "OTHER-EPIC",
		"story_points": 3,
		"labels":       []string{"backend"},
		"components":   []string{"api"},
		"priority":     "High",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rm := result.(map[string]any)
	if rm["key"] != "OTHER-10" {
		t.Errorf("expected key OTHER-10, got %v", rm["key"])
	}
	urlStr, ok := rm["url"].(string)
	if !ok || !strings.Contains(urlStr, "/browse/OTHER-10") {
		t.Errorf("expected browse URL, got %v", rm["url"])
	}
	body := string(capturedBody)
	for _, want := range []string{`"Bug"`, `"OTHER"`, `"High"`, `"backend"`, `"api"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body should contain %s, got: %s", want, body)
		}
	}
}

func TestToolJiraCreate_AssignedTeam(t *testing.T) {
	t.Setenv("JIRA_PROJECT", "PROJ")

	var capturedBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue", func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"key":"PROJ-99"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_create", map[string]any{
		"summary":       "Task with team",
		"assigned_team": "12345",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rm := result.(map[string]any)
	if rm["key"] != "PROJ-99" {
		t.Errorf("expected key PROJ-99, got %v", rm["key"])
	}
	body := string(capturedBody)
	if !strings.Contains(body, `"customfield_11533"`) || !strings.Contains(body, `"12345"`) {
		t.Errorf("body should contain team field with ID 12345, got: %s", body)
	}
}

func TestToolJiraCreate_APIError(t *testing.T) {
	t.Setenv("JIRA_PROJECT", "PROJ")
	t.Setenv("JIRA_TEAM_ID", "10571")

	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":{"summary":"required"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	_, err := callTool(t, s, "rick_jira_create", map[string]any{
		"summary": "Will fail",
	})
	if err == nil {
		t.Fatal("expected error for API 400")
	}
	if !strings.Contains(err.Error(), "create issue") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- toolJiraWrite ---

func TestToolJiraWrite_InvalidJSON(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	tool := s.tools["rick_jira_write"]
	_, err := tool.Handler(context.Background(), json.RawMessage(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("unexpected error: %v", err)
	}
}



func TestToolJiraWrite_DescriptionConvertsToADF(t *testing.T) {
	// The description field path runs jira.MarkdownToADF — verify UpdateField is called
	// with an ADF object (map), not the raw string.
	var capturedPayload []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue/PROJ-5", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			var buf [4096]byte
			n, _ := r.Body.Read(buf[:])
			capturedPayload = buf[:n]
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_write", map[string]any{
		"ticket":     "PROJ-5",
		"field_name": "description",
		"value":      "## Heading\n- item one\n- item two",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rm := result.(map[string]any)
	if rm["field_updated"] == nil {
		t.Errorf("expected field_updated to be set")
	}
	// The PUT body should contain ADF "type":"doc", not the raw markdown string.
	if !strings.Contains(string(capturedPayload), `"doc"`) {
		t.Errorf("expected ADF doc in PUT body, got: %s", capturedPayload)
	}
}

func TestToolJiraWrite_AcceptanceCriteriaMapping(t *testing.T) {
	mux := http.NewServeMux()
	var capturedPath string
	mux.HandleFunc("/rest/api/3/issue/PROJ-6", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			capturedPath = r.URL.Path
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_write", map[string]any{
		"ticket":     "PROJ-6",
		"field_name": "acceptance_criteria",
		"value":      "Given X when Y then Z",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rm := result.(map[string]any)
	if rm["field_updated"] == nil {
		t.Errorf("expected field_updated to be set")
	}
	if capturedPath != "/rest/api/3/issue/PROJ-6" {
		t.Errorf("unexpected PUT path: %s", capturedPath)
	}
}

func TestToolJiraWrite_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue/PROJ-ERR", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errorMessages":["Field not editable"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	_, err := callTool(t, s, "rick_jira_write", map[string]any{
		"ticket":     "PROJ-ERR",
		"field_name": "story_points",
		"value":      3,
	})
	if err == nil {
		t.Fatal("expected error for API 403")
	}
	if !strings.Contains(err.Error(), "update field") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolJiraWrite_UnknownFieldPassthrough(t *testing.T) {
	// Custom field IDs (not in knownFieldMap) should pass through unchanged.
	mux := http.NewServeMux()
	var capturedBody []byte
	mux.HandleFunc("/rest/api/3/issue/PROJ-7", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			var buf [4096]byte
			n, _ := r.Body.Read(buf[:])
			capturedBody = buf[:n]
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_jira_write", map[string]any{
		"ticket":     "PROJ-7",
		"field_name": "customfield_99999",
		"value":      "custom value",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rm := result.(map[string]any)
	if rm["field_updated"] == nil {
		t.Errorf("expected field_updated to be set")
	}
	if !strings.Contains(string(capturedBody), "customfield_99999") {
		t.Errorf("expected custom field in PUT body, got: %s", capturedBody)
	}
}

// --- toolJiraTransition ---

func TestToolJiraTransitionWrite_InvalidJSON(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	tool := s.tools["rick_jira_write"]
	_, err := tool.Handler(context.Background(), json.RawMessage(`{bad`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("unexpected error: %v", err)
	}
}



func TestToolJiraTransitionWrite_APIError_TransitionNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue/PROJ-1/transitions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"transitions": []map[string]any{
					{"id": "11", "to": map[string]any{"name": "To Do"}},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	_, err := callTool(t, s, "rick_jira_write", map[string]any{
		"ticket": "PROJ-1",
		"status": "NONEXISTENT STATUS",
	})
	if err == nil {
		t.Fatal("expected error when transition not found")
	}
	if !strings.Contains(err.Error(), "transition") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- toolJiraComment ---

func TestToolJiraCommentWrite_InvalidJSON(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	tool := s.tools["rick_jira_write"]
	_, err := tool.Handler(context.Background(), json.RawMessage(`{bad json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("unexpected error: %v", err)
	}
}



func TestToolJiraCommentWrite_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/issue/PROJ-ERR/comment", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errorMessages":["Internal error"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	_, err := callTool(t, s, "rick_jira_write", map[string]any{
		"ticket":  "PROJ-ERR",
		"comment": "test comment",
	})
	if err == nil {
		t.Fatal("expected error for API 500")
	}
	if !strings.Contains(err.Error(), "add comment") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- toolJiraEpicIssues ---

func TestToolJiraEpicIssues_InvalidJSON(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	tool := s.tools["rick_jira_read"]
	_, err := tool.Handler(context.Background(), json.RawMessage(`not json at all`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("unexpected error: %v", err)
	}
}







// --- toolJiraSearch ---

func TestToolJiraSearch_InvalidJSON(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	tool := s.tools["rick_jira_search"]
	_, err := tool.Handler(t.Context(), json.RawMessage(`{`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolJiraSearch_MissingJQL(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_jira_search", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing jql")
	}
	if !strings.Contains(err.Error(), "jql is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolJiraSearch_DefaultLimit(t *testing.T) {
	// When limit is 0 or omitted, should default to 50.
	mux := http.NewServeMux()
	var capturedBody []byte
	mux.HandleFunc("/rest/api/3/search/jql", func(w http.ResponseWriter, r *http.Request) {
		var buf [4096]byte
		n, _ := r.Body.Read(buf[:])
		capturedBody = buf[:n]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total":  0,
			"issues": []any{},
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

	result, err := callTool(t, s, "rick_jira_search", map[string]any{
		"jql": "project = TEST",
		// limit omitted — should default to 50
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rm := result.(map[string]any)
	if rm["count"] != 0 {
		t.Errorf("expected count=0, got %v", rm["count"])
	}
	if !strings.Contains(string(capturedBody), "50") {
		t.Errorf("expected default maxResults=50 in request body, got: %s", capturedBody)
	}
}

func TestToolJiraSearch_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/3/search/jql", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errorMessages":["Bad JQL query"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := jira.NewClient(srv.URL, "test", "tok")
	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	_, err := callTool(t, s, "rick_jira_search", map[string]any{
		"jql": "invalid JQL @@@@",
	})
	if err == nil {
		t.Fatal("expected error for API 400")
	}
	if !strings.Contains(err.Error(), "jira search") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- toolJiraLink ---

