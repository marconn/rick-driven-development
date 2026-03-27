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

// --- MarkdownToADF ---

func TestMarkdownToADF_PlainText(t *testing.T) {
	adf := MarkdownToADF("hello world")

	if adf["type"] != "doc" {
		t.Errorf("type=%v, want doc", adf["type"])
	}
	content := adf["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content len=%d, want 1", len(content))
	}
	para := content[0].(map[string]any)
	if para["type"] != "paragraph" {
		t.Errorf("content[0].type=%v, want paragraph", para["type"])
	}
	inner := para["content"].([]any)
	textNode := inner[0].(map[string]any)
	if textNode["text"] != "hello world" {
		t.Errorf("text=%v, want 'hello world'", textNode["text"])
	}
}

func TestMarkdownToADF_BoldText(t *testing.T) {
	adf := MarkdownToADF("**Objetivo:** crear API")
	content := adf["content"].([]any)
	para := content[0].(map[string]any)
	inner := para["content"].([]any)

	// Should have: bold("Objetivo:"), text(" crear API")
	if len(inner) != 2 {
		t.Fatalf("inline nodes=%d, want 2", len(inner))
	}
	boldNode := inner[0].(map[string]any)
	if boldNode["text"] != "Objetivo:" {
		t.Errorf("bold text=%v, want 'Objetivo:'", boldNode["text"])
	}
	marks := boldNode["marks"].([]any)
	mark := marks[0].(map[string]any)
	if mark["type"] != "strong" {
		t.Errorf("mark type=%v, want strong", mark["type"])
	}
	plainNode := inner[1].(map[string]any)
	if plainNode["text"] != " crear API" {
		t.Errorf("plain text=%v, want ' crear API'", plainNode["text"])
	}
}

func TestMarkdownToADF_BulletList(t *testing.T) {
	input := "Intro text\n\n- item one\n- item two\n- item three"
	adf := MarkdownToADF(input)
	content := adf["content"].([]any)

	// Should have: paragraph, bulletList
	if len(content) != 2 {
		t.Fatalf("top-level nodes=%d, want 2", len(content))
	}
	if content[0].(map[string]any)["type"] != "paragraph" {
		t.Errorf("node[0] type=%v, want paragraph", content[0].(map[string]any)["type"])
	}
	bulletList := content[1].(map[string]any)
	if bulletList["type"] != "bulletList" {
		t.Errorf("node[1] type=%v, want bulletList", bulletList["type"])
	}
	items := bulletList["content"].([]any)
	if len(items) != 3 {
		t.Errorf("bullet items=%d, want 3", len(items))
	}
}

func TestMarkdownToADF_HeadingAndMixed(t *testing.T) {
	input := "## Riesgos\n\n- riesgo uno\n- riesgo dos\n\nConclusion final"
	adf := MarkdownToADF(input)
	content := adf["content"].([]any)

	// heading, bulletList, paragraph
	if len(content) != 3 {
		t.Fatalf("top-level nodes=%d, want 3", len(content))
	}
	heading := content[0].(map[string]any)
	if heading["type"] != "heading" {
		t.Errorf("node[0] type=%v, want heading", heading["type"])
	}
	attrs := heading["attrs"].(map[string]any)
	if attrs["level"] != 2 {
		t.Errorf("heading level=%v, want 2", attrs["level"])
	}
	if content[1].(map[string]any)["type"] != "bulletList" {
		t.Error("node[1] should be bulletList")
	}
	if content[2].(map[string]any)["type"] != "paragraph" {
		t.Error("node[2] should be paragraph")
	}
}

func TestMarkdownToADF_BoldInBullets(t *testing.T) {
	input := "- **Importante:** hacer esto\n- Normal"
	adf := MarkdownToADF(input)
	content := adf["content"].([]any)
	bulletList := content[0].(map[string]any)
	items := bulletList["content"].([]any)
	firstItem := items[0].(map[string]any)
	para := firstItem["content"].([]any)[0].(map[string]any)
	inline := para["content"].([]any)

	// Should have bold + plain text
	if len(inline) < 2 {
		t.Fatalf("inline nodes in bullet=%d, want >=2", len(inline))
	}
	boldNode := inline[0].(map[string]any)
	if boldNode["text"] != "Importante:" {
		t.Errorf("bold text=%v, want 'Importante:'", boldNode["text"])
	}
}

func TestMarkdownToADF_RoundTrip(t *testing.T) {
	// Verify complex ADF marshals to valid JSON
	input := "**Objetivo:** test\n\n## Riesgos\n\n- **alto:** risk 1\n- bajo: risk 2\n\nFin"
	adf := MarkdownToADF(input)
	_, err := json.Marshal(adf)
	if err != nil {
		t.Errorf("MarkdownToADF result is not JSON-serializable: %v", err)
	}
}

func TestMarkdownToADF_EmptyInput(t *testing.T) {
	adf := MarkdownToADF("")
	content := adf["content"].([]any)
	if len(content) == 0 {
		t.Error("empty input should produce at least one paragraph")
	}
}

// --- parseInlineMarks ---

func TestParseInlineMarks_NoBold(t *testing.T) {
	nodes := parseInlineMarks("plain text")
	if len(nodes) != 1 {
		t.Fatalf("nodes=%d, want 1", len(nodes))
	}
	n := nodes[0].(map[string]any)
	if n["text"] != "plain text" {
		t.Errorf("text=%v, want 'plain text'", n["text"])
	}
	if _, hasMark := n["marks"]; hasMark {
		t.Error("plain text should not have marks")
	}
}

func TestParseInlineMarks_OnlyBold(t *testing.T) {
	nodes := parseInlineMarks("**all bold**")
	if len(nodes) != 1 {
		t.Fatalf("nodes=%d, want 1", len(nodes))
	}
	n := nodes[0].(map[string]any)
	if n["text"] != "all bold" {
		t.Errorf("text=%v, want 'all bold'", n["text"])
	}
	marks := n["marks"].([]any)
	if marks[0].(map[string]any)["type"] != "strong" {
		t.Error("expected strong mark")
	}
}

func TestParseInlineMarks_UnmatchedBold(t *testing.T) {
	nodes := parseInlineMarks("text **unclosed")
	if len(nodes) != 2 {
		t.Fatalf("nodes=%d, want 2", len(nodes))
	}
	// "text " plain, then "**unclosed" as literal
	n0 := nodes[0].(map[string]any)
	if n0["text"] != "text " {
		t.Errorf("first node text=%v, want 'text '", n0["text"])
	}
	n1 := nodes[1].(map[string]any)
	if n1["text"] != "**unclosed" {
		t.Errorf("second node text=%v, want '**unclosed'", n1["text"])
	}
}

