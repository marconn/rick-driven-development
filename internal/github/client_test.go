package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetIssue_ParsesFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/acme/widgets/issues/42") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("auth header=%q, want Bearer tok", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"number": 42,
			"title": "Login broken",
			"state": "open",
			"html_url": "https://github.com/acme/widgets/issues/42",
			"body": "Repro: click login",
			"user": {"login": "alice"},
			"labels": [{"name":"bug"},{"name":"frontend"}]
		}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClientWithBase(srv.URL, "tok")
	issue, err := c.GetIssue(context.Background(), "acme", "widgets", 42)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}

	if issue.Number != 42 || issue.Title != "Login broken" || issue.State != "open" {
		t.Errorf("unexpected fields: %+v", issue)
	}
	if issue.User.Login != "alice" {
		t.Errorf("user.login=%q, want alice", issue.User.Login)
	}
	if len(issue.Labels) != 2 || issue.Labels[0].Name != "bug" || issue.Labels[1].Name != "frontend" {
		t.Errorf("labels=%+v", issue.Labels)
	}
	if issue.PullRequest != nil {
		t.Error("PullRequest should be nil for a regular issue")
	}
}

func TestGetIssue_DetectsPullRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"number": 7,
			"title": "feat: add login",
			"pull_request": {"url":"https://api.github.com/..."}
		}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClientWithBase(srv.URL, "tok")
	issue, err := c.GetIssue(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue.PullRequest == nil {
		t.Error("PullRequest should be set when payload has pull_request key")
	}
}

func TestGetIssue_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := NewClientWithBase(srv.URL, "tok")
	_, err := c.GetIssue(context.Background(), "acme", "widgets", 404)
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "get issue acme/widgets#404") {
		t.Errorf("error should mention the issue ref, got: %v", err)
	}
}
