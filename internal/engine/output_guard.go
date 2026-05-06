package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// developerGuardOutputLenThreshold is the byte ceiling below which a
// developer-class handler is considered to have produced no usable output.
// 64 bytes covers all observed corrupted shapes from the 2026-04-29 incident
// (`["sub"]`=7B, `{}`=2B, `{"api_key":"apiuser_apikey_julio_ehr"}`=38B) while
// staying well under any plausible legitimate response length (real developer
// outputs are thousands of bytes — file lists, diffs, summaries).
const developerGuardOutputLenThreshold = 64

// developerGuardEnabled is overridable by tests via WithDeveloperGuard option.
// In production the guard is always on.

// aiResponseTextLen returns the length of the decoded text inside an
// AIResponseReceived payload, or -1 when the payload cannot be decoded
// (in which case the guard treats it as not applicable — fail-open).
func aiResponseTextLen(payload []byte) int {
	var p event.AIResponsePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return -1
	}
	if p.Structured {
		// Structured responses keep raw JSON in Output; their byte length is
		// already meaningful (a one-byte JSON `{}` is just as suspect as the
		// plain-text equivalent).
		return len(p.Output)
	}
	var text string
	if err := json.Unmarshal(p.Output, &text); err != nil {
		return len(p.Output)
	}
	return len(text)
}

// isDeveloperHandler reports whether h is the developer persona.
func isDeveloperHandler(h handler.Handler) bool {
	return h != nil && h.Name() == "developer"
}

// developerOutputGuardTrips returns true when the dispatch should be reported
// as PersonaFailed{output_truncated} instead of PersonaCompleted because the
// developer-class handler produced no meaningful text but the workspace shows
// uncommitted work.
func (r *PersonaRunner) developerOutputGuardTrips(h handler.Handler, correlationID string, outputTextLen int) bool {
	if !isDeveloperHandler(h) {
		return false
	}
	// outputTextLen == -1 means we couldn't decode the AIResponsePayload, or
	// the handler emitted no AIResponseReceived at all (compensation handlers,
	// no-op runs). Don't trip the guard on missing data — only on positively
	// short text.
	if outputTextLen < 0 || outputTextLen >= developerGuardOutputLenThreshold {
		return false
	}
	wsPath := r.lookupWorkspacePath(correlationID)
	if wsPath == "" {
		// No WorkspaceReady on the chain → develop-only / synthetic flow with
		// no checkout to inspect. Don't trip — there's no divergence signal.
		return false
	}
	dirty, err := workspaceHasUncommittedChanges(r.guardCtx(), wsPath)
	if err != nil {
		// git invocation failed (path moved, no .git, etc.). Log and stand
		// down — the guard is a safety net, never a source of false fails.
		r.logger.Warn("persona runner: developer guard could not inspect workspace",
			slog.String("workspace", wsPath),
			slog.String("correlation", correlationID),
			slog.Any("err", err))
		return false
	}
	if !dirty {
		return false
	}
	r.logger.Warn("persona runner: developer output guard tripped",
		slog.String("correlation", correlationID),
		slog.String("workspace", wsPath),
		slog.Int("output_text_len", outputTextLen))
	return true
}

// guardCtx returns the runner's lifetime context, or context.Background() when
// the runner hasn't been Start()ed (only happens in unit tests that exercise
// helpers directly). The production dispatch path always has r.ctx set.
func (r *PersonaRunner) guardCtx() context.Context {
	if r.ctx != nil {
		return r.ctx
	}
	return context.Background()
}

// lookupWorkspacePath scans the correlation chain for the most recent
// WorkspaceReady event and returns its Path. Empty string when none found.
func (r *PersonaRunner) lookupWorkspacePath(correlationID string) string {
	if correlationID == "" {
		return ""
	}
	events, err := r.store.LoadByCorrelation(r.guardCtx(), correlationID)
	if err != nil {
		return ""
	}
	path := ""
	for _, e := range events {
		if e.Type != event.WorkspaceReady {
			continue
		}
		var p event.WorkspaceReadyPayload
		if json.Unmarshal(e.Payload, &p) == nil && p.Path != "" {
			path = p.Path
		}
	}
	return path
}

// workspaceHasUncommittedChanges runs `git -C <path> status --porcelain` and
// reports whether the working tree has any staged or unstaged changes.
// Stderr is intentionally suppressed; only the stdout (porcelain output) is
// inspected for non-emptiness.
func workspaceHasUncommittedChanges(ctx context.Context, path string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}
