package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/jira"
)

// --- Test helpers ---

// testServer builds a Server with standard test deps.
func testServer(t *testing.T) (*Server, func()) {
	t.Helper()
	deps, cleanup := testDeps(t)
	s := NewServer(deps, testLogger())
	return s, func() {
		s.Close()
		cleanup()
	}
}

// callTool calls a named tool directly and returns the result.
func callTool(t *testing.T, s *Server, name string, args any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	tool, ok := s.tools[name]
	if !ok {
		t.Fatalf("tool not found: %s", name)
	}
	return tool.Handler(context.Background(), raw)
}

// ============================================================
// A. safeWorkspacePath — SECURITY CRITICAL
// ============================================================

// TestSafeWorkspacePath tests the path validation guard for workspace cleanup.
// These tests verify that the function prevents path traversal and enforces
// the *-rick-ws-* naming pattern so arbitrary directories cannot be deleted.
func TestSafeWorkspacePath(t *testing.T) {
	// Create a real temp directory to act as RICK_REPOS_PATH so EvalSymlinks succeeds.
	reposPath := t.TempDir()

	// Create a valid workspace directory under reposPath.
	validWS := filepath.Join(reposPath, "myapp-rick-ws-12345")
	if err := os.MkdirAll(validWS, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Create a directory outside reposPath to test traversal prevention.
	outsideDir := t.TempDir()
	outsideWS := filepath.Join(outsideDir, "some-rick-ws-99999")
	if err := os.MkdirAll(outsideWS, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	t.Setenv("RICK_REPOS_PATH", reposPath)

	tests := []struct {
		name    string
		path    string
		wantErr string // non-empty = expected to fail with this substring
	}{
		{
			name:    "valid rick-ws workspace under RICK_REPOS_PATH",
			path:    validWS,
			wantErr: "",
		},
		{
			name:    "path outside RICK_REPOS_PATH",
			path:    outsideWS,
			wantErr: "outside $RICK_REPOS_PATH",
		},
		{
			name:    "path not matching rick-ws pattern",
			path:    filepath.Join(reposPath, "myapp-no-match"),
			wantErr: "not matching *-rick-ws-*",
		},
		{
			name:    "traversal attempt via relative segments",
			path:    filepath.Join(reposPath, "..", "some-rick-ws-other"),
			wantErr: "outside $RICK_REPOS_PATH",
		},
		{
			name:    "RICK_REPOS_PATH itself (no pattern match)",
			path:    reposPath,
			wantErr: "not matching *-rick-ws-*",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safeWorkspacePath(tc.path)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (result=%q)", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("expected error %q, got: %v", tc.wantErr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == "" {
				t.Error("expected non-empty resolved path")
			}
		})
	}
}

func TestSafeWorkspacePath_NoRICK_REPOS_PATH(t *testing.T) {
	t.Setenv("RICK_REPOS_PATH", "")
	_, err := safeWorkspacePath("/some/path")
	if err == nil {
		t.Fatal("expected error when RICK_REPOS_PATH is not set")
	}
	if !strings.Contains(err.Error(), "RICK_REPOS_PATH") {
		t.Errorf("expected RICK_REPOS_PATH error, got: %v", err)
	}
}

// ============================================================
// B. safeBranchRe — branch name injection prevention
// ============================================================

// TestSafeBranchRe verifies the regex blocks shell metacharacters in branch names.
// This prevents git flag injection via crafted branch names passed to exec.Command.
func TestSafeBranchRe(t *testing.T) {
	tests := []struct {
		name      string
		branch    string
		wantMatch bool
	}{
		// Valid branch names.
		{"feature branch with slash", "feature/PROJ-123", true},
		{"fix with hyphen", "fix-bar", true},
		{"jira ticket format", "PROJ-123", true},
		{"simple main", "main", true},
		{"dotted version", "release/1.2.3", true},
		{"underscore", "my_feature", true},

		// Invalid — shell metacharacters.
		{"semicolon injection", "feature;rm -rf /", false},
		{"pipe injection", "feature|cat /etc/passwd", false},
		{"ampersand injection", "feature&cmd", false},
		{"dollar injection", "$PATH", false},
		{"backtick injection", "`id`", false},
		{"space in name", "feature branch", false},
		{"empty string", "", false},
		{"starts with dot", ".hidden", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := safeBranchRe.MatchString(tc.branch)
			if got != tc.wantMatch {
				t.Errorf("safeBranchRe.MatchString(%q) = %v, want %v", tc.branch, got, tc.wantMatch)
			}
		})
	}
}

