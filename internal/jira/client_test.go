package jira

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewClientFromEnvMissing(t *testing.T) {
	t.Setenv("JIRA_URL", "")
	t.Setenv("JIRA_EMAIL", "")
	t.Setenv("JIRA_TOKEN", "")

	cli := NewClientFromEnv()
	if cli != nil {
		t.Error("want nil when env vars are missing")
	}
}

func TestNewClientFromEnvPresent(t *testing.T) {
	t.Setenv("JIRA_URL", "https://example.atlassian.net")
	t.Setenv("JIRA_EMAIL", "user@test.com")
	t.Setenv("JIRA_TOKEN", "tok")

	cli := NewClientFromEnv()
	if cli == nil {
		t.Fatal("want non-nil client")
	}
	if cli.baseURL != "https://example.atlassian.net" {
		t.Errorf("want baseURL without trailing slash, got %q", cli.baseURL)
	}
}

func TestFetchIssue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "a@b.com" || pass != "tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"key": "TEST-1",
			"fields": {
				"summary": "Fix bug",
				"status": {"name": "Open"},
				"labels": ["repo:myrepo"],
				"components": [{"name": "backend"}]
			}
		}`))
	}))
	defer srv.Close()

	cli := NewClient(srv.URL, "a@b.com", "tok")
	issue, err := cli.FetchIssue(context.Background(), "TEST-1")
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	if issue.Fields.Summary != "Fix bug" {
		t.Errorf("want summary 'Fix bug', got %q", issue.Fields.Summary)
	}
	if len(issue.Fields.Labels) != 1 || issue.Fields.Labels[0] != "repo:myrepo" {
		t.Errorf("want labels [repo:myrepo], got %v", issue.Fields.Labels)
	}
	if names := issue.ComponentNames(); len(names) != 1 || names[0] != "backend" {
		t.Errorf("want components [backend], got %v", names)
	}
}

func TestFetchIssue404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cli := NewClient(srv.URL, "a@b.com", "tok")
	_, err := cli.FetchIssue(context.Background(), "NOPE-1")
	if err == nil {
		t.Fatal("want error for 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention 404, got: %v", err)
	}
}

func TestUpdateField(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cli := NewClient(srv.URL, "a@b.com", "tok")
	err := cli.UpdateField(context.Background(), "TEST-1", "customfield_10037", "hello")
	if err != nil {
		t.Fatalf("UpdateField: %v", err)
	}
	if !strings.Contains(gotBody, "customfield_10037") {
		t.Errorf("body should contain field ID, got: %s", gotBody)
	}
}

func TestADFToPlainTextString(t *testing.T) {
	if got := ADFToPlainText("hello"); got != "hello" {
		t.Errorf("want 'hello', got %q", got)
	}
}

func TestADFToPlainTextNil(t *testing.T) {
	if got := ADFToPlainText(nil); got != "" {
		t.Errorf("want empty, got %q", got)
	}
}

func TestADFToPlainTextADFObject(t *testing.T) {
	doc := map[string]any{
		"type": "doc",
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "Hello"},
					map[string]any{"type": "text", "text": "world"},
				},
			},
		},
	}
	got := ADFToPlainText(doc)
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "world") {
		t.Errorf("want 'Hello' and 'world' in result, got %q", got)
	}
}
