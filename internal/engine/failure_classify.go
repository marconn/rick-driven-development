package engine

import (
	"context"
	"errors"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
)

// rateLimitStderrPatterns are the substrings the classifier looks for in
// captured backend stderr/stdout-tail to recognise an upstream rate-limit.
// Conservative on purpose — a false positive labels a real crash as
// "rate_limited" which routes through the pause path instead of failing, so
// the operator could miss a genuine bug. Add patterns only when an actual
// rate-limit produced the literal string in production logs.
//
// Provider sources:
//   - claude CLI: "You've hit your limit · resets <localtime>" (stdout JSON,
//     captured into BackendError.Stderr via the stdoutFallbackPrefix path).
//   - gemini CLI: "quota exceeded for quota" / "RESOURCE_EXHAUSTED" (REST
//     error surface, propagated to stderr).
//   - codex CLI: "rate_limit_exceeded" (JSON error type field).
//
// All matching is case-insensitive substring — the stderr field already
// includes the stdoutFallbackPrefix marker when claude wrote the message to
// stdout, but the marker itself doesn't interfere with substring search.
var rateLimitStderrPatterns = []string{
	"you've hit your limit",
	"hit your usage limit",
	"rate_limit_exceeded",
	"resource_exhausted",
	"quota exceeded",
	"too many requests", // HTTP 429 shape some CLIs surface verbatim
}

// looksLikeRateLimit reports whether stderr contains any of the known
// rate-limit signatures. Empty stderr returns false (silent stall is its own
// classification — FailureKindIdleTimeout already handles that).
func looksLikeRateLimit(stderr string) bool {
	if stderr == "" {
		return false
	}
	lower := strings.ToLower(stderr)
	for _, pat := range rateLimitStderrPatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// classifyDispatchFailure maps a handler dispatch error into a typed
// FailureKind and extracts any captured subprocess stderr plus the backend
// driver name. The returned kind is the most specific classification
// available — callers should emit PersonaFailed with this kind so operators
// and auto-retry policies can tell a wedged subprocess apart from a
// deterministic code error.
//
// The per-correlation ctx is inspected as the last-resort classifier:
// when the handler returned a naked error because its inner context was
// already cancelled, the caller has no stable sentinel to check. In that
// case we surface the cancellation cause so "operator cancelled" does not
// get mis-labelled as a backend crash.
//
// Resolution order (first match wins):
//  1. errors.Is(err, ErrIdleTimeout)        → FailureKindIdleTimeout
//  2. errors.Is(err, context.DeadlineExceeded) → FailureKindWallTimeout
//  3. errors.Is(err, context.Canceled)      → FailureKindCancelled
//  4. *BackendError with rate-limit stderr  → FailureKindRateLimited
//  5. errors.As(err, *BackendError)         → FailureKindBackendError
//  6. dispatchCtx.Err() == context.Canceled/DeadlineExceeded
//     → FailureKindCancelled / FailureKindWallTimeout
//  7. default                               → FailureKindHandlerError
//
// Rate-limit detection sits BELOW the timeout/cancel sentinels — a request
// that timed out is classified by what we did to it (idle/wall timeout),
// not by what the provider would have said. It sits ABOVE the generic
// BackendError fallback so the aggregate can take the pause path instead
// of failing the workflow.
//
// The returned backend name is the BackendError.Backend field when the
// failure came through a BackendError, otherwise "". It is used to populate
// PersonaFailedPayload.Backend so downstream consumers (aggregate,
// projection, status) can attribute the failure to a specific driver
// without re-parsing error strings.
func classifyDispatchFailure(dispatchCtx context.Context, err error) (kind event.FailureKind, stderr string, backendName string) {
	if err == nil {
		return event.FailureKindUnspecified, "", ""
	}

	var backendErr *backend.BackendError
	hasBackendErr := errors.As(err, &backendErr)
	if hasBackendErr {
		stderr = backendErr.Stderr
		backendName = backendErr.Backend
	}

	switch {
	case errors.Is(err, backend.ErrProgressTimeout):
		return event.FailureKindProgressTimeout, stderr, backendName
	case errors.Is(err, backend.ErrIdleTimeout):
		return event.FailureKindIdleTimeout, stderr, backendName
	case errors.Is(err, context.DeadlineExceeded):
		return event.FailureKindWallTimeout, stderr, backendName
	case errors.Is(err, context.Canceled):
		return event.FailureKindCancelled, stderr, backendName
	case hasBackendErr && looksLikeRateLimit(stderr):
		return event.FailureKindRateLimited, stderr, backendName
	case hasBackendErr:
		return event.FailureKindBackendError, stderr, backendName
	}

	// Fallback: inspect the dispatch context when the error has no
	// distinguishing marker. A cancelled per-correlation context implies
	// the handler was aborted mid-flight and should not be labelled as
	// its own bug.
	if dispatchCtx != nil {
		if ctxErr := dispatchCtx.Err(); ctxErr != nil {
			switch {
			case errors.Is(ctxErr, context.DeadlineExceeded):
				return event.FailureKindWallTimeout, stderr, backendName
			case errors.Is(ctxErr, context.Canceled):
				return event.FailureKindCancelled, stderr, backendName
			}
		}
	}

	return event.FailureKindHandlerError, stderr, backendName
}

// failureKindIsAutoRetryable reports whether a PersonaFailed of this shape
// warrants an automatic retry-with-rotation (WorkflowRetried{Automatic}).
//
//   - FailureKindIdleTimeout: the backend subprocess went silent — a retry on
//     a rotated backend frequently succeeds (the developer-zero-iteration fix).
//   - FailureKindWallTimeout: terminal for a watchdog-equipped backend (a 15m+
//     active run is unlikely to finish on immediate retry), BUT transient for a
//     backend with no idle watchdog (antigravity). There the wall-clock is the
//     only liveness deadline, so a wall-timeout is the exact analogue of the
//     idle-timeout a watchdog-equipped backend emits on a silent stall — and
//     rotating to a different backend is the recovery. Without this, antigravity
//     in a review rotation can never auto-recover: its sole failure mode
//     (wall-timeout) would be unretryable and it deterministically re-lands on
//     itself (the reviewer-wedge loop).
//
// Every other kind has its own path (rate-limit pause, partial-review skip,
// terminal WorkflowFailed) and must not consume an auto-retry slot here.
func failureKindIsAutoRetryable(p event.PersonaFailedPayload) bool {
	switch p.FailureKind {
	case event.FailureKindIdleTimeout:
		return true
	case event.FailureKindWallTimeout:
		return !backend.HasIdleWatchdog(p.Backend)
	default:
		return false
	}
}
