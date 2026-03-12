package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// Event is emitted to the frontend during agent processing.
type Event struct {
	Type     string `json:"type"`               // "response", "tool_call", "tool_result", "error", "done"
	Content  string `json:"content,omitempty"`   // text content (for response/error)
	ToolName string `json:"tool_name,omitempty"` // tool name (for tool_call/tool_result)
}

// App is the Wails application backend. Exported methods are callable
// from the Svelte frontend via the generated bindings.
type App struct {
	ctx         context.Context
	operator    *Operator
	mcpClient   *MCPClient
	memoryStore *MemoryStore
	cfg         Config
	logger      *slog.Logger
	initErr     string // non-empty if Init failed — shown on first SendMessage
}

// NewApp creates the application backend.
func NewApp() *App {
	cfg := DefaultConfig()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ms, err := NewMemoryStore()
	if err != nil {
		logger.Warn("memory store unavailable", slog.String("error", err.Error()))
	}

	return &App{
		cfg:         cfg,
		operator:    NewOperator(cfg, ms, logger),
		mcpClient:   NewMCPClient(cfg.ServerURL),
		memoryStore: ms,
		logger:      logger,
	}
}

// startup is called when the Wails app starts.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	// Validate config before attempting init.
	if err := a.cfg.Validate(); err != nil {
		a.initErr = err.Error()
		a.logger.Error("operator config invalid", slog.String("error", err.Error()))
		runtime.EventsEmit(ctx, "agent:error", err.Error())
		return
	}

	// Try to discover tools from server on startup.
	go func() {
		if err := a.operator.Init(ctx); err != nil {
			a.initErr = err.Error()
			a.logger.Warn("operator init failed", slog.String("error", err.Error()))
			runtime.EventsEmit(ctx, "agent:error", err.Error())
		} else {
			a.initErr = ""
			runtime.EventsEmit(ctx, "agent:connected", true)
		}
	}()
}

// shutdown is called when the Wails app shuts down.
func (a *App) shutdown(_ context.Context) {
	a.logger.Info("app shutting down")
}

// SendMessage sends a user message to the agent and streams responses
// back to the frontend via Wails events.
func (a *App) SendMessage(text string) {
	go func() {
		// Surface the actual init failure reason instead of a generic message.
		if a.initErr != "" {
			runtime.EventsEmit(a.ctx, "agent:event", Event{
				Type:    "error",
				Content: "Operator not ready: " + a.initErr,
			})
			return
		}

		emit := func(evt Event) {
			runtime.EventsEmit(a.ctx, "agent:event", evt)
		}

		response, err := a.operator.Run(a.ctx, text, emit)
		if err != nil {
			runtime.EventsEmit(a.ctx, "agent:event", Event{
				Type:    "error",
				Content: err.Error(),
			})
			return
		}

		runtime.EventsEmit(a.ctx, "agent:event", Event{
			Type:    "response",
			Content: response,
		})
		runtime.EventsEmit(a.ctx, "agent:event", Event{Type: "done"})
	}()
}

// GetConfig returns the current configuration (API key excluded).
func (a *App) GetConfig() Config {
	return Config{
		ServerURL: a.cfg.ServerURL,
		Model:     a.cfg.Model,
	}
}

// CheckConnection tests connectivity to the rick-server.
func (a *App) CheckConnection() bool {
	return a.operator.Connected(a.ctx)
}

// ClearContext resets the operator's conversation history without
// re-discovering MCP tools. Returns an error string or empty on success.
func (a *App) ClearContext() string {
	if err := a.operator.ResetContext(a.ctx); err != nil {
		return err.Error()
	}
	return ""
}

// SaveMemory persists a memory entry. Returns the saved Memory or an error string.
func (a *App) SaveMemory(content, category string) *Memory {
	if a.memoryStore == nil {
		return nil
	}
	m, err := a.memoryStore.Add(content, category)
	if err != nil {
		a.logger.Error("save memory failed", slog.String("error", err.Error()))
		return nil
	}
	return &m
}

// ListMemories returns all stored memories.
func (a *App) ListMemories() []Memory {
	if a.memoryStore == nil {
		return nil
	}
	return a.memoryStore.List()
}

// DeleteMemory removes a memory by ID (or prefix). Returns true if found.
func (a *App) DeleteMemory(id string) bool {
	if a.memoryStore == nil {
		return false
	}
	return a.memoryStore.Delete(id)
}

// SearchMemories returns memories matching a query string.
func (a *App) SearchMemories(query string) []Memory {
	if a.memoryStore == nil {
		return nil
	}
	return a.memoryStore.Search(query)
}

// Reconnect re-discovers tools from the MCP server.
func (a *App) Reconnect() string {
	if err := a.cfg.Validate(); err != nil {
		a.initErr = err.Error()
		return err.Error()
	}
	if err := a.operator.Init(a.ctx); err != nil {
		a.initErr = err.Error()
		return err.Error()
	}
	a.initErr = ""
	return ""
}
