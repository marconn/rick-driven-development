package backend

import (
	"fmt"
	"os"
	"strings"
)

// New creates a backend by name. Valid names: "claude", "gemini", "codex".
// Backend binary paths can be overridden via RICK_CLAUDE_BIN, RICK_GEMINI_BIN,
// and RICK_CODEX_BIN environment variables; otherwise they default to the bare
// binary name.
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

	case "codex":
		bin := os.Getenv("RICK_CODEX_BIN")
		if bin == "" {
			bin = "codex"
		}
		return NewCodex(bin), nil

	default:
		return nil, fmt.Errorf("unknown backend: %s (valid: claude, gemini, codex)", name)
	}
}

// DefaultReviewBackends is the fallback rotation order for review-phase
// handlers when RICK_REVIEW_BACKENDS is unset.
var DefaultReviewBackends = []string{"claude", "gemini", "codex"}

// NewReviewBackend builds the backend used by review-phase handlers
// (reviewer, qa, pr-consolidator, pr-category reviewers, feedback-analyzer,
// pr-replier, pr-summarizer, qa-analyzer).
//
// Behavior:
//   - len(names) == 0 → fall back to DefaultReviewBackends.
//   - len == 1        → return that single backend (no rotation overhead).
//   - len >= 2        → return a RoundRobin wrapper over the list, preserving
//     caller-supplied order as the rotation order.
//
// Invalid or duplicate names are rejected. Names are lowercased and trimmed
// before lookup.
func NewReviewBackend(names []string) (Backend, error) {
	if len(names) == 0 {
		names = DefaultReviewBackends
	}
	seen := make(map[string]struct{}, len(names))
	backends := make([]Backend, 0, len(names))
	for _, raw := range names {
		n := strings.ToLower(strings.TrimSpace(raw))
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			return nil, fmt.Errorf("review backend %q listed more than once", n)
		}
		seen[n] = struct{}{}
		b, err := New(n)
		if err != nil {
			return nil, fmt.Errorf("review backend: %w", err)
		}
		backends = append(backends, b)
	}
	switch len(backends) {
	case 0:
		return nil, fmt.Errorf("review backend: no valid backends after parsing")
	case 1:
		return backends[0], nil
	default:
		return NewRoundRobin(backends...)
	}
}

// ParseReviewBackendsEnv splits a RICK_REVIEW_BACKENDS value into a name
// list. Empty input yields nil so callers can apply their own default.
func ParseReviewBackendsEnv(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}
