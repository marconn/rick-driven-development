package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
)

// TestClassifyDispatchFailure exercises every branch of the resolver in the
// same order PersonaRunner hits them at dispatch time, with an emphasis on
// the failure shape that motivated this work: Claude subprocess silent for
// 2m and killed by the idle watchdog — the operator needs
// FailureKindIdleTimeout surfaced, not a generic "handler_error".
func TestClassifyDispatchFailure(t *testing.T) {
	stderr := "stderr tail from subprocess"
	idleErr := &backend.BackendError{
		Backend:  "claude",
		Inner:    fmt.Errorf("%w (stall=2m0s)", backend.ErrIdleTimeout),
		Duration: 2 * time.Minute,
		Stderr:   stderr,
	}
	wallErr := &backend.BackendError{
		Backend:  "claude",
		Inner:    context.DeadlineExceeded,
		Duration: 20 * time.Minute,
		Stderr:   "",
	}
	cancelErr := &backend.BackendError{
		Backend: "claude",
		Inner:   context.Canceled,
	}
	crashErr := &backend.BackendError{
		Backend: "codex",
		Inner:   errors.New("exit status 1"),
		Stderr:  "missing API key",
	}
	// Rate-limit shapes — the substring matcher in looksLikeRateLimit is
	// the canonical detector. The Stderr text mirrors what claude and
	// gemini actually wrote to stdout/stderr in production failures
	// (3555adef-... 2026-05-15 for the claude case).
	claudeRateLimit := &backend.BackendError{
		Backend: "claude",
		Inner:   errors.New("exit status 1"),
		Stderr:  "[no stderr; stdout tail]\nYou've hit your limit · resets 4:50pm (America/Costa_Rica)",
	}
	geminiQuota := &backend.BackendError{
		Backend: "gemini",
		Inner:   errors.New("exit status 1"),
		Stderr:  "Error: RESOURCE_EXHAUSTED: quota exceeded for quota metric",
	}

	cases := []struct {
		name        string
		ctxSetup    func() context.Context
		err         error
		wantKind    event.FailureKind
		wantStderr  string
		wantBackend string
	}{
		{
			name:        "nil_error_is_unspecified",
			ctxSetup:    context.Background,
			err:         nil,
			wantKind:    event.FailureKindUnspecified,
			wantStderr:  "",
			wantBackend: "",
		},
		{
			name:     "idle_timeout_wrapped_by_handler",
			ctxSetup: context.Background,
			// Mirror AIHandler's fmt.Errorf("handler %s: backend: %w", ...) wrap.
			err:         fmt.Errorf("handler developer: backend: %w", idleErr),
			wantKind:    event.FailureKindIdleTimeout,
			wantStderr:  stderr,
			wantBackend: "claude",
		},
		{
			name:        "wall_timeout_detected_via_deadline_exceeded",
			ctxSetup:    context.Background,
			err:         fmt.Errorf("handler developer: backend: %w", wallErr),
			wantKind:    event.FailureKindWallTimeout,
			wantStderr:  "",
			wantBackend: "claude",
		},
		{
			name:        "cancelled_detected_via_canceled_sentinel",
			ctxSetup:    context.Background,
			err:         fmt.Errorf("handler developer: backend: %w", cancelErr),
			wantKind:    event.FailureKindCancelled,
			wantStderr:  "",
			wantBackend: "claude",
		},
		{
			name:        "backend_error_with_stderr",
			ctxSetup:    context.Background,
			err:         fmt.Errorf("handler developer: backend: %w", crashErr),
			wantKind:    event.FailureKindBackendError,
			wantStderr:  "missing API key",
			wantBackend: "codex",
		},
		{
			// Bug 2 regression: claude rate-limit message captured into the
			// BackendError.Stderr via the stdoutFallbackPrefix path. The
			// classifier must surface FailureKindRateLimited so the
			// aggregate takes the WorkflowPaused branch instead of failing
			// the workflow and cascade-cancelling the parallel sibling.
			name:        "rate_limited_claude_stdout_fallback",
			ctxSetup:    context.Background,
			err:         fmt.Errorf("handler reviewer: backend: %w", claudeRateLimit),
			wantKind:    event.FailureKindRateLimited,
			wantStderr:  claudeRateLimit.Stderr,
			wantBackend: "claude",
		},
		{
			// gemini surfaces quota exhaustion as RESOURCE_EXHAUSTED — the
			// pattern allow-list covers both spellings.
			name:        "rate_limited_gemini_resource_exhausted",
			ctxSetup:    context.Background,
			err:         fmt.Errorf("handler qa: backend: %w", geminiQuota),
			wantKind:    event.FailureKindRateLimited,
			wantStderr:  geminiQuota.Stderr,
			wantBackend: "gemini",
		},
		{
			// A backend error with stderr that does NOT match any
			// rate-limit pattern must still fall to FailureKindBackendError
			// — the pattern matcher is conservative on purpose.
			name:     "backend_error_unrelated_stderr_stays_backend_error",
			ctxSetup: context.Background,
			err: fmt.Errorf("handler developer: backend: %w", &backend.BackendError{
				Backend: "claude",
				Inner:   errors.New("exit status 1"),
				Stderr:  "panic: index out of range [3] with length 2",
			}),
			wantKind:    event.FailureKindBackendError,
			wantStderr:  "panic: index out of range [3] with length 2",
			wantBackend: "claude",
		},
		{
			name:        "handler_error_when_no_backend_markers",
			ctxSetup:    context.Background,
			err:         errors.New("handler researcher: load system prompt: file not found"),
			wantKind:    event.FailureKindHandlerError,
			wantStderr:  "",
			wantBackend: "",
		},
		{
			// Fallback path: handler returned a naked error because its ctx
			// was cancelled mid-call. Without this fallback, operator-cancel
			// would mislabel as handler_error.
			name: "naked_error_with_cancelled_ctx_resolves_to_cancelled",
			ctxSetup: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			err:         errors.New("opaque inner error"),
			wantKind:    event.FailureKindCancelled,
			wantStderr:  "",
			wantBackend: "",
		},
		{
			name: "naked_error_with_deadline_ctx_resolves_to_wall_timeout",
			ctxSetup: func() context.Context {
				ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
				t.Cleanup(cancel)
				// Give the deadline a moment to actually trip.
				time.Sleep(5 * time.Millisecond)
				return ctx
			},
			err:         errors.New("opaque inner error"),
			wantKind:    event.FailureKindWallTimeout,
			wantStderr:  "",
			wantBackend: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, stderr, backendName := classifyDispatchFailure(tc.ctxSetup(), tc.err)
			if kind != tc.wantKind {
				t.Errorf("kind = %q; want %q", kind, tc.wantKind)
			}
			if stderr != tc.wantStderr {
				t.Errorf("stderr = %q; want %q", stderr, tc.wantStderr)
			}
			if backendName != tc.wantBackend {
				t.Errorf("backend = %q; want %q", backendName, tc.wantBackend)
			}
		})
	}
}
