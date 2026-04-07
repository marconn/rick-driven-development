package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestJiraClient(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := &Client{
		baseURL:    srv.URL,
		email:      "test@example.com",
		token:      "token123",
		project:    "TEST",
		teamID:     "10571",
		httpClient: &http.Client{},
	}
	return srv, client
}

func TestClient_CreateEpic_RequestShape(t *testing.T) {
	var capturedBody map[string]any
	var capturedAuth string

	_, client := newTestJiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"key":"TEST-1","id":"10001"}`)) //nolint:errcheck
	})

	key, err := client.CreateEpic(context.Background(), "My Epic", "Epic description")
	if err != nil {
		t.Fatalf("CreateEpic: %v", err)
	}
	if key != "TEST-1" {
		t.Errorf("key=%q, want TEST-1", key)
	}

	// Verify basic auth is sent
	if !strings.HasPrefix(capturedAuth, "Basic ") {
		t.Errorf("expected Basic auth, got %q", capturedAuth)
	}

	// Verify fields structure
	fields, ok := capturedBody["fields"].(map[string]any)
	if !ok {
		t.Fatal("request body missing 'fields' object")
	}
	if issuetype, _ := fields["issuetype"].(map[string]any); issuetype["name"] != "Epic" {
		t.Errorf("issuetype.name=%v, want Epic", issuetype["name"])
	}
	if project, _ := fields["project"].(map[string]any); project["key"] != "TEST" {
		t.Errorf("project.key=%v, want TEST", project["key"])
	}
	if fields["summary"] != "My Epic" {
		t.Errorf("summary=%v, want 'My Epic'", fields["summary"])
	}
	// Team field must be set
	if team, _ := fields["customfield_11533"].(map[string]any); team["id"] != "10571" {
		t.Errorf("customfield_11533.id=%v, want 10571", team["id"])
	}
	// Epic name field
	if fields["customfield_10201"] != "My Epic" {
		t.Errorf("customfield_10201=%v, want 'My Epic'", fields["customfield_10201"])
	}
	// Description must be ADF
	desc, _ := fields["description"].(map[string]any)
	if desc["type"] != "doc" {
		t.Errorf("description.type=%v, want doc", desc["type"])
	}
}

func TestClient_CreateTask_RequestShape(t *testing.T) {
	var capturedBody map[string]any

	_, client := newTestJiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"key":"TEST-42"}`)) //nolint:errcheck
	})

	key, err := client.CreateTask(context.Background(), "TEST-1", "Do the thing", "Description", 5)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if key != "TEST-42" {
		t.Errorf("key=%q, want TEST-42", key)
	}

	fields, ok := capturedBody["fields"].(map[string]any)
	if !ok {
		t.Fatal("missing 'fields' in request body")
	}
	if issuetype, _ := fields["issuetype"].(map[string]any); issuetype["name"] != "Task" {
		t.Errorf("issuetype.name=%v, want Task", issuetype["name"])
	}
	// Epic link
	if fields["customfield_10200"] != "TEST-1" {
		t.Errorf("customfield_10200=%v, want TEST-1", fields["customfield_10200"])
	}
	// Story points
	if fields["customfield_10004"] != float64(5) {
		t.Errorf("customfield_10004=%v, want 5", fields["customfield_10004"])
	}
}

func TestClient_CreateTask_ZeroPointsOmitted(t *testing.T) {
	var capturedBody map[string]any

	_, client := newTestJiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"key":"TEST-2"}`)) //nolint:errcheck
	})

	_, err := client.CreateTask(context.Background(), "TEST-1", "Task", "Desc", 0)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	fields := capturedBody["fields"].(map[string]any)
	if _, exists := fields["customfield_10004"]; exists {
		t.Error("customfield_10004 should be omitted when story_points=0")
	}
}

func TestClient_CreateEpic_EmptyTeamID_OmitsField(t *testing.T) {
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"key":"TEST-3"}`)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	client := &Client{
		baseURL:    srv.URL,
		email:      "u",
		token:      "t",
		project:    "TEST",
		teamID:     "", // empty team ID
		httpClient: &http.Client{},
	}
	_, err := client.CreateEpic(context.Background(), "E", "D")
	if err != nil {
		t.Fatalf("CreateEpic: %v", err)
	}

	fields := capturedBody["fields"].(map[string]any)
	if _, exists := fields["customfield_11533"]; exists {
		t.Error("customfield_11533 should be omitted when teamID is empty")
	}
}

