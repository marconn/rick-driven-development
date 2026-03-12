package source_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/source"
)

func TestResolveRawText(t *testing.T) {
	r := source.NewResolver()
	s, err := r.Resolve(context.Background(), "write me a unit test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Type != source.Raw {
		t.Errorf("type: got %q, want %q", s.Type, source.Raw)
	}
	if s.Reference != "" {
		t.Errorf("reference: got %q, want empty", s.Reference)
	}
	if s.Content != "write me a unit test" {
		t.Errorf("content: got %q, want %q", s.Content, "write me a unit test")
	}
}

func TestResolveFileSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "context.md")
	if err := os.WriteFile(path, []byte("# context\nsome content"), 0600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	r := source.NewResolver()
	s, err := r.Resolve(context.Background(), "file:"+path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Type != source.File {
		t.Errorf("type: got %q, want %q", s.Type, source.File)
	}
	if s.Reference != path {
		t.Errorf("reference: got %q, want %q", s.Reference, path)
	}
	if s.Content != "# context\nsome content" {
		t.Errorf("content: got %q, want %q", s.Content, "# context\nsome content")
	}
}

func TestResolveFileNotFound(t *testing.T) {
	r := source.NewResolver()
	_, err := r.Resolve(context.Background(), "file:nonexistent.md")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestResolveJiraSource(t *testing.T) {
	r := source.NewResolver()
	s, err := r.Resolve(context.Background(), "jira:PROJ-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Type != source.Jira {
		t.Errorf("type: got %q, want %q", s.Type, source.Jira)
	}
	if s.Reference != "PROJ-123" {
		t.Errorf("reference: got %q, want %q", s.Reference, "PROJ-123")
	}
	if s.Content != "jira:PROJ-123" {
		t.Errorf("content: got %q, want %q", s.Content, "jira:PROJ-123")
	}
}

func TestResolveGitHubSource(t *testing.T) {
	// Requires gh CLI, network access, and a valid repo — skip in CI.
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh CLI not available")
	}

	r := source.NewResolver()
	// Use a well-known public issue: golang/go#1 (Rob Pike's first issue).
	s, err := r.Resolve(context.Background(), "gh:golang/go#1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Type != source.GitHub {
		t.Errorf("type: got %q, want %q", s.Type, source.GitHub)
	}
	if s.Reference != "golang/go#1" {
		t.Errorf("reference: got %q, want %q", s.Reference, "golang/go#1")
	}
	if s.Content == "" {
		t.Error("expected non-empty content from GitHub issue")
	}
}

func TestResolveGitHubSourceBadFormat(t *testing.T) {
	r := source.NewResolver()
	s, err := r.Resolve(context.Background(), "gh:no-number")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No # separator → falls through without fetching
	if s.Type != source.GitHub {
		t.Errorf("type: got %q, want %q", s.Type, source.GitHub)
	}
}

func TestResolveEmptyInput(t *testing.T) {
	r := source.NewResolver()
	s, err := r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Type != source.Raw {
		t.Errorf("type: got %q, want %q", s.Type, source.Raw)
	}
	if s.Content != "" {
		t.Errorf("content: got %q, want empty", s.Content)
	}
}

func TestResolveFilePrefix(t *testing.T) {
	r := source.NewResolver()
	_, err := r.Resolve(context.Background(), "file:")
	if err == nil {
		t.Fatal("expected error for bare file: prefix, got nil")
	}
}

// --- GitHub source parsing (no network required) ---

// TestResolveGitHubNoHash verifies that gh: without a # separator
// returns a Source without calling the gh CLI (safe, no network).
func TestResolveGitHubNoHash(t *testing.T) {
	r := source.NewResolver()
	s, err := r.Resolve(context.Background(), "gh:owner/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Type != source.GitHub {
		t.Errorf("type: got %q, want %q", s.Type, source.GitHub)
	}
	if s.Reference != "owner/repo" {
		t.Errorf("reference: got %q, want %q", s.Reference, "owner/repo")
	}
	// No # means the content falls through as the raw ref.
	if s.Content != "owner/repo" {
		t.Errorf("content: got %q, want %q", s.Content, "owner/repo")
	}
}

// TestResolveGitHubOnlyPrefix checks an edge: "gh:" with nothing after it.
func TestResolveGitHubOnlyPrefix(t *testing.T) {
	r := source.NewResolver()
	s, err := r.Resolve(context.Background(), "gh:")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty ref, no # — falls through to raw reference path.
	if s.Type != source.GitHub {
		t.Errorf("type: got %q, want %q", s.Type, source.GitHub)
	}
}

// TestResolveSourceTypeIdentity verifies that each recognised prefix
// resolves to the correct SourceType.
func TestResolveSourceTypeIdentity(t *testing.T) {
	r := source.NewResolver()
	ctx := context.Background()

	tests := []struct {
		input    string
		wantType source.SourceType
	}{
		{"plain text prompt", source.Raw},
		{"jira:PROJ-999", source.Jira},
		{"gh:owner/repo", source.GitHub}, // no # so no gh CLI call
	}

	for _, tc := range tests {
		s, err := r.Resolve(ctx, tc.input)
		if err != nil {
			t.Fatalf("Resolve(%q): unexpected error: %v", tc.input, err)
		}
		if s.Type != tc.wantType {
			t.Errorf("Resolve(%q).Type = %q, want %q", tc.input, s.Type, tc.wantType)
		}
	}
}

// TestResolveJiraKeyPreserved verifies the jira key is available via Reference.
func TestResolveJiraKeyPreserved(t *testing.T) {
	r := source.NewResolver()
	s, err := r.Resolve(context.Background(), "jira:ACME-42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Reference != "ACME-42" {
		t.Errorf("reference: got %q, want %q", s.Reference, "ACME-42")
	}
}

// TestResolveFileNonEmptyPath verifies file: with a valid path returns File type.
func TestResolveFileTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	r := source.NewResolver()
	s, err := r.Resolve(context.Background(), "file:"+path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Type != source.File {
		t.Errorf("type: got %q, want %q", s.Type, source.File)
	}
	if s.Content != "hello world" {
		t.Errorf("content: got %q, want %q", s.Content, "hello world")
	}
}
