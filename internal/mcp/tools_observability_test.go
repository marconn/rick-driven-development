package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// gitInit prepares a fresh git repo with initial state and a synthetic
// origin/main ref so we can drive `git diff origin/main...HEAD` against a
// known base. Returns the repo path.
func gitInit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "commit.gpgsign", "false")
	run("config", "user.email", "test@test")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base.txt: %v", err)
	}
	run("add", "base.txt")
	run("commit", "-q", "-m", "base")
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	return dir
}

// TestToolDiff_IncludesUncommittedChanges is the regression test for the
// 2026-04-29 bug: rick_diff returned an empty diff while the workspace had
// 4 staged files / 356 insertions. The pre-fix code ran only
// `git diff origin/main...HEAD` (3-dot symmetric diff), which excludes
// everything in the working tree. Post-fix the response carries both a
// "Committed since origin/<base>" section and an "Uncommitted (staged +
// unstaged)" section.
func TestToolDiff_IncludesUncommittedChanges(t *testing.T) {
	dir := gitInit(t)

	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// One commit on top of base — populates the "Committed since base" section.
	if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("committed line\n"), 0o644); err != nil {
		t.Fatalf("write committed.txt: %v", err)
	}
	runGit("add", "committed.txt")
	runGit("commit", "-q", "-m", "committed")

	// Staged-only file — should appear in the uncommitted section.
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged line\n"), 0o644); err != nil {
		t.Fatalf("write staged.txt: %v", err)
	}
	runGit("add", "staged.txt")

	// Unstaged modification of a tracked file — also uncommitted section.
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\nmodified line\n"), 0o644); err != nil {
		t.Fatalf("modify base.txt: %v", err)
	}

	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	defer s.Close()

	correlationID := "wf-difftest"
	wsReady := event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
		Path:   dir,
		Branch: "feature",
		Base:   "main",
	})).WithCorrelation(correlationID).WithAggregate("agg-difftest", 1)
	if err := deps.Store.Append(context.Background(), "agg-difftest", 0, []event.Envelope{wsReady}); err != nil {
		t.Fatalf("append ws-ready: %v", err)
	}

	res, err := callTool(t, s, "rick_diff", map[string]any{
		"workflow_id": correlationID,
	})
	if err != nil {
		t.Fatalf("rick_diff: %v", err)
	}
	resMap, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type: %T", res)
	}
	diff, _ := resMap["diff"].(string)

	wantSubstr := []struct {
		desc string
		s    string
	}{
		{"committed section header", "=== Committed since origin/main ==="},
		{"uncommitted section header", "=== Uncommitted (staged + unstaged) ==="},
		{"committed file appears in committed section", "committed.txt"},
		{"staged file appears in uncommitted section", "staged.txt"},
		{"unstaged modification appears in uncommitted section", "modified line"},
	}
	for _, w := range wantSubstr {
		if !strings.Contains(diff, w.s) {
			t.Errorf("diff missing %s (%q)\n--- diff ---\n%s\n--- end ---", w.desc, w.s, diff)
		}
	}
}

// TestToolDiff_NoBaseBranch covers the edge case where WorkspaceReady has
// no Base set: the committed section is omitted entirely, but the
// uncommitted section is still emitted (so a working-tree-only workspace
// is still visible).
func TestToolDiff_NoBaseBranch(t *testing.T) {
	dir := gitInit(t)

	cmd := exec.Command("git", "-C", dir, "add", "base.txt")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")

	if err := os.WriteFile(filepath.Join(dir, "fresh.txt"), []byte("fresh line\n"), 0o644); err != nil {
		t.Fatalf("write fresh.txt: %v", err)
	}
	addCmd := exec.Command("git", "-C", dir, "add", "fresh.txt")
	addCmd.Env = cmd.Env
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	defer s.Close()

	correlationID := "wf-difftest-nobase"
	wsReady := event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
		Path:   dir,
		Branch: "feature",
		Base:   "",
	})).WithCorrelation(correlationID).WithAggregate("agg-difftest-nobase", 1)
	if err := deps.Store.Append(context.Background(), "agg-difftest-nobase", 0, []event.Envelope{wsReady}); err != nil {
		t.Fatalf("append ws-ready: %v", err)
	}

	res, err := callTool(t, s, "rick_diff", map[string]any{
		"workflow_id": correlationID,
	})
	if err != nil {
		t.Fatalf("rick_diff: %v", err)
	}
	resMap, _ := res.(map[string]any)
	diff, _ := resMap["diff"].(string)

	if strings.Contains(diff, "=== Committed since") {
		t.Errorf("committed section should be omitted when base is empty\n--- diff ---\n%s\n--- end ---", diff)
	}
	if !strings.Contains(diff, "=== Uncommitted (staged + unstaged) ===") {
		t.Errorf("uncommitted section header missing\n--- diff ---\n%s\n--- end ---", diff)
	}
	if !strings.Contains(diff, "fresh.txt") {
		t.Errorf("staged file not in diff\n--- diff ---\n%s\n--- end ---", diff)
	}
}

// TestToolDiff_StatOnly verifies the --stat flag flows through both diff
// invocations rather than being applied to only one section.
func TestToolDiff_StatOnly(t *testing.T) {
	dir := gitInit(t)

	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("committed line\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit("add", "committed.txt")
	runGit("commit", "-q", "-m", "committed")
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged line\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit("add", "staged.txt")

	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	defer s.Close()

	correlationID := "wf-difftest-stat"
	wsReady := event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
		Path: dir, Branch: "feature", Base: "main",
	})).WithCorrelation(correlationID).WithAggregate("agg-difftest-stat", 1)
	if err := deps.Store.Append(context.Background(), "agg-difftest-stat", 0, []event.Envelope{wsReady}); err != nil {
		t.Fatalf("append: %v", err)
	}

	raw, _ := json.Marshal(map[string]any{
		"workflow_id": correlationID,
		"stat_only":   true,
	})
	res, err := s.tools["rick_diff"].Handler(context.Background(), raw)
	if err != nil {
		t.Fatalf("rick_diff: %v", err)
	}
	resMap, _ := res.(map[string]any)
	diff, _ := resMap["diff"].(string)

	// --stat output contains "<path> | <count> ..." pattern; full diff would
	// contain "+++" / "---" markers we should NOT see in stat mode.
	if strings.Contains(diff, "+++") || strings.Contains(diff, "---") {
		t.Errorf("stat_only=true should not include full diff hunks\n--- diff ---\n%s\n--- end ---", diff)
	}
	if !strings.Contains(diff, "committed.txt") || !strings.Contains(diff, "staged.txt") {
		t.Errorf("stat_only output missing expected files\n--- diff ---\n%s\n--- end ---", diff)
	}
}
