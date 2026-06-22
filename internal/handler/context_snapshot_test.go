package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// writeFile is a test helper that writes a file and fails the test on error.
func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// mustUnmarshal is a test helper that unmarshals JSON and fails on error.
func mustUnmarshal(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}

func TestContextSnapshotName(t *testing.T) {
	h := NewContextSnapshot(testDeps())
	if h.Name() != "context-snapshot" {
		t.Errorf("want name 'context-snapshot', got %q", h.Name())
	}
}

func TestContextSnapshotSubscribes(t *testing.T) {
	h := NewContextSnapshot(testDeps())
	// Subscribes returns nil for DAG-dispatched handlers — subscriptions are
	// derived from workflow Graph definitions at runtime.
	subs := h.Subscribes()
	if subs != nil {
		t.Errorf("want nil subscriptions for DAG-dispatched handler, got %v", subs)
	}
}

func TestContextSnapshotEmptyCorrelation(t *testing.T) {
	h := NewContextSnapshot(testDeps())
	env := event.New(event.PersonaCompleted, 1, nil)
	got, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty correlation, got %v", got)
	}
}

func TestContextSnapshotNoWorkspaceReady(t *testing.T) {
	// Correlation chain exists but no WorkspaceReady → no-op
	store := newMockStore()
	store.correlationEvents["corr-1"] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "fix bug",
		})).WithCorrelation("corr-1"),
	}

	h := &ContextSnapshotHandler{store: store}
	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-1")
	got, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for no workspace, got %d events", len(got))
	}
}

func TestContextSnapshotEmitsCodebaseEvent(t *testing.T) {
	tmp := t.TempDir()
	// Create a minimal Go project
	writeFile(t, filepath.Join(tmp, "go.mod"), []byte("module example.com/test\n\ngo 1.24\n"))
	writeFile(t, filepath.Join(tmp, "main.go"), []byte("package main\nfunc main() {}\n"))
	writeFile(t, filepath.Join(tmp, "internal", "handler.go"), []byte("package internal\n"))

	store := newMockStore()
	store.correlationEvents["corr-2"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
			Path:   tmp,
			Branch: "issue-1",
			Base:   "main",
		})).WithCorrelation("corr-2"),
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "fix handler bug",
		})).WithCorrelation("corr-2"),
	}

	h := &ContextSnapshotHandler{store: store}
	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-2")
	got, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should emit at least context.codebase
	if len(got) == 0 {
		t.Fatal("expected at least 1 event")
	}
	if got[0].Type != event.ContextCodebase {
		t.Errorf("expected ContextCodebase, got %s", got[0].Type)
	}

	var payload event.ContextCodebasePayload
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Language != "go" {
		t.Errorf("expected language go, got %s", payload.Language)
	}
	if len(payload.Tree) < 3 {
		t.Errorf("expected at least 3 tree entries, got %d", len(payload.Tree))
	}
	// go.mod and main.go should be in Files (key files)
	foundGoMod := false
	foundMain := false
	for _, f := range payload.Files {
		if f.Path == "go.mod" {
			foundGoMod = true
		}
		if f.Path == "main.go" {
			foundMain = true
		}
	}
	if !foundGoMod {
		t.Error("go.mod should be a key file")
	}
	if !foundMain {
		t.Error("main.go should be a key file")
	}
}

func TestHasHeavyClaudeContext(t *testing.T) {
	big := make([]byte, heavyClaudeContextBytes+1)

	t.Run("no claude context", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, "main.go"), []byte("package main\n"))
		if hasHeavyClaudeContext(tmp) {
			t.Error("empty repo should not have heavy claude context")
		}
	})

	t.Run("small CLAUDE.md is not heavy", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, "CLAUDE.md"), []byte("# small\n"))
		if hasHeavyClaudeContext(tmp) {
			t.Error("small CLAUDE.md should not count as heavy")
		}
	})

	t.Run("large .claude/rules is heavy", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, ".claude", "rules", "go-backend.md"), big)
		if !hasHeavyClaudeContext(tmp) {
			t.Error("large .claude/rules should be heavy")
		}
	})

	t.Run("large root CLAUDE.md is heavy", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, "CLAUDE.md"), big)
		if !hasHeavyClaudeContext(tmp) {
			t.Error("large CLAUDE.md should be heavy")
		}
	})

	t.Run("non-md files under .claude/rules ignored", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, ".claude", "rules", "data.bin"), big)
		if hasHeavyClaudeContext(tmp) {
			t.Error("non-.md files should not count toward claude context")
		}
	})
}

