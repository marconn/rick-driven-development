package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// MCPClient is a thin JSON-RPC client for calling MCP tools over HTTP.
type MCPClient struct {
	endpoint string
	client   *http.Client
	mu       sync.Mutex
	reqID    int
}

// NewMCPClient creates an MCPClient targeting the given MCP HTTP endpoint.
func NewMCPClient(endpoint string) *MCPClient {
	return &MCPClient{
		endpoint: endpoint,
		client:   &http.Client{},
	}
}

// mcpRequest is a JSON-RPC 2.0 request.
type mcpRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// mcpResponse is a JSON-RPC 2.0 response.
type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpToolResult is the tools/call result envelope.
type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// CallTool invokes an MCP tool by name with the given arguments and returns
// the text content from the first content block.
func (c *MCPClient) CallTool(ctx context.Context, name string, args any) (json.RawMessage, error) {
	c.mu.Lock()
	c.reqID++
	id := c.reqID
	c.mu.Unlock()

	argsJSON, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: marshal args: %w", err)
	}

	req := mcpRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      name,
			"arguments": json.RawMessage(argsJSON),
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mcpclient: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcpclient: unexpected status %d", resp.StatusCode)
	}

	var rpcResp mcpResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("mcpclient: decode response: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("mcpclient: rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	var toolResult mcpToolResult
	if err := json.Unmarshal(rpcResp.Result, &toolResult); err != nil {
		return nil, fmt.Errorf("mcpclient: decode tool result: %w", err)
	}

	if toolResult.IsError {
		if len(toolResult.Content) > 0 {
			return nil, fmt.Errorf("mcpclient: tool error: %s", toolResult.Content[0].Text)
		}
		return nil, fmt.Errorf("mcpclient: tool returned error")
	}

	if len(toolResult.Content) == 0 {
		return nil, fmt.Errorf("mcpclient: empty tool result")
	}

	return json.RawMessage(toolResult.Content[0].Text), nil
}

// Ping checks if the MCP server is reachable.
func (c *MCPClient) Ping(ctx context.Context) bool {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return false
	}
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
