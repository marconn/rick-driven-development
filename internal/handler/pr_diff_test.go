package handler

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newGitRepoWithBaseAndHead builds a tiny git repo under t.TempDir() with a
// base commit on `main` and a head commit on `feature` that adds one file.
// An `origin/main` ref points at the base commit, so `origin/main...HEAD`
// produces the feature-branch diff — matching how pr-workspace's clone looks
// after `git fetch origin` + checkout of the PR branch.
func newGitRepoWithBaseAndHead(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = minimalGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init", "-b", "main")
	mustWriteFile(t, filepath.Join(dir, "seed.txt"), "hello\n")
	run("add", "seed.txt")
	run("commit", "-m", "base")
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	run("checkout", "-b", "feature")
	mustWriteFile(t, filepath.Join(dir, "added.txt"), "new content\nline two\nline three\n")
	run("add", "added.txt")
	run("commit", "-m", "add file")

	return dir
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestGenerateWorkspaceDiff_Success(t *testing.T) {
	dir := newGitRepoWithBaseAndHead(t)

	diff, files, ok := generateWorkspaceDiff(context.Background(), dir, "main")
	if !ok {
		t.Fatalf("generateWorkspaceDiff ok=false on a valid repo")
	}
	if len(files) != 1 || files[0].Path != "added.txt" {
		t.Errorf("files: want [added.txt], got %+v", files)
	}
	if files[0].Added != 3 || files[0].Removed != 0 {
		t.Errorf("numstat: want +3/-0 for added.txt, got +%d/-%d", files[0].Added, files[0].Removed)
	}
	if !strings.Contains(diff, "+new content") {
		t.Errorf("diff missing added content: %q", diff)
	}
	if !strings.Contains(diff, "+++ b/added.txt") {
		t.Errorf("diff missing unified header for added.txt: %q", diff)
	}
}

func TestGenerateWorkspaceDiff_EmptyInputs(t *testing.T) {
	if _, _, ok := generateWorkspaceDiff(context.Background(), "", "main"); ok {
		t.Error("empty workspacePath should yield ok=false")
	}
	if _, _, ok := generateWorkspaceDiff(context.Background(), "/tmp", ""); ok {
		t.Error("empty base should yield ok=false")
	}
}

func TestGenerateWorkspaceDiff_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	if _, _, ok := generateWorkspaceDiff(context.Background(), dir, "main"); ok {
		t.Error("non-repo dir should yield ok=false")
	}
}

func TestBuildWorkspaceDiffEnrichment_UntruncatedIsPrDiff(t *testing.T) {
	dir := newGitRepoWithBaseAndHead(t)

	enrichment := buildWorkspaceDiffEnrichment(context.Background(), dir, "main")
	if enrichment.Kind != "pr-diff" {
		t.Errorf("kind: want 'pr-diff' for small diff, got %q", enrichment.Kind)
	}
	if !strings.Contains(enrichment.Summary, "## PR Changed Files") {
		t.Error("summary missing file manifest header")
	}
	if !strings.Contains(enrichment.Summary, "(+3 / -0)") {
		t.Errorf("summary missing per-file LOC counts: %q", enrichment.Summary)
	}
	if strings.Contains(enrichment.Summary, "TRUNCATED") {
		t.Error("small diff should not include TRUNCATED marker")
	}
}

func TestBuildWorkspaceDiffEnrichment_TruncatedFlipsKind(t *testing.T) {
	dir := newGitRepoWithBaseAndHead(t)

	// Append a large file on HEAD so the diff body exceeds prDiffMaxBytes.
	big := strings.Repeat("x"+strings.Repeat("x", 78)+"\n", prDiffMaxBytes/80+16)
	mustWriteFile(t, filepath.Join(dir, "big.txt"), big)
	cmd := exec.Command("git", "-C", dir, "add", "big.txt")
	cmd.Env = minimalGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", dir, "commit", "-m", "big")
	cmd.Env = minimalGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	enrichment := buildWorkspaceDiffEnrichment(context.Background(), dir, "main")
	if enrichment.Kind != "pr-diff-truncated" {
		t.Errorf("kind: want 'pr-diff-truncated' for oversized diff, got %q (summary len=%d)",
			enrichment.Kind, len(enrichment.Summary))
	}
	if !strings.Contains(enrichment.Summary, "TRUNCATED") {
		t.Error("truncated diff should include TRUNCATED marker in summary")
	}
	// File manifest must still be complete — that's the whole point of the
	// fallback UX: reviewers know what they missed.
	if !strings.Contains(enrichment.Summary, "big.txt") {
		t.Error("truncated-diff manifest must still list every changed file")
	}
	if !strings.Contains(enrichment.Summary, "added.txt") {
		t.Error("truncated-diff manifest must still list the smaller files")
	}
}

func TestFetchWorkspaceRawDiff_ReturnsFullDiff(t *testing.T) {
	dir := newGitRepoWithBaseAndHead(t)

	diff := fetchWorkspaceRawDiff(context.Background(), dir, "main")
	if diff == "" {
		t.Fatal("fetchWorkspaceRawDiff returned empty for a valid repo")
	}
	if !strings.Contains(diff, "+new content") {
		t.Errorf("raw diff missing added content: %q", diff)
	}
}

func TestFetchWorkspaceRawDiff_EmptyInputs(t *testing.T) {
	if diff := fetchWorkspaceRawDiff(context.Background(), "", "main"); diff != "" {
		t.Errorf("empty workspacePath: want '', got %q", diff)
	}
	if diff := fetchWorkspaceRawDiff(context.Background(), "/tmp", ""); diff != "" {
		t.Errorf("empty base: want '', got %q", diff)
	}
}

func minimalGitEnv() []string {
	return []string{
		"PATH=" + pathFromEnv(),
		"HOME=/tmp",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	}
}

func pathFromEnv() string {
	if out, err := exec.Command("sh", "-c", "echo -n $PATH").Output(); err == nil {
		return string(out)
	}
	return "/usr/bin:/bin"
}
