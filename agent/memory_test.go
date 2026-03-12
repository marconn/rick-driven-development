package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testMemoryStore(t *testing.T) *MemoryStore {
	t.Helper()
	dir := t.TempDir()
	ms := &MemoryStore{path: filepath.Join(dir, "memories.json")}
	return ms
}

func TestMemoryAddAndList(t *testing.T) {
	ms := testMemoryStore(t)

	m, err := ms.Add("user prefers dark mode", "preference")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if m.Category != "preference" {
		t.Errorf("expected category 'preference', got %q", m.Category)
	}
	if m.Content != "user prefers dark mode" {
		t.Errorf("expected content 'user prefers dark mode', got %q", m.Content)
	}

	list := ms.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(list))
	}
	if list[0].ID != m.ID {
		t.Errorf("expected ID %s, got %s", m.ID, list[0].ID)
	}
}

func TestMemoryDefaultCategory(t *testing.T) {
	ms := testMemoryStore(t)

	m, err := ms.Add("something general", "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Category != "general" {
		t.Errorf("expected default category 'general', got %q", m.Category)
	}
}

func TestMemoryDelete(t *testing.T) {
	ms := testMemoryStore(t)

	m, _ := ms.Add("to be deleted", "test")
	if !ms.Delete(m.ID) {
		t.Error("expected Delete to return true")
	}
	if len(ms.List()) != 0 {
		t.Error("expected empty list after delete")
	}
}

func TestMemoryDeleteByPrefix(t *testing.T) {
	ms := testMemoryStore(t)

	m, _ := ms.Add("prefix test", "test")
	// Use first 4 chars as prefix.
	if !ms.Delete(m.ID[:4]) {
		t.Error("expected Delete by prefix to return true")
	}
	if len(ms.List()) != 0 {
		t.Error("expected empty list after prefix delete")
	}
}

func TestMemoryDeleteNotFound(t *testing.T) {
	ms := testMemoryStore(t)
	if ms.Delete("nonexistent") {
		t.Error("expected Delete to return false for nonexistent ID")
	}
}

func TestMemorySearch(t *testing.T) {
	ms := testMemoryStore(t)

	ms.Add("user is a backend engineer", "user")       //nolint:errcheck
	ms.Add("uses vim for editing", "environment")       //nolint:errcheck
	ms.Add("prefers concise responses", "preference")   //nolint:errcheck

	results := ms.Search("engineer")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !strings.Contains(results[0].Content, "engineer") {
		t.Errorf("expected result to contain 'engineer', got %q", results[0].Content)
	}

	// Search by category.
	results = ms.Search("environment")
	if len(results) != 1 {
		t.Fatalf("expected 1 result for category search, got %d", len(results))
	}
}

func TestMemoryFormatForPrompt(t *testing.T) {
	ms := testMemoryStore(t)

	// Empty store returns empty string.
	if ms.FormatForPrompt() != "" {
		t.Error("expected empty prompt for empty store")
	}

	ms.Add("user is Marco", "user")             //nolint:errcheck
	ms.Add("uses Go and Svelte", "environment") //nolint:errcheck

	prompt := ms.FormatForPrompt()
	if !strings.Contains(prompt, "Operator Memory") {
		t.Error("expected prompt to contain header")
	}
	if !strings.Contains(prompt, "user is Marco") {
		t.Error("expected prompt to contain user memory")
	}
	if !strings.Contains(prompt, "uses Go and Svelte") {
		t.Error("expected prompt to contain environment memory")
	}
}

func TestMemoryPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memories.json")

	// Write.
	ms1 := &MemoryStore{path: path}
	ms1.Add("persistent memory", "test") //nolint:errcheck

	// Reload.
	ms2 := &MemoryStore{path: path}
	ms2.load()
	list := ms2.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 memory after reload, got %d", len(list))
	}
	if list[0].Content != "persistent memory" {
		t.Errorf("expected 'persistent memory', got %q", list[0].Content)
	}
}

func TestMemoryVersion(t *testing.T) {
	ms := testMemoryStore(t)

	v0 := ms.Version()
	ms.Add("first", "test") //nolint:errcheck
	v1 := ms.Version()
	if v1 <= v0 {
		t.Error("expected version to increment after Add")
	}

	m, _ := ms.Add("second", "test")
	v2 := ms.Version()
	if v2 <= v1 {
		t.Error("expected version to increment after second Add")
	}

	ms.Delete(m.ID)
	v3 := ms.Version()
	if v3 <= v2 {
		t.Error("expected version to increment after Delete")
	}
}

func TestMemoryVersionNonzeroOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memories.json")

	// Seed the store.
	ms1 := &MemoryStore{path: path}
	ms1.Add("pre-existing", "test") //nolint:errcheck

	// Reload into a new store — version must be > 0 for injection to trigger.
	ms2 := &MemoryStore{path: path}
	ms2.load()
	// Simulate what NewMemoryStore does.
	if len(ms2.memories) > 0 {
		ms2.version.Store(1)
	}
	if ms2.Version() == 0 {
		t.Error("expected non-zero version after loading pre-existing memories")
	}
}

func TestMemoryStoreFilePermissions(t *testing.T) {
	ms := testMemoryStore(t)
	ms.Add("test", "test") //nolint:errcheck

	info, err := os.Stat(ms.path)
	if err != nil {
		t.Fatal(err)
	}
	perm := info.Mode().Perm()
	if perm != 0o644 {
		t.Errorf("expected file permissions 0644, got %o", perm)
	}
}

// --- NewMemoryStore constructor tests ---