// ============================================================
// C. extractPageID
// ============================================================

// TestExtractPageID verifies numeric IDs and URL extraction.
func TestExtractPageID(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain numeric ID passthrough",
			input: "12345",
			want:  "12345",
		},
		{
			name:  "full confluence URL with /pages/",
			input: "https://wiki.example.com/wiki/spaces/ENG/pages/987654321/My+Page",
			want:  "987654321",
		},
		{
			name:  "short confluence URL",
			input: "https://example.atlassian.net/wiki/pages/42/Title",
			want:  "42",
		},
		{
			name:  "URL ending at ID (no trailing slash or title)",
			input: "https://example.com/pages/55555",
			want:  "55555",
		},
		{
			name:  "alphanumeric ID passthrough",
			input: "ABC-123",
			want:  "ABC-123",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractPageID(tc.input)
			if got != tc.want {
				t.Errorf("extractPageID(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ============================================================
// D. extractPRURL
// ============================================================

// TestExtractPRURL verifies GitHub PR URL parsing from arbitrary text.
func TestExtractPRURL(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantOwner string
		wantRepo  string
		wantPR    int
	}{
		{
			name:      "plain PR URL",
			text:      "https://github.com/acme/myrepo/pull/42",
			wantOwner: "acme",
			wantRepo:  "myrepo",
			wantPR:    42,
		},
		{
			name:      "PR URL embedded in prose",
			text:      "Please review https://github.com/org/repo/pull/123 by Friday",
			wantOwner: "org",
			wantRepo:  "repo",
			wantPR:    123,
		},
		{
			name:      "enterprise GitHub URL",
			text:      "https://github.example.com/team/project/pull/7",
			wantOwner: "team",
			wantRepo:  "project",
			wantPR:    7,
		},
		{
			name:      "no URL in text",
			text:      "implement feature X",
			wantOwner: "",
			wantRepo:  "",
			wantPR:    0,
		},
		{
			name:      "non-PR GitHub URL",
			text:      "https://github.com/acme/myrepo/issues/42",
			wantOwner: "",
			wantRepo:  "",
			wantPR:    0,
		},
		{
			name:      "empty text",
			text:      "",
			wantOwner: "",
			wantRepo:  "",
			wantPR:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, pr := extractPRURL(tc.text)
			if owner != tc.wantOwner || repo != tc.wantRepo || pr != tc.wantPR {
				t.Errorf("extractPRURL(%q) = (%q, %q, %d), want (%q, %q, %d)",
					tc.text, owner, repo, pr, tc.wantOwner, tc.wantRepo, tc.wantPR)
			}
		})
	}
}

// ============================================================
// E. extractRepo + unique helpers
// ============================================================

// TestExtractRepo verifies repo extraction from Jira issue labels and summary.
func TestExtractRepo(t *testing.T) {
	tests := []struct {
		name    string
		labels  []string
		summary string
		want    string
	}{
		{
			name:    "repo label present",
			labels:  []string{"team:backend", "repo:myapp"},
			summary: "Add endpoint",
			want:    "myapp",
		},
		{
			name:    "repo extracted from summary prefix",
			labels:  []string{},
			summary: "booking: implement checkout flow",
			want:    "booking",
		},
		{
			name:    "label takes precedence over summary",
			labels:  []string{"repo:myrepo"},
			summary: "other: task description",
			want:    "myrepo",
		},
		{
			name:    "no repo signal",
			labels:  []string{"priority:high"},
			summary: "Generic task with no repo prefix",
			want:    "",
		},
		{
			name:    "summary prefix too long (>30 chars not treated as repo)",
			labels:  []string{},
			summary: "thisIsAVeryLongPrefixThatExceedsThirtyChars: task",
			want:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractRepo(tc.labels, tc.summary)
			if got != tc.want {
				t.Errorf("extractRepo(%v, %q) = %q, want %q", tc.labels, tc.summary, got, tc.want)
			}
		})
	}
}

