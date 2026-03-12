package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
)

// ServeHTTP starts an HTTP server that handles MCP JSON-RPC requests.
// POST /mcp — accepts JSON-RPC request body, returns JSON-RPC response.
// GET  /mcp — returns server info and tool list for health checks.
// The server shuts down gracefully when ctx is cancelled.
func (s *Server) ServeHTTP(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", s.handleHTTPPost)
	mux.HandleFunc("GET /mcp", s.handleHTTPGet)

	srv := &http.Server{
		Addr:    addr,
		Handler: withCORS(mux),
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}

	// Graceful shutdown on context cancellation.
	go func() {
		<-ctx.Done()
		s.logger.Info("http: shutting down")
		_ = srv.Close()
	}()

	s.logger.Info("http: listening", slog.String("addr", addr))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("mcp: http server: %w", err)
	}
	return nil
}

func (s *Server) handleHTTPPost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONResponse(w, jsonRPCResponse{
			JSONRPC: jsonRPCVersion,
			Error:   &jsonRPCError{Code: -32700, Message: "parse error"},
		})
		return
	}

	// HTTP clients skip the initialize/initialized handshake — auto-mark as
	// initialized so tool calls work immediately.
	s.mu.Lock()
	if !s.initialized {
		s.initialized = true
	}
	s.mu.Unlock()

	resp := s.handleRequest(r.Context(), req)
	if resp == nil {
		// Notification — return 204 No Content.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeJSONResponse(w, *resp)
}

func (s *Server) handleHTTPGet(w http.ResponseWriter, _ *http.Request) {
	defs := make([]ToolDefinition, 0, len(s.tools))
	for _, t := range s.tools {
		defs = append(defs, t.Definition)
	}

	info := struct {
		Server entityInfo       `json:"server"`
		Tools  []ToolDefinition `json:"tools"`
	}{
		Server: entityInfo{Name: "rick", Version: "2.0.0"},
		Tools:  defs,
	}

	writeJSONResponse(w, info)
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

// withCORS wraps a handler with permissive CORS headers for local Wails
// webview requests and development use.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
