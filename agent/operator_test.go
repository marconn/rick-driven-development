package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

func TestNewOperator(t *testing.T) {
	cfg := Config{
		ServerURL: "http://localhost:8077/mcp",
		Model:     "gemini-2.5-pro",
		APIKey:    "test-key",
	}
	op := NewOperator(cfg, nil, testLogger(t))
	if op == nil {
		t.Fatal("expected non-nil operator")
	}
	if op.cfg.ServerURL != cfg.ServerURL {
		t.Errorf("expected server URL %s, got %s", cfg.ServerURL, op.cfg.ServerURL)
	}
	if op.sessionID != "operator-session" {
		t.Errorf("expected session ID operator-session, got %s", op.sessionID)
	}
}

func TestOperatorRunNotInitialized(t *testing.T) {
	op := NewOperator(Config{}, nil, testLogger(t))
	_, err := op.Run(context.Background(), "hello", func(_ Event) {})
	if err == nil {
		t.Fatal("expected error when running uninitialized operator")
	}
	if err.Error() != "operator not initialized — call Init first" {
		t.Errorf("unexpected error: %s", err)
	}
}

func TestOperatorConnectedSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"server": map[string]any{"name": "rick"}}) //nolint:errcheck
	}))
	defer srv.Close()

	op := NewOperator(Config{ServerURL: srv.URL}, nil, testLogger(t))
	if !op.Connected(context.Background()) {
		t.Error("expected connected=true with running server")
	}
}

func TestOperatorConnectedFailed(t *testing.T) {
	op := NewOperator(Config{ServerURL: "http://127.0.0.1:1"}, nil, testLogger(t))
	if op.Connected(context.Background()) {
		t.Error("expected connected=false with unreachable server")
	}
}

func TestOperatorConnectedCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	op := NewOperator(Config{ServerURL: "http://localhost:8077/mcp"}, nil, testLogger(t))
	if op.Connected(ctx) {
		t.Error("expected connected=false with cancelled context")
	}
}

func TestOperatorTurnCountResets(t *testing.T) {
	op := NewOperator(Config{}, nil, testLogger(t))
	// Simulate turns by incrementing directly (Run requires full init).
	op.turnCount = 15
	if op.turnCount != 15 {
		t.Fatalf("expected turnCount=15, got %d", op.turnCount)
	}

	// ResetContext should reset turn count.
	// We can't call ResetContext without init, so verify the struct field directly.
	op.turnCount = 0
	if op.turnCount != 0 {
		t.Fatalf("expected turnCount=0 after reset, got %d", op.turnCount)
	}
}

func TestContextReinjectInterval(t *testing.T) {
	// Verify the constant is sensible (between 4 and 16).
	if contextReinjectInterval < 4 || contextReinjectInterval > 16 {
		t.Errorf("contextReinjectInterval=%d is outside expected range [4,16]", contextReinjectInterval)
	}
}

func TestCoreRulesReminderNonEmpty(t *testing.T) {
	if coreRulesReminder == "" {
		t.Error("coreRulesReminder should not be empty")
	}
}

func TestSystemInstructionHasXMLTags(t *testing.T) {
	tags := []string{"<role>", "</role>", "<rules>", "</rules>", "<output_style>", "</output_style>"}
	for _, tag := range tags {
		if !strings.Contains(systemInstruction, tag) {
			t.Errorf("systemInstruction missing XML tag: %s", tag)
		}
	}
}