// TestUnique verifies deduplication of string slices.
func TestUnique(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{
			name:  "empty slice",
			input: []string{},
			want:  []string{},
		},
		{
			name:  "no duplicates",
			input: []string{"a", "b", "c"},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "duplicates removed",
			input: []string{"a", "b", "a", "c", "b"},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "all duplicates",
			input: []string{"x", "x", "x"},
			want:  []string{"x"},
		},
		{
			name:  "nil input",
			input: nil,
			want:  []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := unique(tc.input)
			if len(got) != len(tc.want) {
				t.Errorf("unique(%v) = %v (len %d), want %v (len %d)",
					tc.input, got, len(got), tc.want, len(tc.want))
				return
			}
			for i, g := range got {
				if g != tc.want[i] {
					t.Errorf("unique(%v)[%d] = %q, want %q", tc.input, i, g, tc.want[i])
				}
			}
		})
	}
}

// ============================================================
// F. computeWavePlan via mock Jira server
// ============================================================

// newMockWaveJira builds a mock Jira HTTP server for wave plan tests.
// children is a list of (key, summary) with optional block deps provided via
// the deps map: key → []blockerKeys.
func newMockWaveJira(t *testing.T, children []jira.EpicChildIssue, deps map[string][]string) (*jira.Client, func()) {
	t.Helper()

	mux := http.NewServeMux()

	// /rest/api/3/search/jql — returns epic children
	mux.HandleFunc("/rest/api/3/search/jql", func(w http.ResponseWriter, r *http.Request) {
		issues := make([]map[string]any, 0, len(children))
		for _, c := range children {
			issues = append(issues, map[string]any{
				"key": c.Key,
				"fields": map[string]any{
					"summary":           c.Summary,
					"status":            map[string]any{"name": c.Status},
					"labels":            c.Labels,
					"customfield_10004": c.Points,
					"issuetype":         map[string]any{"name": "Task"},
				},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total":  len(issues),
			"issues": issues,
		})
	})

	// /rest/api/3/issue/{key} — returns links for each issue
	mux.HandleFunc("/rest/api/3/issue/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
		var links []map[string]any
		for _, blocker := range deps[key] {
			links = append(links, map[string]any{
				"type":        map[string]any{"name": "Blocks"},
				"inwardIssue": map[string]any{"key": blocker},
			})
		}
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

// TestComputeWavePlan_LinearChain_Sequential verifies that a strict A→B→C dependency chain
// produces exactly 3 sequential waves.
func TestComputeWavePlan_LinearChain_Sequential(t *testing.T) {
	children := []jira.EpicChildIssue{
		{Key: "T-1", Summary: "A", Status: "To Do"},
		{Key: "T-2", Summary: "B", Status: "To Do"},
		{Key: "T-3", Summary: "C", Status: "To Do"},
	}
	// T-2 blocked by T-1, T-3 blocked by T-2.
	blockDeps := map[string][]string{
		"T-2": {"T-1"},
		"T-3": {"T-2"},
	}

	client, stop := newMockWaveJira(t, children, blockDeps)
	defer stop()

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
		t.Errorf("expected 3 waves for A→B→C chain, got %d: %+v", len(result.Waves), result.Waves)
	}
	// Each wave should have exactly one ticket.
	for i, w := range result.Waves {
		if len(w.Tickets) != 1 {
			t.Errorf("wave %d: expected 1 ticket, got %d", i+1, len(w.Tickets))
		}
	}
}

// TestComputeWavePlan_Parallel verifies that independent tasks land in the same wave.
func TestComputeWavePlan_Parallel(t *testing.T) {
	children := []jira.EpicChildIssue{
		{Key: "P-1", Summary: "Task A", Status: "To Do", Points: 3},
		{Key: "P-2", Summary: "Task B", Status: "To Do", Points: 5},
		{Key: "P-3", Summary: "Task C", Status: "To Do", Points: 2},
	}
	// No dependencies — all independent.
	client, stop := newMockWaveJira(t, children, nil)
	defer stop()

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := s.computeWavePlan(context.Background(), "EPIC-PAR")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Waves) != 1 {
		t.Errorf("expected 1 wave for independent tasks, got %d", len(result.Waves))
	}
	if result.Waves[0].Wave != 1 {
		t.Errorf("expected wave number 1, got %d", result.Waves[0].Wave)
	}
	if len(result.Waves[0].Tickets) != 3 {
		t.Errorf("expected 3 tickets in single wave, got %d", len(result.Waves[0].Tickets))
	}
	if result.Parallelism != 3 {
		t.Errorf("expected parallelism=3, got %d", result.Parallelism)
	}
	// Total points should be 3+5+2=10.
	if result.TotalPoints != 10.0 {
		t.Errorf("expected total_points=10, got %.1f", result.TotalPoints)
	}
}

// TestComputeWavePlan_CircularDependency_Direct verifies that circular deps don't cause
// infinite loops — all tickets get assigned via the fallback path.
func TestComputeWavePlan_CircularDependency_Direct(t *testing.T) {
	children := []jira.EpicChildIssue{
		{Key: "C-1", Summary: "A", Status: "To Do"},
		{Key: "C-2", Summary: "B", Status: "To Do"},
	}
	// C-1 blocked by C-2, C-2 blocked by C-1 → circular.
	blockDeps := map[string][]string{
		"C-1": {"C-2"},
		"C-2": {"C-1"},
	}

	client, stop := newMockWaveJira(t, children, blockDeps)
	defer stop()

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = client
	s := NewServer(deps, testLogger())
	defer s.Close()

	// Must not hang or panic.
	result, err := s.computeWavePlan(context.Background(), "EPIC-CIRC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// All tickets must be assigned regardless of cycle.
	total := 0
	for _, w := range result.Waves {
		total += len(w.Tickets)
	}
	if total != 2 {
		t.Errorf("expected 2 tickets assigned in total, got %d", total)
	}
}

// ============================================================
// Tool-level error path tests (no client configured)
// ============================================================

func TestJiraTools_NoClientConfigured(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	jiraTools := []struct {
		name string
		args any
	}{
		{"rick_jira_read", map[string]any{"ticket": "PROJ-1"}},
		{"rick_jira_write", map[string]any{"ticket": "PROJ-1", "field_name": "description", "value": "x"}},
		{"rick_jira_transition", map[string]any{"ticket": "PROJ-1", "status": "Done"}},
		{"rick_jira_comment", map[string]any{"ticket": "PROJ-1", "comment": "test"}},
		{"rick_jira_epic_issues", map[string]any{"epic": "PROJ-EPIC"}},
		{"rick_jira_search", map[string]any{"jql": "project = PROJ"}},
		{"rick_jira_link", map[string]any{"from_ticket": "PROJ-1", "to_ticket": "PROJ-2"}},
	}

	for _, tc := range jiraTools {
		t.Run(tc.name, func(t *testing.T) {
			_, err := callTool(t, s, tc.name, tc.args)
			if err == nil {
				t.Fatal("expected error when Jira client not configured")
			}
			if !strings.Contains(err.Error(), "Jira client not configured") {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestConfluenceTools_NoClientConfigured(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	confluenceTools := []struct {
		name string
		args any
	}{
		{"rick_confluence_read", map[string]any{"page_id": "12345"}},
		{"rick_confluence_write", map[string]any{
			"page_id":       "12345",
			"content":       "# Test",
			"after_heading": "Plan Tecnico",
		}},
	}

	for _, tc := range confluenceTools {
		t.Run(tc.name, func(t *testing.T) {
			_, err := callTool(t, s, tc.name, tc.args)
			if err == nil {
				t.Fatal("expected error when Confluence client not configured")
			}
			if !strings.Contains(err.Error(), "Confluence client not configured") {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestWaveTools_NoJiraClient(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	waveTools := []struct {
		name string
		args any
	}{
		{"rick_wave_plan", map[string]any{"epic": "PROJ-EPIC"}},
		{"rick_wave_launch", map[string]any{"epic": "PROJ-EPIC"}},
		{"rick_wave_status", map[string]any{"epic": "PROJ-EPIC"}},
	}

	for _, tc := range waveTools {
		t.Run(tc.name, func(t *testing.T) {
			_, err := callTool(t, s, tc.name, tc.args)
			if err == nil {
				t.Fatal("expected error when Jira client not configured")
			}
			if !strings.Contains(err.Error(), "Jira client not configured") {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestToolWorkspaceCleanup_MissingPath(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_workspace_cleanup", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolWavePlan_MissingEpic(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_wave_plan", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing epic")
	}
	if !strings.Contains(err.Error(), "epic is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolConfluenceRead_MissingPageID(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_confluence_read", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing page_id")
	}
	if !strings.Contains(err.Error(), "page_id is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Tool-level: job tools
func TestToolJobStatus_NotFound(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_job_status", map[string]any{"job_id": "nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent job")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found', got: %v", err)
	}
}

func TestToolJobStatus_MissingJobID(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_job_status", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing job_id")
	}
	if !strings.Contains(err.Error(), "job_id is required") {
		t.Errorf("expected 'job_id is required', got: %v", err)
	}
}

func TestToolJobCancel_NotFound(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_job_cancel", map[string]any{"job_id": "nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent job")
	}
}

func TestToolJobsList_Empty(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	result, err := callTool(t, s, "rick_jobs", map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	jlr, ok := result.(jobsListResult)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if jlr.Count != 0 {
		t.Errorf("expected 0 jobs, got %d", jlr.Count)
	}
}

// observability tools
func TestToolSearchWorkflows_RequiresTag(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_search_workflows", map[string]any{})
	if err == nil {
		t.Fatal("expected error when no search criteria provided")
	}
	if !strings.Contains(err.Error(), "at least one") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestToolWorkflowOutput_MissingID(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_workflow_output", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing workflow_id")
	}
}

func TestToolRetryWorkflow_MissingID(t *testing.T) {
	s, cleanup := testServer(t)
	defer cleanup()

	_, err := callTool(t, s, "rick_retry_workflow", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing workflow_id")
	}
}

// ============================================================
// G. toolWaveLaunch — repo field propagation regression test
// ============================================================

// TestToolWaveLaunch_RepoPassedToWorkflow verifies that when wave tickets have
// a "repo:owner/myrepo" label, toolWaveLaunch forwards the repo field to the
// launched WorkflowRequested event. This is a regression test for the bug
// where ticket.Repo was not passed to toolRunWorkflow, causing jira-dev
// workflows to lose the repo context.
func TestToolWaveLaunch_RepoPassedToWorkflow(t *testing.T) {
	children := []jira.EpicChildIssue{
		{
			Key:     "WAVE-1",
			Summary: "Implement feature X",
			Status:  "To Do",
			Labels:  []string{"repo:owner/myrepo"},
		},
	}

	jiraClient, stopJira := newMockWaveJira(t, children, nil)
	defer stopJira()

	deps, cleanup := testDeps(t)
	defer cleanup()
	deps.Jira = jiraClient

	// Register jira-dev workflow so toolRunWorkflow can find it.
	deps.Engine.RegisterWorkflow(engine.JiraDevWorkflowDef())

	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := callTool(t, s, "rick_wave_launch", map[string]any{
		"epic": "WAVE-EPIC",
		"dag":  "jira-dev",
	})
	if err != nil {
		t.Fatalf("toolWaveLaunch: %v", err)
	}

	wlr, ok := result.(waveLaunchResult)
	if !ok {
		t.Fatalf("unexpected result type: %T", result)
	}
	if len(wlr.Launched) != 1 {
		t.Fatalf("expected 1 launched ticket, got %d (errors: %v)", len(wlr.Launched), wlr.Errors)
	}

	correlationID := wlr.Launched[0].CorrelationID
	if correlationID == "" {
		t.Fatal("expected non-empty correlation_id from launched workflow")
	}

	// Load the WorkflowRequested event from the store and verify the repo field.
	ctx := context.Background()
	events, err := deps.Store.Load(ctx, correlationID)
	if err != nil {
		t.Fatalf("load events for correlation %s: %v", correlationID, err)
	}
	if len(events) == 0 {
		t.Fatal("no events found for launched workflow")
	}

	var reqPayload struct {
		Repo string `json:"repo"`
	}
	if unmarshalErr := json.Unmarshal(events[0].Payload, &reqPayload); unmarshalErr != nil {
		t.Fatalf("unmarshal WorkflowRequested payload: %v", unmarshalErr)
	}
	if reqPayload.Repo != "owner/myrepo" {
		t.Errorf("WorkflowRequested.Repo = %q, want %q", reqPayload.Repo, "owner/myrepo")
	}
}
