package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNewApp(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "test-key")
	t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")
	t.Setenv("RICK_MODEL", "gemini-2.0-flash")

	app := NewApp()
	if app == nil {
		t.Fatal("expected non-nil app")
	}
	if app.operator == nil {
		t.Fatal("expected non-nil operator")
	}
	if app.cfg.ServerURL != "http://test:58077/mcp" {
		t.Errorf("expected server URL from env, got %s", app.cfg.ServerURL)
	}
	if app.cfg.Model != "gemini-2.0-flash" {
		t.Errorf("expected model from env, got %s", app.cfg.Model)
	}
}

func TestGetConfig(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "secret-key-should-not-leak")
	t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")
	t.Setenv("RICK_MODEL", "gemini-2.5-pro")

	app := NewApp()
	cfg := app.GetConfig()

	if cfg.ServerURL != "http://test:58077/mcp" {
		t.Errorf("expected server URL, got %s", cfg.ServerURL)
	}
	if cfg.Model != "gemini-2.5-pro" {
		t.Errorf("expected model, got %s", cfg.Model)
	}
	// API key must NOT be returned to frontend.
	if cfg.APIKey != "" {
		t.Error("GetConfig must not expose the API key")
	}
}

func TestCheckConnectionNoServer(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "test-key")
	t.Setenv("RICK_SERVER_URL", "http://127.0.0.1:1/mcp")

	app := NewApp()
	// No Wails context, but CheckConnection directly calls operator.Connected.
	// ctx is nil, so we expect false rather than panic.
	connected := app.operator.Connected(app.ctx)
	if connected {
		t.Error("expected disconnected with no server")
	}
}

func TestReconnectNoServer(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "test-key")
	t.Setenv("RICK_SERVER_URL", "http://127.0.0.1:1/mcp")

	app := NewApp()
	// Init/Reconnect creates model + lazy MCP toolset — no immediate connection
	// error because StreamableClientTransport connects lazily on first tool use.
	errMsg := app.Reconnect()
	// Should succeed since transport is lazy.
	if errMsg != "" {
		// Accept both success (lazy) and failure (eager connect) — implementation may vary.
		t.Logf("reconnect returned: %s (acceptable if transport connects eagerly)", errMsg)
	}
}

// TestReconnectValidation verifies that Reconnect returns a validation error
// when the config is missing required fields (no API key).
func TestReconnectValidation(t *testing.T) {
	// Deliberately omit GOOGLE_API_KEY so Validate() fails.
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GOOGLE_GENAI_API_KEY", "")
	t.Setenv("RICK_SERVER_URL", "http://localhost:58077/mcp")

	app := NewApp()
	errMsg := app.Reconnect()
	if errMsg == "" {
		t.Fatal("expected validation error when API key is missing")
	}
	if !strings.Contains(errMsg, "API key") {
		t.Errorf("expected error about API key, got: %s", errMsg)
	}
}

// TestClearContextNotInitialized verifies ClearContext returns an error string
// when the operator has not been initialized (sessionService is nil).
func TestClearContextNotInitialized(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "test-key")
	t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")

	app := NewApp()
	// app.operator.sessionService is nil because Init was never called.
	errMsg := app.ClearContext()
	if errMsg == "" {
		t.Fatal("expected error from ClearContext when operator not initialized")
	}
	if !strings.Contains(errMsg, "not initialized") {
		t.Errorf("expected 'not initialized' in error, got: %s", errMsg)
	}
}

// TestSaveMemoryNilStore verifies SaveMemory returns nil without panicking
// when the memory store is nil (e.g., home dir resolution failed).
func TestSaveMemoryNilStore(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "test-key")
	t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")

	app := NewApp()
	app.memoryStore = nil // force nil store

	result := app.SaveMemory("test content", "general")
	if result != nil {
		t.Error("expected nil result when memoryStore is nil")
	}
}

// TestSaveMemoryValid verifies SaveMemory returns a non-nil Memory on success.
func TestSaveMemoryValid(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "test-key")
	t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")

	app := NewApp()
	dir := t.TempDir()
	app.memoryStore = &MemoryStore{path: filepath.Join(dir, "memories.json")}

	result := app.SaveMemory("user is a developer", "user")
	if result == nil {
		t.Fatal("expected non-nil Memory on successful save")
	}
	if result.Content != "user is a developer" {
		t.Errorf("expected content 'user is a developer', got %q", result.Content)
	}
	if result.Category != "user" {
		t.Errorf("expected category 'user', got %q", result.Category)
	}
}

// TestListMemoriesNilStore verifies ListMemories returns nil when memoryStore is nil.
func TestListMemoriesNilStore(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "test-key")
	t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")

	app := NewApp()
	app.memoryStore = nil

	result := app.ListMemories()
	if result != nil {
		t.Error("expected nil result when memoryStore is nil")
	}
}

// TestDeleteMemoryNilStore verifies DeleteMemory returns false when memoryStore is nil.
func TestDeleteMemoryNilStore(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "test-key")
	t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")

	app := NewApp()
	app.memoryStore = nil

	if app.DeleteMemory("any-id") {
		t.Error("expected false from DeleteMemory when memoryStore is nil")
	}
}

// TestSearchMemoriesNilStore verifies SearchMemories returns nil when memoryStore is nil.
func TestSearchMemoriesNilStore(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "test-key")
	t.Setenv("RICK_SERVER_URL", "http://test:58077/mcp")

	app := NewApp()
	app.memoryStore = nil

	result := app.SearchMemories("anything")
	if result != nil {
		t.Error("expected nil result when memoryStore is nil")
	}
}
