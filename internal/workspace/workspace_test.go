package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupTestRepo creates a minimal git repo with one commit at basePath/name.
// It also sets origin to itself so fetch/checkout operations work.
func setupTestRepo(t *testing.T, basePath, name string) string {
	t.Helper()
	repoPath := filepath.Join(basePath, name)
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cmds := [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
		{"remote", "add", "origin", repoPath},
		{"fetch", "origin"},
	}
	for _, args := range cmds {
		if _, err := runGit(repoPath, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return repoPath
}

// --- SetupWorkspace tests ---

func TestSetupWorkspace(t *testing.T) {
	t.Run("given empty repo then returns error", func(t *testing.T) {
		_, err := SetupWorkspace("", "PROJ-123", "", "", "", "", false)
		if err == nil || !strings.Contains(err.Error(), "repo is required") {
			t.Fatalf("expected 'repo is required' error, got: %v", err)
		}
	})

	t.Run("given empty ticket then returns error", func(t *testing.T) {
		_, err := SetupWorkspace("myapp", "", "", "", "", "", false)
		if err == nil || !strings.Contains(err.Error(), "ticket or branch is required") {
			t.Fatalf("expected 'ticket or branch is required' error, got: %v", err)
		}
	})

	t.Run("given unset RICK_REPOS_PATH then returns error", func(t *testing.T) {
		t.Setenv("RICK_REPOS_PATH", "")
		_, err := SetupWorkspace("myapp", "PROJ-123", "", "", "", "", false)
		if err == nil || !strings.Contains(err.Error(), "RICK_REPOS_PATH") {
			t.Fatalf("expected RICK_REPOS_PATH error, got: %v", err)
		}
	})

	t.Run("given missing repo then returns error", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("RICK_REPOS_PATH", tmp)
		_, err := SetupWorkspace("nonexistent", "PROJ-123", "", "", "", "", false)
		if err == nil || !strings.Contains(err.Error(), "repository not found") {
			t.Fatalf("expected 'repository not found' error, got: %v", err)
		}
	})

	t.Run("given valid repo when creating non-isolated branch then succeeds", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("RICK_REPOS_PATH", tmp)
		setupTestRepo(t, tmp, "myapp")

		ws, err := SetupWorkspace("myapp", "PROJ-99999", "", "", "", "", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ws.Branch != "PROJ-99999" {
			t.Errorf("expected branch PROJ-99999, got: %s", ws.Branch)
		}
		if ws.Isolated {
			t.Error("expected Isolated=false")
		}
		if ws.Base != "main" {
			t.Errorf("expected base main, got: %s", ws.Base)
		}

		// Verify branch was created in original repo.
		branch, err := runGit(filepath.Join(tmp, "myapp"), "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			t.Fatalf("rev-parse: %v", err)
		}
		if branch != "PROJ-99999" {
			t.Errorf("expected branch PROJ-99999, got: %s", branch)
		}
	})

	t.Run("given valid repo when creating isolated clone then succeeds", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("RICK_REPOS_PATH", tmp)
		setupTestRepo(t, tmp, "myapp")

		ws, err := SetupWorkspace("myapp", "PROJ-88888", "", "", "", "", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !ws.Isolated {
			t.Error("expected Isolated=true")
		}

		// Verify clone exists and is on correct branch.
		clonePath := filepath.Join(tmp, "myapp-PROJ-88888")
		if ws.Path != clonePath {
			t.Errorf("expected path %s, got: %s", clonePath, ws.Path)
		}
		if _, err := os.Stat(clonePath); err != nil {
			t.Fatalf("clone directory missing: %v", err)
		}
		branch, err := runGit(clonePath, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			t.Fatalf("rev-parse: %v", err)
		}
		if branch != "PROJ-88888" {
			t.Errorf("expected branch PROJ-88888, got: %s", branch)
		}

		// Original repo should be unaffected.
		origBranch, _ := runGit(filepath.Join(tmp, "myapp"), "rev-parse", "--abbrev-ref", "HEAD")
		if origBranch == "PROJ-88888" {
			t.Error("original repo should not be on the new branch")
		}
	})

	t.Run("given valid repo when creating isolated clone with suffix then uses suffix in name", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("RICK_REPOS_PATH", tmp)
		setupTestRepo(t, tmp, "myapp")

		ws, err := SetupWorkspace("myapp", "PROJ-77777", "", "", "task1", "", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		expected := filepath.Join(tmp, "myapp-PROJ-77777-task1")
		if ws.Path != expected {
			t.Errorf("expected path %s, got: %s", expected, ws.Path)
		}
		if _, err := os.Stat(expected); err != nil {
			t.Fatalf("clone directory with suffix missing: %v", err)
		}
	})

	t.Run("given isolated clone then drops .rick/workspace.yaml and excludes from git", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("RICK_REPOS_PATH", tmp)
		setupTestRepo(t, tmp, "myapp")

		ws, err := SetupWorkspace("myapp", "PROJ-MARKER", "", "", "abcd1234", "corr-xyz-789", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// .rick/workspace.yaml exists with the expected metadata.
		markerPath := filepath.Join(ws.Path, ".rick", "workspace.yaml")
		data, err := os.ReadFile(markerPath)
		if err != nil {
			t.Fatalf("marker missing: %v", err)
		}
		marker := string(data)
		for _, want := range []string{
			"branch: PROJ-MARKER",
			"base: main",
			"isolated: true",
			"do_not_cd_out: true",
			"correlation_id: corr-xyz-789",
			"path: " + ws.Path,
		} {
			if !strings.Contains(marker, want) {
				t.Errorf("marker missing %q\nfull marker:\n%s", want, marker)
			}
		}

		// .git/info/exclude must contain .rick/ so the marker stays out of git.
		excludeData, err := os.ReadFile(filepath.Join(ws.Path, ".git", "info", "exclude"))
		if err != nil {
			t.Fatalf("read exclude: %v", err)
		}
		if !strings.Contains(string(excludeData), ".rick/") {
			t.Errorf("expected .rick/ in .git/info/exclude, got:\n%s", excludeData)
		}

		// git status must report a clean tree — if the marker leaks into the
		// index, the commit phase will accidentally stage it.
		status, err := runGit(ws.Path, "status", "--porcelain")
		if err != nil {
			t.Fatalf("git status: %v", err)
		}
		if status != "" {
			t.Errorf("expected clean git status, got:\n%s", status)
		}
	})

	t.Run("given valid repo when specifying custom base then uses that base", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("RICK_REPOS_PATH", tmp)
		repoPath := setupTestRepo(t, tmp, "myapp")

		// Create a develop branch.
		_, _ = runGit(repoPath, "checkout", "-b", "develop")
		_, _ = runGit(repoPath, "commit", "--allow-empty", "-m", "develop commit")
		_, _ = runGit(repoPath, "checkout", "main")
		_, _ = runGit(repoPath, "fetch", "origin")

		ws, err := SetupWorkspace("myapp", "PROJ-66666", "", "develop", "", "", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ws.Base != "develop" {
			t.Errorf("expected base develop, got: %s", ws.Base)
		}
	})

	t.Run("given existing branch when calling setup then reuses branch and resets to origin", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("RICK_REPOS_PATH", tmp)
		repoPath := setupTestRepo(t, tmp, "myapp")

		// Pre-create the branch with an extra commit.
		_, _ = runGit(repoPath, "checkout", "-b", "PROJ-55555")
		_, _ = runGit(repoPath, "commit", "--allow-empty", "-m", "extra")
		_, _ = runGit(repoPath, "checkout", "main")

		ws, err := SetupWorkspace("myapp", "PROJ-55555", "", "", "", "", false)
		if err != nil {
			t.Fatalf("expected success when branch exists, got: %v", err)
		}
		if ws.Branch != "PROJ-55555" {
			t.Errorf("expected branch PROJ-55555, got: %s", ws.Branch)
		}

		// Verify we're on the branch and it was reset to origin/main.
		branch, _ := runGit(repoPath, "rev-parse", "--abbrev-ref", "HEAD")
		if branch != "PROJ-55555" {
			t.Errorf("expected HEAD on PROJ-55555, got: %s", branch)
		}
		// The extra commit should be gone after reset.
		originMain, _ := runGit(repoPath, "rev-parse", "origin/main")
		head, _ := runGit(repoPath, "rev-parse", "HEAD")
		if head != originMain {
			t.Errorf("expected HEAD to match origin/main after reset")
		}
	})

	t.Run("given existing destination when calling isolated setup then cleans up and recreates", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("RICK_REPOS_PATH", tmp)
		setupTestRepo(t, tmp, "myapp")

		// Pre-create the destination with a marker file.
		destPath := filepath.Join(tmp, "myapp-PROJ-44444")
		_ = os.MkdirAll(destPath, 0o755)
		_ = os.WriteFile(filepath.Join(destPath, "stale-marker.txt"), []byte("old"), 0o644)

		ws, err := SetupWorkspace("myapp", "PROJ-44444", "", "", "", "", true)
		if err != nil {
			t.Fatalf("expected success after cleaning stale destination, got: %v", err)
		}
		if !ws.Isolated {
			t.Error("expected Isolated=true")
		}

		// Stale marker should be gone (directory was cleaned and recreated).
		if _, err := os.Stat(filepath.Join(destPath, "stale-marker.txt")); !os.IsNotExist(err) {
			t.Error("stale marker should have been removed")
		}
		// New workspace should exist with .git.
		if _, err := os.Stat(filepath.Join(destPath, ".git")); err != nil {
			t.Error("expected .git in recreated workspace")
		}
	})

	t.Run("given isolated setup with existing branch then succeeds with reset", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("RICK_REPOS_PATH", tmp)
		repoPath := setupTestRepo(t, tmp, "myapp")

		// Pre-create the branch in the source — this used to fail, now handled.
		_, _ = runGit(repoPath, "checkout", "-b", "PROJ-33333")
		_, _ = runGit(repoPath, "checkout", "main")

		ws, err := SetupWorkspace("myapp", "PROJ-33333", "", "", "", "", true)
		if err != nil {
			t.Fatalf("expected success for isolated setup with existing branch, got: %v", err)
		}
		if !ws.Isolated {
			t.Error("expected Isolated=true")
		}
		if ws.Branch != "PROJ-33333" {
			t.Errorf("expected branch PROJ-33333, got: %s", ws.Branch)
		}
	})

	t.Run("given branch override then checks out existing remote branch", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("RICK_REPOS_PATH", tmp)
		repoPath := setupTestRepo(t, tmp, "myapp")

		// Create a feature branch with a commit on origin.
		_, _ = runGit(repoPath, "checkout", "-b", "PROJ-33393")
		_, _ = runGit(repoPath, "commit", "--allow-empty", "-m", "PR commit")
		prHead, _ := runGit(repoPath, "rev-parse", "HEAD")
		_, _ = runGit(repoPath, "checkout", "main")
		_, _ = runGit(repoPath, "fetch", "origin")

		// Use branch override — ticket is different from the branch name.
		ws, err := SetupWorkspace("myapp", "PROJ-33398", "PROJ-33393", "", "", "", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ws.Branch != "PROJ-33393" {
			t.Errorf("expected branch PROJ-33393 (override), got: %s", ws.Branch)
		}

		// Verify HEAD matches the PR branch, not origin/main.
		head, _ := runGit(repoPath, "rev-parse", "HEAD")
		if head != prHead {
			t.Errorf("expected HEAD to match PR branch commit, got different SHA")
		}
	})

	t.Run("given branch override without ticket then succeeds", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("RICK_REPOS_PATH", tmp)
		repoPath := setupTestRepo(t, tmp, "myapp")

		// Create a branch on origin.
		_, _ = runGit(repoPath, "checkout", "-b", "feature-branch")
		_, _ = runGit(repoPath, "commit", "--allow-empty", "-m", "feature")
		_, _ = runGit(repoPath, "checkout", "main")
		_, _ = runGit(repoPath, "fetch", "origin")

		ws, err := SetupWorkspace("myapp", "", "feature-branch", "", "", "", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ws.Branch != "feature-branch" {
			t.Errorf("expected branch feature-branch, got: %s", ws.Branch)
		}
	})

	t.Run("given isolated setup failure on invalid base then cleans up copy", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("RICK_REPOS_PATH", tmp)
		setupTestRepo(t, tmp, "myapp")

		// Use a non-existent base branch to trigger a real failure.
		_, err := SetupWorkspace("myapp", "PROJ-22222", "", "nonexistent-base", "", "", true)
		if err == nil {
			t.Fatal("expected error for invalid base branch")
		}

		// The isolated copy should have been cleaned up.
		clonePath := filepath.Join(tmp, "myapp-PROJ-22222")
		if _, statErr := os.Stat(clonePath); !os.IsNotExist(statErr) {
			t.Errorf("expected clone to be cleaned up, but it exists: %s", clonePath)
		}
	})
}

