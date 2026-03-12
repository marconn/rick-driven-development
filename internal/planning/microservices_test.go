package planning

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListAvailable_ScansGitRepos(t *testing.T) {
	dir := t.TempDir()
	msMap := NewMicroserviceMap(dir)

	// Create some directories -- only dirs with .git should be listed.
	gitRepo := filepath.Join(dir, "frontend-emr")
	_ = os.Mkdir(gitRepo, 0o755)
	_ = os.Mkdir(filepath.Join(gitRepo, ".git"), 0o755)

	nonRepo := filepath.Join(dir, "random-dir")
	_ = os.Mkdir(nonRepo, 0o755)

	gitRepo2 := filepath.Join(dir, "bff-emr")
	_ = os.Mkdir(gitRepo2, 0o755)
	_ = os.Mkdir(filepath.Join(gitRepo2, ".git"), 0o755)

	repos := msMap.ListAvailable()
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos, got %d: %v", len(repos), repos)
	}

	repoSet := make(map[string]bool)
	for _, r := range repos {
		repoSet[r] = true
	}
	if !repoSet["frontend-emr"] {
		t.Error("expected frontend-emr in available repos")
	}
	if !repoSet["bff-emr"] {
		t.Error("expected bff-emr in available repos")
	}
	if repoSet["random-dir"] {
		t.Error("random-dir should not be in available repos (no .git)")
	}
}

func TestListAvailable_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	msMap := NewMicroserviceMap(dir)

	repos := msMap.ListAvailable()
	if len(repos) != 0 {
		t.Errorf("expected 0 repos for empty dir, got %d", len(repos))
	}
}

func TestListAvailable_InvalidPath(t *testing.T) {
	msMap := NewMicroserviceMap("/nonexistent/path/that/does/not/exist")

	repos := msMap.ListAvailable()
	if repos != nil {
		t.Errorf("expected nil for nonexistent path, got %v", repos)
	}
}

func TestResolveAll_FallbackToDirectoryName(t *testing.T) {
	dir := t.TempDir()
	msMap := NewMicroserviceMap(dir)

	// Create a repo that matches a microservice name
	repoDir := filepath.Join(dir, "practice-web")
	_ = os.Mkdir(repoDir, 0o755)

	found, missing := msMap.ResolveAll([]string{"practice-web", "nonexistent-service"})
	if len(found) != 1 {
		t.Fatalf("expected 1 found, got %d", len(found))
	}
	if found["practice-web"] != repoDir {
		t.Errorf("expected path %s, got %s", repoDir, found["practice-web"])
	}
	if len(missing) != 1 || missing[0] != "nonexistent-service" {
		t.Errorf("expected [nonexistent-service] missing, got %v", missing)
	}
}

func TestLoadFromFile_AgentsMD_ParsesRepoTable(t *testing.T) {
	dir := t.TempDir()
	msMap := NewMicroserviceMap(dir)

	// Write a minimal AGENTS.md with a Repository Index table
	// and a Table of Contents that also mentions "Repository Index".
	agentsContent := `# Platform Guide

## Table of Contents
- [Repository Index](#repository-index)
- [Resources](#resources)

## Platform Overview

| Product | Description |
|---------|-------------|
| Foo | Bar |

## Repository Index

| Repository | Category | Status | Description |
|------------|----------|--------|-------------|
| myapp | Core Platform | active | Main application |
| booking | Core Platform | active | Appointment scheduling |
| practice-web | Frontend | active | Doctor-facing SPA |

## Resources

Some links.
`
	agentsPath := dir + "/AGENTS.md"
	_ = os.WriteFile(agentsPath, []byte(agentsContent), 0o644)

	if err := msMap.LoadFromFile(agentsPath); err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}

	names := msMap.Names()
	if len(names) != 3 {
		t.Fatalf("expected 3 repos from AGENTS.md, got %d: %v", len(names), names)
	}

	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}
	for _, expected := range []string{"myapp", "booking", "practice-web"} {
		if !nameSet[expected] {
			t.Errorf("expected repo %s not found in %v", expected, names)
		}
	}

	// Platform context should be stored
	if msMap.PlatformContext() == "" {
		t.Error("expected non-empty platform context")
	}
}

func TestReposPath(t *testing.T) {
	msMap := NewMicroserviceMap("/some/path")
	if msMap.ReposPath() != "/some/path" {
		t.Errorf("expected /some/path, got %s", msMap.ReposPath())
	}
}
