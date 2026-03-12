package backend

import (
	"fmt"
	"os"
)

// New creates a backend by name. Valid names: "claude", "gemini".
// Backend binary paths can be overridden via RICK_CLAUDE_BIN and RICK_GEMINI_BIN
// environment variables; otherwise they default to the bare binary name.
func New(name string) (Backend, error) {
	switch name {
	case "claude":
		bin := os.Getenv("RICK_CLAUDE_BIN")
		if bin == "" {
			bin = "claude"
		}
		return NewClaude(bin), nil

	case "gemini":
		bin := os.Getenv("RICK_GEMINI_BIN")
		if bin == "" {
			bin = "gemini"
		}
		return NewGemini(bin), nil

	default:
		return nil, fmt.Errorf("unknown backend: %s (valid: claude, gemini)", name)
	}
}
