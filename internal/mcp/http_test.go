package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// httpHandler returns an http.Handler for the MCP server under test. It reuses
// the production router (httpMux) so test coverage tracks the real routing.
func httpHandler(s *Server) http.Handler {
	return withCORS(s.httpMux())
}

func postMCP(t *testing.T, handler http.Handler, req jsonRPCRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func parseHTTPResponse(t *testing.T, w *httptest.ResponseRecorder) jsonRPCResponse {
	t.Helper()
	var resp jsonRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, w.Body.String())
	}
	return resp
}

func TestHTTPToolsList(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	h := httpHandler(s)

	w := postMCP(t, h, jsonRPCRequest{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage("1"),
		Method:  methodToolsList,
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	resp := parseHTTPResponse(t, w)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	data, _ := json.Marshal(resp.Result)
	var result toolsListResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Tools) < 11 {
		t.Errorf("expected at least 11 tools, got %d", len(result.Tools))
	}
}

func TestHTTPToolCall(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	h := httpHandler(s)

	// Seed a workflow
	aggregateID := "http-wf-1"
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "workspace-dev",
	})).WithAggregate(aggregateID, 1).WithCorrelation(aggregateID).WithSource("test")
	if err := deps.Store.Append(context.Background(), aggregateID, 0, []event.Envelope{reqEvt}); err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(toolsCallParams{
		Name:      "rick_workflow_inspect",
		Arguments: json.RawMessage(fmt.Sprintf(`{"workflow_id":"%s"}`, aggregateID)),
	})
	w := postMCP(t, h, jsonRPCRequest{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage("2"),
		Method:  methodToolsCall,
		Params:  params,
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	resp := parseHTTPResponse(t, w)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	data, _ := json.Marshal(resp.Result)
	var result toolsCallResult
	_ = json.Unmarshal(data, &result)
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].Text)
	}

	var inspect struct {
		Status workflowStatusResult `json:"status"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &inspect); err != nil {
		t.Fatal(err)
	}
	if inspect.Status.Status != "requested" {
		t.Errorf("expected status requested, got %s", inspect.Status.Status)
	}
}

func TestHTTPAutoInitialize(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	h := httpHandler(s)

	// HTTP clients skip initialize handshake — tool calls should work immediately.
	params, _ := json.Marshal(toolsCallParams{
		Name: "rick_list_workflows",
	})
	w := postMCP(t, h, jsonRPCRequest{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage("1"),
		Method:  methodToolsCall,
		Params:  params,
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	resp := parseHTTPResponse(t, w)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	// Verify initialized flag was set.
	s.mu.Lock()
	initialized := s.initialized
	s.mu.Unlock()
	if !initialized {
		t.Error("expected server to be auto-initialized for HTTP")
	}
}

func TestHTTPGetHealthCheck(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	h := httpHandler(s)

	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", ct)
	}

	var info struct {
		Server         entityInfo       `json:"server"`
		DefaultBackend string           `json:"default_backend"`
		Tools          []ToolDefinition `json:"tools"`
	}
	if err := json.NewDecoder(w.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Server.Name != "rick" {
		t.Errorf("expected server name rick, got %s", info.Server.Name)
	}
	if info.DefaultBackend != "claude" {
		t.Errorf("expected default_backend 'claude' with no backend injected, got %q", info.DefaultBackend)
	}
	if len(info.Tools) < 11 {
		t.Errorf("expected at least 11 tools, got %d", len(info.Tools))
	}
}

// Health payload must report the configured default backend so MCP clients
// can discover what they get when they omit the backend param.
func TestHTTPGetHealthCheck_ReportsInjectedBackend(t *testing.T) {
	be := &stubBackend{name: "gemini"}
	deps, cleanup := testDepsWithBackend(t, be)
	defer cleanup()
	s := NewServer(deps, testLogger())
	h := httpHandler(s)

	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	var info struct {
		DefaultBackend string `json:"default_backend"`
	}
	if err := json.NewDecoder(w.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.DefaultBackend != "gemini" {
		t.Errorf("expected default_backend 'gemini', got %q", info.DefaultBackend)
	}
}

func TestHTTPInitializeNegotiatesProtocolVersion(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	h := httpHandler(s)

	// The operator-facing failure is on the HTTP POST path: a recent Claude Code
	// client initializes over POST /mcp asking for 2025-06-18. The server must
	// echo that version, not the hardcoded 2024-11-05, or the client drops every
	// rick tool. Assert negotiation at the real transport boundary.
	params, _ := json.Marshal(initializeParams{
		ProtocolVersion: "2025-06-18",
		ClientInfo:      entityInfo{Name: "claude-code", Version: "1.0"},
	})
	w := postMCP(t, h, jsonRPCRequest{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage("1"),
		Method:  methodInitialize,
		Params:  params,
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	resp := parseHTTPResponse(t, w)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
	data, _ := json.Marshal(resp.Result)
	var result initializeResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.ProtocolVersion != "2025-06-18" {
		t.Errorf("expected negotiated 2025-06-18, got %q", result.ProtocolVersion)
	}
}

func TestHTTPGetEventStreamReturns405(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	h := httpHandler(s)

	// MCP Streamable HTTP clients open GET /mcp with Accept: text/event-stream
	// to receive server-initiated messages. Rick has no such stream, so it must
	// answer 405 — not a 200 JSON body that the client mistakes for an
	// immediately-closed stream and then reconnect-loops on (dropping rick's
	// tools from the model's callable set during each drop window).
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	r.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET /mcp with text/event-stream, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json body, got %s", ct)
	}
}

func TestHTTPUnknownPathReturnsJSON404(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	h := httpHandler(s)

	// Clients probe OAuth discovery endpoints (/.well-known/oauth-*) on connect.
	// The 404 body must be valid JSON: a plain-text body crashes the client's
	// OAuth error parser, failing auth completion and dropping rick's tools from
	// the model's callable set even though the connection itself is healthy.
	r := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %s", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("404 body must be valid JSON for the client's OAuth parser: %v", err)
	}
}

func TestHTTPInvalidJSON(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	h := httpHandler(s)

	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte("{bad json")))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (JSON-RPC error in body), got %d", w.Code)
	}

	resp := parseHTTPResponse(t, w)
	if resp.Error == nil {
		t.Fatal("expected JSON-RPC parse error")
	}
	if resp.Error.Code != -32700 {
		t.Errorf("expected code -32700, got %d", resp.Error.Code)
	}
}

func TestHTTPNotification(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	h := httpHandler(s)

	body, _ := json.Marshal(jsonRPCRequest{
		JSONRPC: jsonRPCVersion,
		Method:  methodInitialized,
	})
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204 for notification, got %d", w.Code)
	}
}

func TestHTTPCORSPreflight(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	h := httpHandler(s)

	r := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204 for OPTIONS, got %d", w.Code)
	}
	if v := w.Header().Get("Access-Control-Allow-Origin"); v != "*" {
		t.Errorf("expected CORS origin *, got %s", v)
	}
}

func TestHTTPCORSHeaders(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	h := httpHandler(s)

	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if v := w.Header().Get("Access-Control-Allow-Origin"); v != "*" {
		t.Errorf("expected CORS origin *, got %s", v)
	}
	if v := w.Header().Get("Access-Control-Allow-Methods"); v != "GET, POST, OPTIONS" {
		t.Errorf("expected CORS methods, got %s", v)
	}
}

func TestHTTPPing(t *testing.T) {
	deps, cleanup := testDeps(t)
	defer cleanup()
	s := NewServer(deps, testLogger())
	h := httpHandler(s)

	w := postMCP(t, h, jsonRPCRequest{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage("1"),
		Method:  methodPing,
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	resp := parseHTTPResponse(t, w)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
}
