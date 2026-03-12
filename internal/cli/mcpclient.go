package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// mcpCall sends a JSON-RPC tools/call request to the rick MCP server.
// Returns the tool result text, or an error if the server is unreachable
// or the tool returns an error.
func mcpCall(ctx context.Context, serverURL, toolName string, args map[string]any) (string, error) {
	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("server unreachable: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	var rpcResp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if rpcResp.Error != nil {
		return "", fmt.Errorf("rpc error: %s", rpcResp.Error.Message)
	}

	// Extract text from tool result.
	var toolResult struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(rpcResp.Result, &toolResult); err != nil {
		return "", fmt.Errorf("parse tool result: %w", err)
	}

	text := ""
	if len(toolResult.Content) > 0 {
		text = toolResult.Content[0].Text
	}
	if toolResult.IsError {
		return "", fmt.Errorf("tool error: %s", text)
	}
	return text, nil
}

const defaultMCPURL = "http://localhost:8077/mcp"

// replayAggregate loads all events for an aggregate and replays them.
func replayAggregate(ctx context.Context, store eventstore.Store, aggregateID string) (*engine.WorkflowAggregate, error) {
	events, err := store.Load(ctx, aggregateID)
	if err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("workflow not found: %s", aggregateID)
	}

	agg := engine.NewWorkflowAggregate(aggregateID)
	for _, env := range events {
		agg.Apply(env)
	}
	return agg, nil
}