// Fix: on repos that auto-load heavy Claude context, the file-tree dump is
// redundant for a claude developer (it reads .claude/rules and globs itself)
// but other backends still need it. snapshotCodebase must drop the tree only
// for claude+heavy, and always keep key files.
func TestContextSnapshotTreeGatingByBackend(t *testing.T) {
	// Build a workspace that ships heavy .claude/rules plus real source.
	newWorkspace := func(t *testing.T) (store *mockStore, corr string) {
		t.Helper()
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, ".claude", "rules", "go-backend.md"),
			make([]byte, heavyClaudeContextBytes+1))
		writeFile(t, filepath.Join(tmp, "go.mod"), []byte("module example.com/test\n\ngo 1.24\n"))
		writeFile(t, filepath.Join(tmp, "main.go"), []byte("package main\nfunc main() {}\n"))

		store = newMockStore()
		store.correlationEvents["corr-tree"] = []event.Envelope{
			event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
				Path: tmp, Branch: "issue-1", Base: "main",
			})).WithCorrelation("corr-tree"),
			event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
				Prompt: "fix bug",
			})).WithCorrelation("corr-tree"),
		}
		return store, "corr-tree"
	}

	codebaseOf := func(t *testing.T, h *ContextSnapshotHandler, corr string) event.ContextCodebasePayload {
		t.Helper()
		env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation(corr)
		got, err := h.Handle(context.Background(), env)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) == 0 || got[0].Type != event.ContextCodebase {
			t.Fatalf("expected ContextCodebase first, got %v", got)
		}
		var p event.ContextCodebasePayload
		mustUnmarshal(t, got[0].Payload, &p)
		return p
	}

	hasKeyFile := func(p event.ContextCodebasePayload, name string) bool {
		for _, f := range p.Files {
			if f.Path == name {
				return true
			}
		}
		return false
	}

	t.Run("claude + heavy context: tree omitted, key files kept", func(t *testing.T) {
		store, corr := newWorkspace(t)
		h := &ContextSnapshotHandler{store: store, developerBackend: "claude"}
		p := codebaseOf(t, h, corr)
		if len(p.Tree) != 0 {
			t.Errorf("expected no tree entries for claude+heavy, got %d", len(p.Tree))
		}
		if !hasKeyFile(p, "go.mod") || !hasKeyFile(p, "main.go") {
			t.Errorf("key files must survive tree trim, got %+v", p.Files)
		}
	})

	t.Run("non-claude backend keeps tree even with heavy context", func(t *testing.T) {
		store, corr := newWorkspace(t)
		h := &ContextSnapshotHandler{store: store, developerBackend: "gemini"}
		p := codebaseOf(t, h, corr)
		if len(p.Tree) == 0 {
			t.Error("non-claude backend must keep the file tree")
		}
	})
}

func TestContextSnapshotTaskRelevance(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "go.mod"), []byte("module test\n"))
	writeFile(t, filepath.Join(tmp, "internal", "model", "clinic", "db.go"), []byte("package clinic\n"))
	writeFile(t, filepath.Join(tmp, "internal", "unrelated", "other.go"), []byte("package other\n"))

	store := newMockStore()
	store.correlationEvents["corr-3"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{
			Path: tmp,
		})).WithCorrelation("corr-3"),
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "fix clinic NULL scan error",
		})).WithCorrelation("corr-3"),
	}

	h := &ContextSnapshotHandler{store: store}
	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-3")
	got, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var payload event.ContextCodebasePayload
	mustUnmarshal(t, got[0].Payload, &payload)

	// "clinic" appears in the task and in the file path → should be a key file
	foundClinic := false
	for _, f := range payload.Files {
		if f.Path == filepath.Join("internal", "model", "clinic", "db.go") {
			foundClinic = true
		}
	}
	if !foundClinic {
		t.Error("clinic/db.go should be included based on task relevance")
	}
}

