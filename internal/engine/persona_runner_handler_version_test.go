package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/buildinfo"
	"github.com/marconn/rick-event-driven-development/internal/event"
)

// TestPersonaRunner_HandlerVersion_PopulatedOnPersonaFailed asserts that every
// PersonaFailed event emitted by executeDispatch carries a non-empty
// HandlerVersion. This is the operator-visible signal that tells which binary
// version produced the failure — critical for diagnosing fleet drift across
// deployments.
//
// The test registers a handler that always fails, publishes a trigger, and
// verifies the HandlerVersion field on the captured PersonaFailed payload.
func TestPersonaRunner_HandlerVersion_PopulatedOnPersonaFailed(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t)

	h := &stubHandler{
		name: "developer",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return nil, fmt.Errorf("handler developer: simulated failure for version test")
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	runner.Start(context.Background(), reg)

	var mu sync.Mutex
	var captured *event.PersonaFailedPayload
	done := make(chan struct{}, 1)

	bus.Subscribe(event.PersonaFailed, func(_ context.Context, env event.Envelope) error {
		var p event.PersonaFailedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil
		}
		if p.Persona != "developer" {
			return nil
		}
		mu.Lock()
		captured = &p
		mu.Unlock()
		select {
		case done <- struct{}{}:
		default:
		}
		return nil
	})

	trigger := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:    "architect",
		ChainDepth: 0,
	})).WithCorrelation("corr-hv-test")

	if err := bus.Publish(context.Background(), trigger); err != nil {
		t.Fatalf("publish trigger: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for PersonaFailed")
	}

	mu.Lock()
	p := captured
	mu.Unlock()

	if p == nil {
		t.Fatal("PersonaFailed payload not captured")
	}
	if p.HandlerVersion == "" {
		t.Errorf("HandlerVersion is empty; want non-empty VCS revision from buildinfo.Version()")
	}
	// The value must match what buildinfo.Version() returns — the runner and
	// the test binary are the same binary, so the revision stamp is identical.
	if want := buildinfo.Version(); p.HandlerVersion != want {
		t.Errorf("HandlerVersion = %q; want %q (buildinfo.Version())", p.HandlerVersion, want)
	}
}
