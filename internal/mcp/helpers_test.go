package mcp

import (
	"encoding/json"
	"testing"
)

// --- extractPRURL tests ---

func TestExtractPRURL_Valid(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		owner   string
		repo    string
		prNum   int
	}{
		{
			name:  "standard github URL",
			text:  "https://github.com/owner/myrepo/pull/123",
			owner: "owner", repo: "myrepo", prNum: 123,
		},
		{
			name:  "URL in prose",
			text:  "Please review https://github.com/acme/backend/pull/456 thanks",
			owner: "acme", repo: "backend", prNum: 456,
		},
		{
			name:  "github enterprise URL",
			text:  "https://github.enterprise.com/team/service/pull/789",
			owner: "team", repo: "service", prNum: 789,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, num := extractPRURL(tc.text)
			if owner != tc.owner {
				t.Errorf("owner: want %q, got %q", tc.owner, owner)
			}
			if repo != tc.repo {
				t.Errorf("repo: want %q, got %q", tc.repo, repo)
			}
			if num != tc.prNum {
				t.Errorf("prNum: want %d, got %d", tc.prNum, num)
			}
		})
	}
}

func TestExtractPRURL_NoMatch(t *testing.T) {
	texts := []string{
		"no URL here",
		"https://gitlab.com/foo/bar/merge_requests/1",
		"https://github.com/owner/repo/issues/42",
	}
	for _, text := range texts {
		owner, repo, num := extractPRURL(text)
		if owner != "" || repo != "" || num != 0 {
			t.Errorf("expected no match for %q, got owner=%q repo=%q num=%d",
				text, owner, repo, num)
		}
	}
}

// --- safeBranchRe tests ---

func TestSafeBranchRe_Valid(t *testing.T) {
	validBranches := []string{
		"main",
		"feature/PROJ-1234",
		"fix/bug-123",
		"release-1.2.3",
		"dev_branch",
	}
	for _, b := range validBranches {
		if !safeBranchRe.MatchString(b) {
			t.Errorf("expected %q to be valid branch name", b)
		}
	}
}

func TestSafeBranchRe_Invalid(t *testing.T) {
	invalidBranches := []string{
		"",
		"branch with spaces",
		"branch;rm -rf /",
		"branch$(evil)",
		"branch`evil`",
		"branch&&evil",
		"branch|evil",
	}
	for _, b := range invalidBranches {
		if safeBranchRe.MatchString(b) {
			t.Errorf("expected %q to be rejected as branch name", b)
		}
	}
}

// --- extractPageID tests ---

func TestExtractPageID_NumericString(t *testing.T) {
	id := extractPageID("12345")
	if id != "12345" {
		t.Errorf("expected '12345', got %q", id)
	}
}

func TestExtractPageID_ConfluenceURL(t *testing.T) {
	url := "https://confluence.example.com/wiki/spaces/PROJ/pages/98765432/My+Page+Title"
	id := extractPageID(url)
	if id != "98765432" {
		t.Errorf("expected '98765432', got %q", id)
	}
}

func TestExtractPageID_URLWithTrailingSlash(t *testing.T) {
	url := "https://wiki.example.com/pages/11111/"
	id := extractPageID(url)
	// The function strips trailing content after /pages/ID/, returning "11111".
	if id == "" {
		t.Error("expected non-empty page ID")
	}
}

func TestExtractPageID_EmptyInput(t *testing.T) {
	// Empty string should return empty string (falls through to "already numeric" branch).
	id := extractPageID("")
	if id != "" {
		t.Errorf("expected empty, got %q", id)
	}
}

// --- extractOutputString tests ---

func TestExtractOutputString_JSONString(t *testing.T) {
	raw := json.RawMessage(`"hello world"`)
	got := extractOutputString(raw)
	if got != "hello world" {
		t.Errorf("expected 'hello world', got %q", got)
	}
}

func TestExtractOutputString_JSONObject(t *testing.T) {
	raw := json.RawMessage(`{"key":"value"}`)
	got := extractOutputString(raw)
	// Falls back to raw bytes when not a JSON string.
	if got != `{"key":"value"}` {
		t.Errorf("expected raw JSON, got %q", got)
	}
}

func TestExtractOutputString_Empty(t *testing.T) {
	got := extractOutputString(nil)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractOutputString_EmptyJSONString(t *testing.T) {
	raw := json.RawMessage(`""`)
	got := extractOutputString(raw)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// --- extractRepo tests ---

func TestExtractRepo_FromLabel(t *testing.T) {
	labels := []string{"backend", "repo:myapp", "high-priority"}
	repo := extractRepo(labels, "some summary")
	if repo != "myapp" {
		t.Errorf("expected 'myapp', got %q", repo)
	}
}

func TestExtractRepo_FromSummaryPrefix(t *testing.T) {
	labels := []string{"backend"}
	repo := extractRepo(labels, "myapp: Add new endpoint for users")
	if repo != "myapp" {
		t.Errorf("expected 'myapp', got %q", repo)
	}
}

func TestExtractRepo_NoMatch(t *testing.T) {
	labels := []string{"backend", "high-priority"}
	repo := extractRepo(labels, "Add new feature")
	if repo != "" {
		t.Errorf("expected empty, got %q", repo)
	}
}

func TestExtractRepo_LabelTakesPrecedence(t *testing.T) {
	labels := []string{"repo:booking"}
	repo := extractRepo(labels, "myapp: something unrelated")
	if repo != "booking" {
		t.Errorf("expected 'booking', got %q", repo)
	}
}

// --- unique helper tests ---

func TestUnique_Dedup(t *testing.T) {
	input := []string{"a", "b", "a", "c", "b"}
	result := unique(input)
	if len(result) != 3 {
		t.Errorf("expected 3 unique elements, got %d: %v", len(result), result)
	}
	seen := make(map[string]bool)
	for _, s := range result {
		if seen[s] {
			t.Errorf("duplicate element: %q", s)
		}
		seen[s] = true
	}
}

func TestUnique_Empty(t *testing.T) {
	result := unique(nil)
	if len(result) != 0 {
		t.Errorf("expected empty, got %v", result)
	}
}

func TestUnique_NoDups(t *testing.T) {
	input := []string{"x", "y", "z"}
	result := unique(input)
	if len(result) != 3 {
		t.Errorf("expected 3, got %d", len(result))
	}
}