func TestContextSnapshotSkipsDirs(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "go.mod"), []byte("module test\n"))
	writeFile(t, filepath.Join(tmp, "vendor", "github.com", "foo", "bar.go"), []byte("package foo\n"))
	writeFile(t, filepath.Join(tmp, "node_modules", "lodash", "index.js"), []byte("module.exports = {}"))
	writeFile(t, filepath.Join(tmp, "main.go"), []byte("package main\n"))

	store := newMockStore()
	store.correlationEvents["corr-4"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp})).WithCorrelation("corr-4"),
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "test"})).WithCorrelation("corr-4"),
	}

	h := &ContextSnapshotHandler{store: store}
	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-4")
	got, _ := h.Handle(context.Background(), env)

	var payload event.ContextCodebasePayload
	mustUnmarshal(t, got[0].Payload, &payload)

	for _, entry := range payload.Tree {
		if filepath.Base(entry.Path) == "bar.go" || filepath.Base(entry.Path) == "index.js" {
			t.Errorf("vendor/node_modules file should be skipped: %s", entry.Path)
		}
	}
}

func TestContextSnapshotSchemaDetection(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "go.mod"), []byte("module test\n"))
	writeFile(t, filepath.Join(tmp, "api", "proto", "service.proto"), []byte("syntax = \"proto3\";\n"))
	writeFile(t, filepath.Join(tmp, "migrations", "001_init.sql"), []byte("CREATE TABLE users (id INT);\n"))

	store := newMockStore()
	store.correlationEvents["corr-5"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp})).WithCorrelation("corr-5"),
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "test"})).WithCorrelation("corr-5"),
	}

	h := &ContextSnapshotHandler{store: store}
	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-5")
	got, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have codebase + schema events
	hasCodebase := false
	hasSchema := false
	for _, e := range got {
		switch e.Type {
		case event.ContextCodebase:
			hasCodebase = true
		case event.ContextSchema:
			hasSchema = true
			var sp event.ContextSchemaPayload
			mustUnmarshal(t, e.Payload, &sp)
			if len(sp.Proto) != 1 {
				t.Errorf("expected 1 proto file, got %d", len(sp.Proto))
			}
			if len(sp.SQL) != 1 {
				t.Errorf("expected 1 SQL file, got %d", len(sp.SQL))
			}
		}
	}
	if !hasCodebase {
		t.Error("missing context.codebase event")
	}
	if !hasSchema {
		t.Error("missing context.schema event")
	}
}

func TestContextSnapshotGitDetection(t *testing.T) {
	tmp := t.TempDir()
	setupTestGitRepo(t, tmp, "repo")
	repoPath := filepath.Join(tmp, "repo")
	writeFile(t, filepath.Join(repoPath, "go.mod"), []byte("module test\n"))

	store := newMockStore()
	store.correlationEvents["corr-6"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{Path: repoPath})).WithCorrelation("corr-6"),
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "test"})).WithCorrelation("corr-6"),
	}

	h := &ContextSnapshotHandler{store: store}
	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-6")
	got, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	hasGit := false
	for _, e := range got {
		if e.Type == event.ContextGit {
			hasGit = true
			var gp event.ContextGitPayload
			mustUnmarshal(t, e.Payload, &gp)
			if gp.HEAD == "" {
				t.Error("expected non-empty HEAD")
			}
			if len(gp.RecentLog) == 0 {
				t.Error("expected at least 1 commit in log")
			}
		}
	}
	if !hasGit {
		t.Error("missing context.git event")
	}
}

// ---------------------------------------------------------------------------
// Budget limits
// ---------------------------------------------------------------------------

func TestContextSnapshotMaxTreeEntries(t *testing.T) {
	// Create a workspace with more than maxTreeEntries (500) files.
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "go.mod"), []byte("module test\n"))

	// Create 520 files — well over the 500 limit.
	for i := range 520 {
		name := filepath.Join(tmp, "internal", "gen", fmt.Sprintf("file_%04d.go", i))
		writeFile(t, name, []byte(fmt.Sprintf("package gen\n// file %d\n", i)))
	}

	store := newMockStore()
	store.correlationEvents["corr-tree-limit"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp})).WithCorrelation("corr-tree-limit"),
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "test"})).WithCorrelation("corr-tree-limit"),
	}

	h := &ContextSnapshotHandler{store: store}
	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-tree-limit")
	got, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least 1 event")
	}

	var payload event.ContextCodebasePayload
	mustUnmarshal(t, got[0].Payload, &payload)

	// Tree should be capped at maxTreeEntries.
	if len(payload.Tree) > maxTreeEntries {
		t.Errorf("tree entries should be capped at %d, got %d", maxTreeEntries, len(payload.Tree))
	}
}

