package backend

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultStallTimeout is applied when RICK_BACKEND_STALL_TIMEOUT is unset.
// 6 minutes covers the worst-case stdout-silence window observed in
// anthropics/claude-code#51568 (extended-thinking gaps up to 7.5 min) and
// the long-tool-execution gap (between system.task_started and
// system.task_notification, the parent stdout is silent for the tool's full
// runtime). Shorter values produce false-positive idle timeouts on healthy
// developer-phase runs that spend time in MCP / Bash / Task subagents. The
// CLI's own internal stream watchdog (CLAUDE_CODE_STREAM_TIMEOUT, 45s
// default; 5min full-abort+retry since 2.1.105) is the primary liveness
// detector — this outer timeout exists only to bound the dead time when
// the CLI itself wedges past its own watchdogs.
const defaultStallTimeout = 6 * time.Minute

// Descriptor describes a known backend: its name, the env var that overrides
// its CLI binary path, and the default binary name when that env is unset.
type Descriptor struct {
	Name       string
	BinEnv     string
	DefaultBin string
}

// Catalog is the single source of truth for which backends exist and how their
// binaries are resolved. The factory switch, the valid-names error message,
// and any operator-facing backend listing all derive from this so they cannot
// drift apart.
var Catalog = []Descriptor{
	{Name: "claude", BinEnv: "RICK_CLAUDE_BIN", DefaultBin: "claude"},
	{Name: "gemini", BinEnv: "RICK_GEMINI_BIN", DefaultBin: "gemini"},
	{Name: "codex", BinEnv: "RICK_CODEX_BIN", DefaultBin: "codex"},
	{Name: "antigravity", BinEnv: "RICK_ANTIGRAVITY_BIN", DefaultBin: "agy"},
	{Name: "opencode", BinEnv: "RICK_OPENCODE_BIN", DefaultBin: "opencode"},
}

// descriptorFor returns the catalog entry for a backend name, or false.
func descriptorFor(name string) (Descriptor, bool) {
	for _, d := range Catalog {
		if d.Name == name {
			return d, true
		}
	}
	return Descriptor{}, false
}

// ResolveBinary returns the CLI binary path for a backend name, honoring the
// per-backend env override and falling back to the catalog default. Unknown
// names return "".
func ResolveBinary(name string) string {
	d, ok := descriptorFor(name)
	if !ok {
		return ""
	}
	if bin := os.Getenv(d.BinEnv); bin != "" {
		return bin
	}
	return d.DefaultBin
}

// Names returns the valid backend names in catalog order.
func Names() []string {
	out := make([]string, len(Catalog))
	for i, d := range Catalog {
		out[i] = d.Name
	}
	return out
}

// New creates a backend by name. Valid names come from Catalog: "claude",
// "gemini", "codex", "antigravity", "opencode". Backend binary paths can be
// overridden via RICK_CLAUDE_BIN, RICK_GEMINI_BIN, RICK_CODEX_BIN,
// RICK_ANTIGRAVITY_BIN, and RICK_OPENCODE_BIN environment variables; otherwise
// they default to the bare binary name (`agy` for antigravity).
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
	bin := ResolveBinary(name)
	switch name {
	case "claude":
		c := NewClaude(bin)
		c.stallTimeout = stall
		// Completion-progress watchdog is claude-only for now: the wedge it
		// guards (tool-loop chatter with zero text deltas) is claude-specific,
		// and the "substantive progress" signal is wired from the claude
		// stream extractor in claude.go. Other drivers parse different stream
		// shapes and would need their own wiring before honoring it.
		c.progressTimeout = progressTimeoutFromEnv()
		return c, nil

	case "gemini":
		g := NewGemini(bin)
		g.stallTimeout = stall
		return g, nil

	case "codex":
		c := NewCodex(bin)
		c.stallTimeout = stall
		return c, nil

	case "antigravity":
		a := NewAntigravity(bin)
		// Idle byte watchdog is intentionally NOT armed for antigravity:
		// `agy -p` emits plain text on stdout only when the model finishes
		// generating (no incremental stream), so the watchdog has no
		// liveness signal to reset against and false-kills any healthy run
		// past RICK_BACKEND_STALL_TIMEOUT. `--print-timeout 30m` plus the
		// outer rick wall-clock (RICK_BACKEND_TIMEOUT / RICK_REVIEW_BACKEND_TIMEOUT)
		// are the binding deadlines instead.
		return a, nil

	case "opencode":
		o := NewOpencode(bin)
		o.stallTimeout = stall
		return o, nil

	default:
		return nil, fmt.Errorf("unknown backend: %s (valid: %s)", name, strings.Join(Names(), ", "))
	}
}

// HasIdleWatchdog reports whether the named backend arms the idle byte
// watchdog (WithIdleTimeout) when RICK_BACKEND_STALL_TIMEOUT > 0.
//
// All streaming backends (claude/gemini/codex/opencode) do — their stdout
// emits incrementally, so a silent gap is a meaningful liveness signal that
// the watchdog converts into FailureKindIdleTimeout. antigravity does NOT:
// `agy -p` flushes stdout only when the model finishes (no incremental
// stream), so the byte watchdog has nothing to reset against and false-kills
// any healthy run — see newRaw's antigravity case. For antigravity the
// wall-clock is the ONLY liveness deadline.
//
// Callers use this to set retry policy: a wall-timeout on a backend with no
// idle watchdog is the analogue of the idle-timeout a watchdog-equipped
// backend would have produced on a silent stall, so it warrants the same
// retry-with-rotation treatment. Unknown / empty names return true
// (conservative: do not auto-retry an unattributed wall-timeout).
//
// MUST stay in sync with newRaw's per-backend watchdog wiring.
func HasIdleWatchdog(name string) bool {
	return name != "antigravity"
}

// stallTimeoutFromEnv reads RICK_BACKEND_STALL_TIMEOUT. Unset → default
// (defaultStallTimeout); "0" → disabled (idle watchdog off, only the
// wall-clock timeout applies); unparseable → default with no noise.
// Negative values are treated as 0.
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

// progressTimeoutFromEnv reads RICK_BACKEND_PROGRESS_TIMEOUT, the
// completion-progress watchdog window. Unlike the stall watchdog it defaults
// to DISABLED (0): killing a subprocess that is emitting bytes but no answer
// text is a stronger claim than killing a fully silent one, so it must not
// change behavior until an operator opts in. Unset / "0" / negative /
// unparseable all yield 0 (off). A positive duration arms the watchdog at that
// window — pick a value above any legitimate tool-only gap but below
// RICK_BACKEND_TIMEOUT.
func progressTimeoutFromEnv() time.Duration {
	raw := os.Getenv("RICK_BACKEND_PROGRESS_TIMEOUT")
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
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
var DefaultReviewBackends = []string{"antigravity", "claude"}

// NewReviewBackend builds the backend used by review-phase handlers
// (reviewer, qa, pr-category reviewers, feedback-analyzer, pr-replier,
// qa-analyzer). pr-consolidator pins to claude+haiku itself — see
// handler.NewPRConsolidator.
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
