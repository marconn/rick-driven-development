package backend

import (
	"context"
	"io"
	"time"
)

// Backend is the interface for AI LLM provider drivers.
// Implementations shell out to CLI binaries (claude, gemini) and capture
// the full response for event-driven handler consumption.
type Backend interface {
	// Name returns the backend identifier (e.g., "claude", "gemini").
	Name() string

	// Run executes an AI request and returns the captured response.
	// The full LLM output is captured regardless of whether streaming
	// is enabled via Request.Output.
	Run(ctx context.Context, req Request) (*Response, error)
}

// Request configures an AI backend execution.
type Request struct {
	SystemPrompt string    // LLM system prompt (persona instructions).
	UserPrompt   string    // User/task prompt.
	Model        string    // Optional model override (e.g., "claude-haiku-4-5-20251001").
	WorkDir      string    // Working directory for backend execution.
	Yolo         bool      // Skip permission checks (Claude: --dangerously-skip-permissions, Gemini: --yolo).
	MCPConfig    string    // JSON MCP server config (passed via --mcp-config for Claude).
	SessionID    string    // "latest" to continue most recent, or a specific session ID to resume.
	Output       io.Writer // Optional: stream extracted text here in real-time (tee'd with capture buffer).
}

// Response captures the result of an AI backend execution.
type Response struct {
	Output     string        // Full captured text output from the LLM.
	StopReason string        // "end_turn", "max_tokens", etc. Empty if not captured.
	Duration   time.Duration // Wall clock execution duration.
	// TokensUsed is the total token count for the request (input + output +
	// cache_creation_input + cache_read_input). Sourced from the authoritative
	// "result" event in the stream-json output; falls back to the last-seen
	// message_start + message_delta counters if no result arrives. Zero when
	// the backend does not report usage (e.g., Gemini — handled separately).
	TokensUsed int
}

// maxArgSize is the threshold above which prompts are piped via stdin
// instead of passed as CLI arguments, to avoid OS ARG_MAX limits.
// Set conservatively at 128KB (ARG_MAX is ~2MB on Linux, but we share
// the budget with other args, env vars, and the binary path).
const maxArgSize = 128 << 10