func TestClient_CreateEpic_HTTPError(t *testing.T) {
	_, client := newTestJiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"errorMessages":["Unauthorized"]}`)) //nolint:errcheck
	})

	_, err := client.CreateEpic(context.Background(), "Epic", "Desc")
	if err == nil {
		t.Fatal("expected error for HTTP 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status 401: %v", err)
	}
}

// --- LinkIssues ---

func TestClient_LinkIssues_RequestShape(t *testing.T) {
	var capturedBody map[string]any

	_, client := newTestJiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/api/3/issueLink" {
			json.NewDecoder(r.Body).Decode(&capturedBody) //nolint:errcheck
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"key":"TEST-1"}`)) //nolint:errcheck
	})

	err := client.LinkIssues(context.Background(), "TEST-1", "TEST-2")
	if err != nil {
		t.Fatalf("LinkIssues: %v", err)
	}

	linkType, _ := capturedBody["type"].(map[string]any)
	if linkType["name"] != "Blocks" {
		t.Errorf("link type=%v, want Blocks", linkType["name"])
	}
	outward, _ := capturedBody["outwardIssue"].(map[string]any)
	if outward["key"] != "TEST-1" {
		t.Errorf("outwardIssue.key=%v, want TEST-1 (blocker)", outward["key"])
	}
	inward, _ := capturedBody["inwardIssue"].(map[string]any)
	if inward["key"] != "TEST-2" {
		t.Errorf("inwardIssue.key=%v, want TEST-2 (blocked)", inward["key"])
	}
}

func TestClient_LinkIssues_HTTPError(t *testing.T) {
	_, client := newTestJiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"errorMessages":["bad link"]}`)) //nolint:errcheck
	})

	err := client.LinkIssues(context.Background(), "TEST-1", "TEST-2")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention status 400: %v", err)
	}
}

// --- DeleteIssueLink ---

func TestClient_DeleteIssueLink_Success(t *testing.T) {
	var capturedPath string

	_, client := newTestJiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		if r.Method != http.MethodDelete {
			t.Errorf("method=%s, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	err := client.DeleteIssueLink(context.Background(), "12345")
	if err != nil {
		t.Fatalf("DeleteIssueLink: %v", err)
	}
	if capturedPath != "/rest/api/3/issueLink/12345" {
		t.Errorf("path=%s, want /rest/api/3/issueLink/12345", capturedPath)
	}
}

func TestClient_DeleteIssueLink_HTTPError(t *testing.T) {
	_, client := newTestJiraClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"errorMessages":["Link not found"]}`)) //nolint:errcheck
	})

	err := client.DeleteIssueLink(context.Background(), "99999")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status 404: %v", err)
	}
}

// MarkdownToADF is now a thin wrapper around internal/adf.FromMarkdown — all
// converter coverage lives in internal/adf/convert_test.go (headings, lists,
// bold/italic/code/strike marks, links, fenced code, tables, soft-break
// regression for HULI-33546). One smoke test here guards the wrapper.

func TestMarkdownToADF_DelegatesToADFPackage(t *testing.T) {
	doc := MarkdownToADF("**Objetivo:** crear API\n\n## Riesgos\n- alto\n- bajo")
	if doc["type"] != "doc" {
		t.Errorf("type=%v, want doc", doc["type"])
	}
	if _, err := json.Marshal(doc); err != nil {
		t.Errorf("not JSON-serializable: %v", err)
	}
	content := doc["content"].([]any)
	if len(content) < 3 {
		t.Errorf("expected paragraph + heading + bulletList, got %d nodes", len(content))
	}
}

