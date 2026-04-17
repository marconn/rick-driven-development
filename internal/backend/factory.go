package backend

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultStallTimeout is applied when RICK_BACKEND_STALL_TIMEOUT is unset.
// 2 minutes is long enough for any modern LLM CLI to emit its first token
// under normal load — if a subprocess sits silent for that long, it's
// almost certainly wedged and should be killed so the concurrency slot can
// be reclaimed well before the wall-clock timeout fires.
const defaultStallTimeout = 2 * time.Minute

// New creates a backend by name. Valid names: "claude", "gemini", "codex".
// Backend binary paths can be overridden via RICK_CLAUDE_BIN, RICK_GEMINI_BIN,
// and RICK_CODEX_BIN environment variables; otherwise they default to the bare
// binary name.
//
// When RICK_BACKEND_CONCURRENCY_<UPPER> is set to a positive integer, the
// backend is wrapped in a concurrency limiter so no more than that many Run
// calls execute in parallel against the same CLI. The unset / zero / negative
// default is unlimited (historical behavior).
func New(name string) (Backend, error) {
	return NewWithRecorder(name, nil)
}

// NewWithRecorder is New plus an optional Recorder for observability. The
// recorder only fires when a concurrency limit is configured — unlimited
// backends skip it entirely.
func NewWithRecorder(name string, recorder Recorder) (Backend, error) {
	b, err := newRaw(name)
	if err != nil {
		return nil, err
	}
	return NewLimitedBackend(b, concurrencyLimitFor(name), recorder), nil
}

func newRaw(name string) (Backend, error) {
	stall := stallTimeoutFromEnv()
	switch name {
	case "claude":
		bin := os.Getenv("RICK_CLAUDE_BIN")
		if bin == "" {
			bin = "claude"
		}
		c := NewClaude(bin)
		c.stallTimeout = stall
		return c, nil

	case "gemini":
		bin := os.Getenv("RICK_GEMINI_BIN")
		if bin == "" {
			bin = "gemini"
		}
		g := NewGemini(bin)
		g.stallTimeout = stall
		return g, nil

	case "codex":
		bin := os.Getenv("RICK_CODEX_BIN")
		if bin == "" {
			bin = "codex"
		}
		c := NewCodex(bin)
		c.stallTimeout = stall
		return c, nil

	default:
		return nil, fmt.Errorf("unknown backend: %s (valid: claude, gemini, codex)", name)
	}
}

// stallTimeoutFromEnv reads RICK_BACKEND_STALL_TIMEOUT. Unset → default (2m);
// "0" → disabled (idle watchdog off, only the wall-clock timeout applies);
// unparseable → default with no noise. Negative values are treated as 0.
func stallTimeoutFromEnv() time.Duration {
	raw := os.Getenv("RICK_BACKEND_STALL_TIMEOUT")
	if raw == "" {
		return defaultStallTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return defaultStallTimeout
	}
	if d < 0 {
		return 0
	}
	return d
}

// concurrencyLimitFor returns the configured concurrency cap for a backend
// name. Unset, zero, negative, or unparseable values mean unlimited. The env
// var is RICK_BACKEND_CONCURRENCY_CLAUDE / _GEMINI / _CODEX.
func concurrencyLimitFor(name string) int {
	raw := os.Getenv("RICK_BACKEND_CONCURRENCY_" + strings.ToUpper(name))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
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
// before lookup. Each inner backend honors RICK_BACKEND_CONCURRENCY_<UPPER>
// independently, so the aggregate review concurrency is the sum of per-CLI
// caps.
func NewReviewBackend(names []string) (Backend, error) {
	return NewReviewBackendWithRecorder(names, nil)
}

// NewReviewBackendWithRecorder is NewReviewBackend plus an optional observer
// attached to each inner limiter.
func NewReviewBackendWithRecorder(names []string, recorder Recorder) (Backend, error) {
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
		b, err := NewWithRecorder(n, recorder)
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