func TestContextSnapshotMaxSingleFileSizeExcludes(t *testing.T) {
	// Files larger than maxSingleFile (10KB) should be excluded from Files (key files).
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "go.mod"), []byte("module test\n"))

	// Create a large file (> 10KB) that would otherwise be a key file (main.go).
	largeContent := make([]byte, maxSingleFile+100)
	for i := range largeContent {
		largeContent[i] = 'x'
	}
	// Overwrite main.go with large content — it's normally a key file.
	writeFile(t, filepath.Join(tmp, "main.go"), largeContent)

	// Create a small key file to verify small files still work.
	writeFile(t, filepath.Join(tmp, "small.go"), []byte("package main\n// small file\n"))

	store := newMockStore()
	store.correlationEvents["corr-filesize"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, event.MustMarshal(event.WorkspaceReadyPayload{Path: tmp})).WithCorrelation("corr-filesize"),
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "fix small"})).WithCorrelation("corr-filesize"),
	}

	h := &ContextSnapshotHandler{store: store}
	env := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-filesize")
	got, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least 1 event")
	}

	var payload event.ContextCodebasePayload
	mustUnmarshal(t, got[0].Payload, &payload)

	// main.go must NOT appear in Files because it exceeds maxSingleFile.
	for _, f := range payload.Files {
		if f.Path == "main.go" {
			t.Errorf("main.go (%d+ bytes) should be excluded from Files due to size limit", maxSingleFile)
		}
	}

	// The tree should still include main.go (tree always captures all files).
	foundInTree := false
	for _, entry := range payload.Tree {
		if entry.Path == "main.go" {
			foundInTree = true
		}
	}
	if !foundInTree {
		t.Error("main.go should still appear in Tree even if excluded from Files")
	}
}

func TestLanguageDetection(t *testing.T) {
	tests := []struct {
		file string
		want string
	}{
		{"go.mod", "go"},
		{"package.json", "typescript"},
		{"Cargo.toml", "rust"},
		{"pyproject.toml", "python"},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			tmp := t.TempDir()
			writeFile(t, filepath.Join(tmp, tt.file), []byte("content"))
			got := detectLanguage(tmp)
			if got != tt.want {
				t.Errorf("detectLanguage(%s): want %s, got %s", tt.file, tt.want, got)
			}
		})
	}
}

func TestFrameworkDetection(t *testing.T) {
	t.Run("go-grpc", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, "go.mod"), []byte("module test\nrequire google.golang.org/grpc v1.60.0\n"))
		got := detectFramework(tmp)
		if got != "go-grpc" {
			t.Errorf("want go-grpc, got %s", got)
		}
	})
	t.Run("vue-webpack", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, "package.json"), []byte(`{"dependencies":{"vue":"^3.0.0"}}`))
		got := detectFramework(tmp)
		if got != "vue-webpack" {
			t.Errorf("want vue-webpack, got %s", got)
		}
	})
}

func TestLangFromExt(t *testing.T) {
	tests := map[string]string{
		".go": "go", ".ts": "typescript", ".py": "python",
		".proto": "protobuf", ".sql": "sql", ".unknown": "",
	}
	for ext, want := range tests {
		if got := langFromExt(ext); got != want {
			t.Errorf("langFromExt(%s): want %s, got %s", ext, want, got)
		}
	}
}

func TestIsKeyFile(t *testing.T) {
	// Always-key files
	for _, f := range []string{"go.mod", "main.go", "README.md", "Dockerfile"} {
		if !isKeyFile(f, "") {
			t.Errorf("%s should always be a key file", f)
		}
	}
	// Not a key file without task relevance
	if isKeyFile("internal/random/stuff.go", "") {
		t.Error("random file should not be key without task relevance")
	}
	// Task-relevant
	if !isKeyFile("internal/model/clinic/db.go", "fix clinic issue") {
		t.Error("clinic/db.go should be key when task mentions 'clinic'")
	}
}
