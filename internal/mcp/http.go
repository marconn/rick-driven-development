package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// ServeHTTP starts an HTTP server that handles MCP JSON-RPC requests.
// POST /mcp — accepts JSON-RPC request body, returns JSON-RPC response.
// GET  /mcp — returns server info and tool list for health checks.
// The server shuts down gracefully when ctx is cancelled.
func (s *Server) ServeHTTP(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: withCORS(s.httpMux()),
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

// httpMux builds the request router shared by the live server and tests, so the
// two cannot drift. The "/" catch-all serves a JSON 404 for everything outside
// /mcp — see handleHTTPNotFound.
func (s *Server) httpMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", s.handleHTTPPost)
	mux.HandleFunc("GET /mcp", s.handleHTTPGet)
	mux.HandleFunc("/", handleHTTPNotFound)
	return mux
}

func (s *Server) handleHTTPPost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer func() { _ = r.Body.Close() }()

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

func (s *Server) handleHTTPGet(w http.ResponseWriter, r *http.Request) {
	// MCP Streamable HTTP clients open GET /mcp with Accept: text/event-stream
	// to receive server-initiated messages. Rick has no server→client stream, so
	// the spec calls for 405 here. Returning the health JSON instead makes the
	// client treat the ended response as a dropped stream and reconnect-loop;
	// during each drop window it pulls rick's tools from the model's callable set
	// even though /mcp still shows the server connected.
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "this endpoint has no SSE stream; use POST for JSON-RPC")
		return
	}

	defs := make([]ToolDefinition, 0, len(s.tools))
	for _, t := range s.tools {
		defs = append(defs, t.Definition)
	}

	info := struct {
		Server         entityInfo       `json:"server"`
		DefaultBackend string           `json:"default_backend"`
		Tools          []ToolDefinition `json:"tools"`
	}{
		Server:         entityInfo{Name: "rick", Version: "2.0.0"},
		DefaultBackend: s.defaultBackendName(),
		Tools:          defs,
	}

	writeJSONResponse(w, info)
}

// defaultBackendName reports the backend that rick_consult / rick_run use when
// the caller omits the backend param — letting MCP clients discover the
// default they'd otherwise get blind. Mirrors resolveBackend's empty-name path.
func (s *Server) defaultBackendName() string {
	if s.deps.Backend != nil {
		return s.deps.Backend.Name()
	}
	if s.deps.BackendName != "" {
		return s.deps.BackendName
	}
	return "claude"
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

// writeJSONError writes a JSON body with an explicit status. Callers use it for
// non-2xx replies so clients (notably MCP OAuth probes) get a parseable body
// instead of net/http's plain-text default.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleHTTPNotFound answers any unmatched path with a JSON 404. MCP clients
// probe OAuth discovery endpoints (/.well-known/oauth-*) on connect; net/http's
// default plain-text 404 crashes the client's OAuth error parser, which
// JSON-decodes the body. That failed auth completion drops rick's tools from the
// model's callable set even though the MCP connection is otherwise healthy. A
// JSON body lets the client parse the 404 and fall back to no-auth.
func handleHTTPNotFound(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusNotFound, "not found")
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