// --- CleanupIsolatedWorkspace tests ---

func TestCleanupIsolatedWorkspace(t *testing.T) {
	t.Run("given existing directory then removes it", func(t *testing.T) {
		tmp := t.TempDir()
		dirPath := filepath.Join(tmp, "myapp-PROJ-12345")
		_ = os.MkdirAll(dirPath, 0o755)
		_ = os.WriteFile(filepath.Join(dirPath, "test.txt"), []byte("data"), 0o644)

		CleanupIsolatedWorkspace(dirPath)

		if _, err := os.Stat(dirPath); !os.IsNotExist(err) {
			t.Error("directory should have been removed")
		}
	})

	t.Run("given nonexistent path then does not panic", func(t *testing.T) {
		// Should be a no-op, not panic.
		CleanupIsolatedWorkspace("/nonexistent/path/myapp-PROJ-99999")
	})
}

// --- workspaceParams validation tests ---

// workspaceParams holds optional workspace configuration for handler use.
type workspaceParams struct {
	Repo   string
	Ticket string
}

// validateWorkspaceParams checks that Repo and Ticket are either both set or both empty.
func validateWorkspaceParams(p workspaceParams) error {
	if p.Repo != "" && p.Ticket == "" {
		return fmt.Errorf("ticket is required when repo is set")
	}
	if p.Ticket != "" && p.Repo == "" {
		return fmt.Errorf("repo is required when ticket is set")
	}
	return nil
}

func TestValidateWorkspaceParams(t *testing.T) {
	t.Run("given both repo and ticket then no error", func(t *testing.T) {
		err := validateWorkspaceParams(workspaceParams{Repo: "myapp", Ticket: "PROJ-123"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("given neither repo nor ticket then no error", func(t *testing.T) {
		err := validateWorkspaceParams(workspaceParams{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("given repo without ticket then returns error", func(t *testing.T) {
		err := validateWorkspaceParams(workspaceParams{Repo: "myapp"})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("given ticket without repo then returns error", func(t *testing.T) {
		err := validateWorkspaceParams(workspaceParams{Ticket: "PROJ-123"})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}
