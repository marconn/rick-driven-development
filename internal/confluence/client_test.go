package confluence

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewClientFromEnvMissing(t *testing.T) {
	t.Setenv("CONFLUENCE_URL", "")
	t.Setenv("CONFLUENCE_EMAIL", "")
	t.Setenv("CONFLUENCE_TOKEN", "")
	t.Setenv("JIRA_EMAIL", "")
	t.Setenv("JIRA_TOKEN", "")

	if cli := NewClientFromEnv(); cli != nil {
		t.Error("want nil when env vars missing")
	}
}

func TestNewClientFromEnvPresent(t *testing.T) {
	t.Setenv("CONFLUENCE_URL", "https://example.atlassian.net/wiki")
	t.Setenv("CONFLUENCE_EMAIL", "user@test.com")
	t.Setenv("CONFLUENCE_TOKEN", "tok")

	cli := NewClientFromEnv()
	if cli == nil {
		t.Fatal("want non-nil client")
	}
}

func TestNewClientFromEnvFallbackToJiraCreds(t *testing.T) {
	t.Setenv("CONFLUENCE_URL", "https://example.atlassian.net/wiki")
	t.Setenv("CONFLUENCE_EMAIL", "")
	t.Setenv("CONFLUENCE_TOKEN", "")
	t.Setenv("JIRA_EMAIL", "jira@test.com")
	t.Setenv("JIRA_TOKEN", "jiratok")

	cli := NewClientFromEnv()
	if cli == nil {
		t.Fatal("want non-nil client (Jira fallback)")
	}
	if cli.email != "jira@test.com" {
		t.Errorf("want jira email fallback, got %q", cli.email)
	}
}

func TestReadPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/rest/api/content/12345") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "12345",
			"title": "Test Page",
			"body": {"storage": {"value": "<p>Hello</p>"}},
			"version": {"number": 3},
			"space": {"key": "DEV"}
		}`))
	}))
	defer srv.Close()

	cli := NewClient(srv.URL, "a@b.com", "tok")
	page, err := cli.ReadPage(context.Background(), "12345")
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if page.Title != "Test Page" {
		t.Errorf("want title 'Test Page', got %q", page.Title)
	}
	if page.Version != 3 {
		t.Errorf("want version 3, got %d", page.Version)
	}
	if page.SpaceKey != "DEV" {
		t.Errorf("want space key 'DEV', got %q", page.SpaceKey)
	}
}

func TestSplitAtHeading(t *testing.T) {
	html := `<p>Intro</p><h2>Plan Técnico</h2><p>Old plan content</p><h2>Next Section</h2><p>Other</p>`

	before, section, after, found := SplitAtHeading(html, "plan técnico")
	if !found {
		t.Fatal("heading not found")
	}
	if !strings.Contains(before, "Intro") {
		t.Errorf("before should contain Intro, got %q", before)
	}
	if !strings.Contains(section, "Old plan content") {
		t.Errorf("section should contain old content, got %q", section)
	}
	if !strings.Contains(after, "Next Section") {
		t.Errorf("after should contain next section, got %q", after)
	}
}

func TestSplitAtHeadingWithEntities(t *testing.T) {
	html := `<h2>Plan T&eacute;cnico de Implementaci&oacute;n</h2><p>Content</p>`

	_, _, _, found := SplitAtHeading(html, "plan técnico de implementación")
	if !found {
		t.Error("heading with HTML entities should be found")
	}
}

func TestExtractTextContent(t *testing.T) {
	html := `<p>Hello <strong>world</strong></p>`
	text := ExtractTextContent(html)
	if text != "Hello world" {
		t.Errorf("want 'Hello world', got %q", text)
	}
}

func TestNormalizeEntities(t *testing.T) {
	input := "caf&eacute; con le&ntilde;a"
	want := "café con leña"
	if got := NormalizeEntities(input); got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}
