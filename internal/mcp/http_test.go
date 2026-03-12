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

// httpHandler returns an http.Handler for the MCP server under test.
func httpHandler(s *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", s.handleHTTPPost)
	mux.HandleFunc("GET /mcp", s.handleHTTPGet)
	return withCORS(mux)
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
		Name:      "rick_workflow_status",
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

	var status workflowStatusResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &status); err != nil {
		t.Fatal(err)
	}
	if status.Status != "requested" {
		t.Errorf("expected status requested, got %s", status.Status)
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
		Server entityInfo       `json:"server"`
		Tools  []ToolDefinition `json:"tools"`
	}
	if err := json.NewDecoder(w.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Server.Name != "rick" {
		t.Errorf("expected server name rick, got %s", info.Server.Name)
	}
	if len(info.Tools) < 11 {
		t.Errorf("expected at least 11 tools, got %d", len(info.Tools))
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
