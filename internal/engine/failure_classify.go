package engine

import (
	"context"
	"errors"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
)

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
//  4. errors.As(err, *BackendError)         → FailureKindBackendError
//  5. dispatchCtx.Err() == context.Canceled/DeadlineExceeded
//     → FailureKindCancelled / FailureKindWallTimeout
//  6. default                               → FailureKindHandlerError
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
	case errors.Is(err, backend.ErrIdleTimeout):
		return event.FailureKindIdleTimeout, stderr, backendName
	case errors.Is(err, context.DeadlineExceeded):
		return event.FailureKindWallTimeout, stderr, backendName
	case errors.Is(err, context.Canceled):
		return event.FailureKindCancelled, stderr, backendName
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
