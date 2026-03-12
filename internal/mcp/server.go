package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// Protocol constants.
const (
	jsonRPCVersion = "2.0"
	protocolVersion = "2024-11-05"

	methodInitialize       = "initialize"
	methodInitialized      = "notifications/initialized"
	methodToolsList        = "tools/list"
	methodToolsCall        = "tools/call"
	methodPing             = "ping"
)

// jsonRPCRequest is a JSON-RPC 2.0 request or notification.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // nil for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonRPCResponse is a JSON-RPC 2.0 response.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// jsonRPCError is a JSON-RPC 2.0 error object.
type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// initializeParams carries the client's initialize request.
type initializeParams struct {
	ProtocolVersion string     `json:"protocolVersion"`
	ClientInfo      entityInfo `json:"clientInfo"`
}

type entityInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// initializeResult is the server's response to initialize.
type initializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	ServerInfo      entityInfo   `json:"serverInfo"`
	Capabilities    capabilities `json:"capabilities"`
}

type capabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

type toolsCapability struct{}

// toolsListResult is the result of tools/list.
type toolsListResult struct {
	Tools []ToolDefinition `json:"tools"`
}

// toolsCallParams carries the arguments for tools/call.
type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// toolsCallResult is the result of tools/call.
type toolsCallResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Server is an MCP server that exposes Rick's event-driven capabilities.
type Server struct {
	deps   Deps
	tools  map[string]Tool
	logger *slog.Logger
	jobs   *JobManager

	mu          sync.Mutex
	initialized bool
}

// NewServer creates a new MCP server with the given dependencies.
func NewServer(deps Deps, logger *slog.Logger) *Server {
	s := &Server{
		deps:   deps,
		tools:  make(map[string]Tool),
		logger: logger,
		jobs:   NewJobManager(),
	}
	s.registerBuiltinTools()
	return s
}

// Close shuts down server resources (job manager reaper goroutine).
func (s *Server) Close() {
	if s.jobs != nil {
		s.jobs.Stop()
	}
}

// Serve reads JSON-RPC messages from r and writes responses to w.
// It blocks until r is closed or ctx is cancelled.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	// MCP uses newline-delimited JSON
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	var writeMu sync.Mutex
	write := func(resp jsonRPCResponse) {
		data, err := json.Marshal(resp)
		if err != nil {
			s.logger.Error("marshal response", slog.String("error", err.Error()))
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, _ = fmt.Fprintf(w, "%s\n", data)
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			write(jsonRPCResponse{
				JSONRPC: jsonRPCVersion,
				Error:   &jsonRPCError{Code: -32700, Message: "parse error"},
			})
			continue
		}

		resp := s.handleRequest(ctx, req)
		if resp == nil {
			continue // notification, no response needed
		}
		write(*resp)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("mcp: scanner error: %w", err)
	}
	return nil
}

func (s *Server) handleRequest(ctx context.Context, req jsonRPCRequest) *jsonRPCResponse {
	switch req.Method {
	case methodInitialize:
		return s.handleInitialize(req)

	case methodInitialized:
		// Notification — no response
		s.mu.Lock()
		s.initialized = true
		s.mu.Unlock()
		return nil

	case methodPing:
		return &jsonRPCResponse{
			JSONRPC: jsonRPCVersion,
			ID:      req.ID,
			Result:  map[string]any{},
		}

	case methodToolsList:
		return s.handleToolsList(req)

	case methodToolsCall:
		return s.handleToolsCall(ctx, req)

	default:
		return &jsonRPCResponse{
			JSONRPC: jsonRPCVersion,
			ID:      req.ID,
			Error:   &jsonRPCError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)},
		}
	}
}

func (s *Server) handleInitialize(req jsonRPCRequest) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: jsonRPCVersion,
		ID:      req.ID,
		Result: initializeResult{
			ProtocolVersion: protocolVersion,
			ServerInfo:      entityInfo{Name: "rick", Version: "2.0.0"},
			Capabilities: capabilities{
				Tools: &toolsCapability{},
			},
		},
	}
}

func (s *Server) handleToolsList(req jsonRPCRequest) *jsonRPCResponse {
	defs := make([]ToolDefinition, 0, len(s.tools))
	for _, t := range s.tools {
		defs = append(defs, t.Definition)
	}
	return &jsonRPCResponse{
		JSONRPC: jsonRPCVersion,
		ID:      req.ID,
		Result:  toolsListResult{Tools: defs},
	}
}

func (s *Server) handleToolsCall(ctx context.Context, req jsonRPCRequest) *jsonRPCResponse {
	var params toolsCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &jsonRPCResponse{
			JSONRPC: jsonRPCVersion,
			ID:      req.ID,
			Error:   &jsonRPCError{Code: -32602, Message: "invalid params"},
		}
	}

	tool, ok := s.tools[params.Name]
	if !ok {
		return &jsonRPCResponse{
			JSONRPC: jsonRPCVersion,
			ID:      req.ID,
			Error:   &jsonRPCError{Code: -32602, Message: fmt.Sprintf("unknown tool: %s", params.Name)},
		}
	}

	result, err := tool.Handler(ctx, params.Arguments)
	if err != nil {
		return &jsonRPCResponse{
			JSONRPC: jsonRPCVersion,
			ID:      req.ID,
			Result: toolsCallResult{
				Content: []contentBlock{{Type: "text", Text: err.Error()}},
				IsError: true,
			},
		}
	}

	text, marshalErr := json.MarshalIndent(result, "", "  ")
	if marshalErr != nil {
		return &jsonRPCResponse{
			JSONRPC: jsonRPCVersion,
			ID:      req.ID,
			Result: toolsCallResult{
				Content: []contentBlock{{Type: "text", Text: marshalErr.Error()}},
				IsError: true,
			},
		}
	}

	return &jsonRPCResponse{
		JSONRPC: jsonRPCVersion,
		ID:      req.ID,
		Result: toolsCallResult{
			Content: []contentBlock{{Type: "text", Text: string(text)}},
		},
	}
}
