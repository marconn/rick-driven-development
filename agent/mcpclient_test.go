package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newFakeMCP(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestMCPClient_CallTool(t *testing.T) {
	srv := newFakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req mcpRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}

		if req.Method != "tools/call" {
			t.Errorf("expected method tools/call, got %s", req.Method)
		}

		resp := mcpResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
		}
		result, err := json.Marshal(mcpToolResult{
			Content: []mcpContent{{Type: "text", Text: `{"workflows":[]}`}},
		})
		if err != nil {
			t.Fatalf("marshal tool result: %v", err)
		}
		resp.Result = result

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	})

	client := NewMCPClient(srv.URL)
	raw, err := client.CallTool(context.Background(), "rick_list_workflows", map[string]any{})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}

	var result struct {
		Workflows []any `json:"workflows"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(result.Workflows) != 0 {
		t.Errorf("expected empty workflows, got %d", len(result.Workflows))
	}
}

func TestMCPClient_CallTool_RPCError(t *testing.T) {
	srv := newFakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		var req mcpRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck

		resp := mcpResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &mcpError{Code: -32602, Message: "unknown tool"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	})

	client := NewMCPClient(srv.URL)
	_, err := client.CallTool(context.Background(), "nonexistent", nil)
	if err == nil {
		t.Fatal("expected error for RPC error response")
	}
	if !strings.Contains(err.Error(), "rpc error") {
		t.Errorf("expected error to contain 'rpc error', got: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("expected error to contain 'unknown tool', got: %s", err.Error())
	}
}

func TestMCPClient_CallTool_ToolError(t *testing.T) {
	srv := newFakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		var req mcpRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck

		resp := mcpResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
		}
		result, _ := json.Marshal(mcpToolResult{
			Content: []mcpContent{{Type: "text", Text: "workflow not found"}},
			IsError: true,
		})
		resp.Result = result

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	})

	client := NewMCPClient(srv.URL)
	_, err := client.CallTool(context.Background(), "rick_workflow_status", map[string]any{"workflow_id": "nonexistent"})
	if err == nil {
		t.Fatal("expected error for tool error")
	}
	if !strings.Contains(err.Error(), "tool error") {
		t.Errorf("expected error to contain 'tool error', got: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "workflow not found") {
		t.Errorf("expected error to contain 'workflow not found', got: %s", err.Error())
	}
}

func TestMCPClient_CallTool_Non200Status(t *testing.T) {
	codes := []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusUnauthorized}
	for _, code := range codes {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := newFakeMCP(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			})
			client := NewMCPClient(srv.URL)
			_, err := client.CallTool(context.Background(), "rick_list_workflows", nil)
			if err == nil {
				t.Fatalf("expected error for HTTP %d", code)
			}
			if !strings.Contains(err.Error(), "unexpected status") {
				t.Errorf("expected 'unexpected status' in error, got: %s", err.Error())
			}
		})
	}
}

func TestMCPClient_CallTool_ConnectionRefused(t *testing.T) {
	client := NewMCPClient("http://127.0.0.1:1/mcp")
	_, err := client.CallTool(context.Background(), "rick_list_workflows", nil)
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
	if !strings.Contains(err.Error(), "http request") {
		t.Errorf("expected 'http request' in error, got: %s", err.Error())
	}
}

func TestMCPClient_CallTool_MalformedJSON(t *testing.T) {
	srv := newFakeMCP(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html>502 Bad Gateway</html>`)) //nolint:errcheck
	})
	client := NewMCPClient(srv.URL)
	_, err := client.CallTool(context.Background(), "rick_list_workflows", nil)
	if err == nil {
		t.Fatal("expected error for malformed JSON response")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("expected 'decode response' in error, got: %s", err.Error())
	}
}

func TestMCPClient_CallTool_EmptyContentArray(t *testing.T) {
	srv := newFakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		var req mcpRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck

		resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}
		result, _ := json.Marshal(mcpToolResult{Content: []mcpContent{}})
		resp.Result = result
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	})
	client := NewMCPClient(srv.URL)
	_, err := client.CallTool(context.Background(), "rick_list_workflows", nil)
	if err == nil {
		t.Fatal("expected error for empty content array")
	}
	if !strings.Contains(err.Error(), "empty tool result") {
		t.Errorf("expected 'empty tool result' in error, got: %s", err.Error())
	}
}

func TestMCPClient_CallTool_ToolErrorEmptyContent(t *testing.T) {
	srv := newFakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		var req mcpRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck

		resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}
		result, _ := json.Marshal(mcpToolResult{Content: []mcpContent{}, IsError: true})
		resp.Result = result
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	})
	client := NewMCPClient(srv.URL)
	_, err := client.CallTool(context.Background(), "rick_list_workflows", nil)
	if err == nil {
		t.Fatal("expected error for tool error with empty content")
	}
	if !strings.Contains(err.Error(), "tool returned error") {
		t.Errorf("expected 'tool returned error' in error, got: %s", err.Error())
	}
}

func TestMCPClient_CallTool_ContextCancelled(t *testing.T) {
	srv := newFakeMCP(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`)) //nolint:errcheck
	})
	client := NewMCPClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling
	_, err := client.CallTool(ctx, "rick_list_workflows", nil)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestMCPClient_Ping(t *testing.T) {
	srv := newFakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"server":{"name":"rick"}}`)) //nolint:errcheck
			return
		}
	})

	t.Run("reachable", func(t *testing.T) {
		client := NewMCPClient(srv.URL)
		if !client.Ping(context.Background()) {
			t.Error("expected ping to succeed")
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		client := NewMCPClient("http://127.0.0.1:1/mcp")
		if client.Ping(context.Background()) {
			t.Error("expected ping to fail for unreachable server")
		}
	})

	t.Run("non-200", func(t *testing.T) {
		srv503 := newFakeMCP(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		client := NewMCPClient(srv503.URL)
		if client.Ping(context.Background()) {
			t.Error("expected ping to fail for 503 response")
		}
	})
}