// TestNewMemoryStoreValid verifies NewMemoryStore succeeds when HOME is writable.
func TestNewMemoryStoreValid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	ms, err := NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	if ms == nil {
		t.Fatal("expected non-nil MemoryStore")
	}
	expectedPath := filepath.Join(dir, ".config", "rick", "memories.json")
	if ms.path != expectedPath {
		t.Errorf("expected path %s, got %s", expectedPath, ms.path)
	}
}

// TestNewMemoryStoreLoadsExisting verifies pre-existing memories are loaded on startup.
func TestNewMemoryStoreLoadsExisting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	rickDir := filepath.Join(dir, ".config", "rick")
	if err := os.MkdirAll(rickDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data := `[{"id":"abc12345","content":"pre-existing","category":"test","created_at":"2026-01-01T00:00:00Z"}]`
	if err := os.WriteFile(filepath.Join(rickDir, "memories.json"), []byte(data), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	ms, err := NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	list := ms.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 pre-existing memory, got %d", len(list))
	}
	if list[0].Content != "pre-existing" {
		t.Errorf("expected content 'pre-existing', got %q", list[0].Content)
	}
}

// TestNewMemoryStoreVersionOnLoad verifies version is set to 1 when pre-existing
// memories are loaded, so the operator injects them on the first message.
func TestNewMemoryStoreVersionOnLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	rickDir := filepath.Join(dir, ".config", "rick")
	if err := os.MkdirAll(rickDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data := `[{"id":"abc12345","content":"loaded memory","category":"general","created_at":"2026-01-01T00:00:00Z"}]`
	if err := os.WriteFile(filepath.Join(rickDir, "memories.json"), []byte(data), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	ms, err := NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	if ms.Version() != 1 {
		t.Errorf("expected version 1 after loading pre-existing memories, got %d", ms.Version())
	}
}

// TestNewMemoryStoreCorruptJSON verifies graceful degradation when memories.json
// contains invalid JSON — store must be created with an empty memory list (no panic).
func TestNewMemoryStoreCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	rickDir := filepath.Join(dir, ".config", "rick")
	if err := os.MkdirAll(rickDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rickDir, "memories.json"), []byte("this is not json {{{"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Corrupt JSON must NOT cause an error — store is created empty.
	ms, err := NewMemoryStore()
	if err != nil {
		t.Fatalf("NewMemoryStore with corrupt JSON should succeed: %v", err)
	}
	list := ms.List()
	if len(list) != 0 {
		t.Errorf("expected empty memory list on corrupt JSON, got %d entries", len(list))
	}
	// Version stays 0 since no memories were loaded.
	if ms.Version() != 0 {
		t.Errorf("expected version 0 after corrupt JSON load, got %d", ms.Version())
	}
}

// --- Add rollback on save failure ---

// TestMemoryAddRollbackOnSaveFailure verifies that when save fails (read-only dir),
// the in-memory state is rolled back so memories count is unchanged.
func TestMemoryAddRollbackOnSaveFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, file permission tests are unreliable")
	}

	dir := t.TempDir()
	// Make the directory read-only so WriteFile will fail.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) }) //nolint:errcheck

	ms := &MemoryStore{path: filepath.Join(dir, "memories.json")}
	_, err := ms.Add("should fail", "test")
	if err == nil {
		t.Fatal("expected error when saving to read-only directory")
	}

	// In-memory state must be rolled back.
	if len(ms.memories) != 0 {
		t.Errorf("expected rollback: got %d memories, want 0", len(ms.memories))
	}
}

// --- Delete save failure behavior ---

// TestMemoryDeleteReturnsTrueEvenOnSaveFailure documents the known behavior:
// Delete returns true (found+removed in-memory) even when the subsequent disk
// save fails. This is an acknowledged inconsistency — in-memory is authoritative
// until the next process restart reloads from disk.
func TestMemoryDeleteReturnsTrueEvenOnSaveFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, file permission tests are unreliable")
	}

	dir := t.TempDir()
	ms := &MemoryStore{path: filepath.Join(dir, "memories.json")}

	// Add a memory while the directory is writable.
	m, err := ms.Add("to delete", "test")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Make directory read-only so the save inside Delete fails.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) }) //nolint:errcheck

	// Delete still returns true: item was found and removed from in-memory slice.
	if !ms.Delete(m.ID) {
		t.Error("expected Delete to return true even when save fails")
	}
	if len(ms.List()) != 0 {
		t.Errorf("expected 0 memories after delete, got %d", len(ms.List()))
	}
}

// --- Search empty query ---

// TestMemorySearchEmptyQuery verifies that an empty query string returns all
// memories because every string contains "" as a substring.
func TestMemorySearchEmptyQuery(t *testing.T) {
	ms := testMemoryStore(t)

	ms.Add("backend engineer", "user")        //nolint:errcheck
	ms.Add("prefers vim", "environment")      //nolint:errcheck
	ms.Add("concise responses", "preference") //nolint:errcheck

	results := ms.Search("")
	if len(results) != 3 {
		t.Errorf("expected all 3 memories with empty query, got %d", len(results))
	}
}

// TestMemorySearchCaseInsensitive verifies search is case-insensitive.
func TestMemorySearchCaseInsensitive(t *testing.T) {
	ms := testMemoryStore(t)
	ms.Add("User is a Backend Engineer", "user") //nolint:errcheck

	results := ms.Search("backend")
	if len(results) != 1 {
		t.Fatalf("expected 1 result for lowercase search, got %d", len(results))
	}

	results = ms.Search("BACKEND")
	if len(results) != 1 {
		t.Fatalf("expected 1 result for uppercase search, got %d", len(results))
	}
}

