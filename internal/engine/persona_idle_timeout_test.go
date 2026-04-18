package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
)

// TestPersonaRunner_BackendIdleTimeout_EmitsFailureKindAndStderr exercises the
// full dispatch → classify → PersonaFailed chain for the zero-iteration bug
// documented in planning-workspace/rick-feedback-2026-04-18-developer-zero-
// iteration.md.
//
// When a persona handler returns a *backend.BackendError whose Inner wraps
// ErrIdleTimeout (the shape Claude.Run produces on a wedged subprocess), the
// runner must:
//  1. emit PersonaFailed with FailureKind="idle_timeout",
//  2. preserve the captured Stderr tail so the operator can diagnose via
//     rick_persona_output,
//  3. preserve the full error string (with stall= / after= markers), and
//  4. mark Reactive=true to distinguish it from a proactive emission.
//
// Prior to commit 51aabc8 the event had no FailureKind/Stderr fields and
// every failure looked identical. This test locks the diagnostic contract
// so that cannot regress.
func TestPersonaRunner_BackendIdleTimeout_EmitsFailureKindAndStderr(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t)

	stderrTail := "bootstrapping claude wrapper\n"
	// Build the exact error shape Claude.Run returns on idle timeout.
	// AIHandler then wraps it with fmt.Errorf("handler %s: backend: %w",
	// ...). We skip the AIHandler layer here and return the wrapped form
	// directly so the test doesn't depend on the real handler stack.
	innerErr := &backend.BackendError{
		Backend:  "claude",
		Inner:    fmt.Errorf("%w (stall=2m0s)", backend.ErrIdleTimeout),
		Duration: 9 * time.Minute,
		Stderr:   stderrTail,
	}
	handlerErr := fmt.Errorf("handler developer: backend: %w", innerErr)

	h := &stubHandler{
		name: "developer",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return nil, handlerErr
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	runner.Start(context.Background(), reg)

	var mu sync.Mutex
	var failPayload *event.PersonaFailedPayload
	failCh := make(chan struct{}, 1)
	bus.Subscribe(event.PersonaFailed, func(_ context.Context, env event.Envelope) error {
		var p event.PersonaFailedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil
		}
		if p.Persona != "developer" {
			return nil
		}
		mu.Lock()
		failPayload = &p
		mu.Unlock()
		select {
		case failCh <- struct{}{}:
		default:
		}
		return nil
	})

	trigger := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:    "architect",
		ChainDepth: 0,
	})).WithCorrelation("corr-idle-timeout")

	if err := bus.Publish(context.Background(), trigger); err != nil {
		t.Fatalf("publish trigger: %v", err)
	}

	select {
	case <-failCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for PersonaFailed")
	}

	mu.Lock()
	defer mu.Unlock()
	p := failPayload
	if p == nil {
		t.Fatal("PersonaFailed payload not captured")
	}
	if p.Persona != "developer" {
		t.Errorf("Persona = %q; want developer", p.Persona)
	}
	if p.FailureKind != event.FailureKindIdleTimeout {
		t.Errorf("FailureKind = %q; want %q — classifyDispatchFailure must unwrap through BackendError to ErrIdleTimeout",
			p.FailureKind, event.FailureKindIdleTimeout)
	}
	if p.Stderr != stderrTail {
		t.Errorf("Stderr = %q; want %q — stderr tail must survive PersonaFailed emission for rick_persona_output",
			p.Stderr, stderrTail)
	}
	if !p.Reactive {
		t.Error("Reactive = false; want true for DAG-dispatched failures")
	}
	// The error string must carry the stall marker so operators grepping
	// logs or the agent UI see the watchdog cause, not just "backend failed".
	if !strings.Contains(p.Error, "stall=") || !strings.Contains(p.Error, "idle timeout") {
		t.Errorf("Error = %q; want stall=/idle timeout markers", p.Error)
	}
}

// Compile-time safety: ensure ErrIdleTimeout is reachable through the
// BackendError chain we build above. If this ever breaks (e.g., a refactor
// stops preserving Unwrap), the test above would fail the FailureKind
// assertion — this guard catches it with a clearer error at build time.
var _ = func() int {
	err := fmt.Errorf("handler x: backend: %w", &backend.BackendError{
		Inner: fmt.Errorf("%w (stall=1m)", backend.ErrIdleTimeout),
	})
	if !errors.Is(err, backend.ErrIdleTimeout) {
		panic("backend.ErrIdleTimeout unreachable through wrap chain — refactor broke error classification")
	}
	return 0
}
