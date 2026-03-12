package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- safeWorkspacePath security tests ---

func TestSafeWorkspacePath_ValidPath(t *testing.T) {
	dir := t.TempDir()
	// Create a valid workspace directory matching the *-rick-ws-* pattern.
	wsDir := filepath.Join(dir, "myapp-rick-ws-1234")
	if err := os.MkdirAll(wsDir, 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RICK_REPOS_PATH", dir)

	resolved, err := safeWorkspacePath(wsDir)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if resolved == "" {
		t.Error("expected non-empty resolved path")
	}
}

func TestSafeWorkspacePath_HuliPathNotSet(t *testing.T) {
	t.Setenv("RICK_REPOS_PATH", "")

	_, err := safeWorkspacePath("/some/path/foo-rick-ws-1234")
	if err == nil {
		t.Fatal("expected error when RICK_REPOS_PATH not set")
	}
	if !strings.Contains(err.Error(), "RICK_REPOS_PATH") {
		t.Errorf("expected RICK_REPOS_PATH mention in error, got: %v", err)
	}
}

func TestSafeWorkspacePath_OutsideHulipath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RICK_REPOS_PATH", dir)

	// Point to a directory outside RICK_REPOS_PATH.
	_, err := safeWorkspacePath("/etc/passwd-rick-ws-1234")
	if err == nil {
		t.Fatal("expected error for path outside RICK_REPOS_PATH")
	}
	if !strings.Contains(err.Error(), "refusing to delete path outside") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSafeWorkspacePath_NoHuliPattern(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "myrepo-nopattern")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RICK_REPOS_PATH", dir)

	_, err := safeWorkspacePath(subDir)
	if err == nil {
		t.Fatal("expected error for path not matching *-rick-ws-* pattern")
	}
	if !strings.Contains(err.Error(), "refusing to delete path not matching") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSafeWorkspacePath_PathTraversalAttempt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RICK_REPOS_PATH", dir)

	// Attempt path traversal via ../
	traversal := filepath.Join(dir, "workspace-rick-ws-1234", "..", "..", "etc", "passwd-rick-ws-bad")
	_, err := safeWorkspacePath(traversal)
	if err == nil {
		t.Fatal("expected error for path traversal attempt")
	}
}

func TestSafeWorkspacePath_RickWsPattern(t *testing.T) {
	dir := t.TempDir()
	wsDir := filepath.Join(dir, "myapp-rick-ws-1234")
	if err := os.MkdirAll(wsDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RICK_REPOS_PATH", dir)

	// -rick-ws- pattern should be accepted.
	resolved, err := safeWorkspacePath(wsDir)
	if err != nil {
		t.Fatalf("expected success for rick-ws pattern, got: %v", err)
	}
	if resolved == "" {
		t.Error("expected non-empty resolved path")
	}
}

func TestSafeWorkspacePath_SymlinkOutside(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()

	// Create a symlink inside RICK_REPOS_PATH that points outside.
	symlinkPath := filepath.Join(dir, "repo-rick-ws-9999")
	if err := os.Symlink(outside, symlinkPath); err != nil {
		t.Skip("symlinks not supported: " + err.Error())
	}

	t.Setenv("RICK_REPOS_PATH", dir)

	_, err := safeWorkspacePath(symlinkPath)
	if err == nil {
		t.Fatal("expected error for symlink pointing outside RICK_REPOS_PATH")
	}
}

// --- toolWorkspaceList tests ---

func TestToolWorkspaceList_NoHulipath(t *testing.T) {
	t.Setenv("RICK_REPOS_PATH", "")

	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	defer s.Close()

	_, err := s.toolWorkspaceList(context.TODO(), nil)
	if err == nil {
		t.Fatal("expected error when RICK_REPOS_PATH is not set")
	}
	if !strings.Contains(err.Error(), "RICK_REPOS_PATH") {
		t.Errorf("expected RICK_REPOS_PATH mention, got: %v", err)
	}
}

func TestToolWorkspaceList_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RICK_REPOS_PATH", dir)

	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := s.toolWorkspaceList(context.TODO(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wlr, ok := result.(workspaceListResult)
	if !ok {
		t.Fatalf("unexpected result type: %T", result)
	}
	if wlr.Count != 0 {
		t.Errorf("expected 0 workspaces, got %d", wlr.Count)
	}
}

func TestToolWorkspaceList_FiltersNonHuliDirs(t *testing.T) {
	dir := t.TempDir()
	// Create mix of rick-ws and non-rick-ws directories.
	for _, name := range []string{"myapp-rick-ws-123", "plain-repo", "frontend-rick-ws-456"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("RICK_REPOS_PATH", dir)

	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	defer s.Close()

	result, err := s.toolWorkspaceList(context.TODO(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wlr := result.(workspaceListResult)
	if wlr.Count != 2 {
		t.Errorf("expected 2 rick-ws workspaces, got %d", wlr.Count)
	}
	for _, ws := range wlr.Workspaces {
		if !strings.Contains(ws.Name, "-rick-ws-") {
			t.Errorf("workspace %q should match -rick-ws- pattern", ws.Name)
		}
	}
}
