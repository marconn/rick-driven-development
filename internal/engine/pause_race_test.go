package engine

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// These tests cover the race between a handler's outputs (advisory verdict +
// PersonaCompleted) and the lifecycle pause set by the engine in response. The
// race window: a downstream handler's wrap goroutine fires on PersonaCompleted
// while engine.processLoop is still deriving WorkflowPaused from the verdict.
// If the wrap passes its pause check at line 677 before pauser.pause is set,
// the dispatch is enqueued and the drain goroutine has historically had no
// pause-awareness — so the downstream handler runs on top of an advisory pause.
//
// All three tests should FAIL before the fix and PASS after.

// TestPersonaRunner_DrainPauseCheck_BlocksMidQueue exercises the drain-time
// pause check. Multiple events are enqueued for one handler; pause is set
// after the first item is dispatched. The remaining items must be moved to the
// blocked list and not dispatched until resume.
func TestPersonaRunner_DrainPauseCheck_BlocksMidQueue(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t)

	dispatched := make(chan string, 8)
	firstSeen := make(chan struct{}, 1)
	var firstOnce atomic.Bool

	h := &stubHandler{
		name: "downstream",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, env event.Envelope) ([]event.Envelope, error) {
			var pc event.PersonaCompletedPayload
			_ = json.Unmarshal(env.Payload, &pc)
			dispatched <- pc.TriggerID
			if firstOnce.CompareAndSwap(false, true) {
				firstSeen <- struct{}{}
				// Hold the first dispatch open long enough for the test to
				// (a) enqueue more items behind it and (b) flip pause on,
				// before the drain pops the next item.
				time.Sleep(150 * time.Millisecond)
			}
			return nil, nil
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	runner.Start(context.Background(), reg)

	corrID := "corr-drain-pause"
	mkEvt := func(suffix string) event.Envelope {
		return event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
			Persona:      "developer",
			TriggerEvent: "phase.scheduled",
			TriggerID:    "trigger-" + suffix,
			Reactive:     false,
		})).
			WithAggregate("wf-1", 1).
			WithCorrelation(corrID).
			WithSource("test:e" + suffix)
	}

	ctx := context.Background()
	if err := bus.Publish(ctx, mkEvt("1")); err != nil {
		t.Fatalf("publish 1: %v", err)
	}

	// Wait for the first dispatch to begin (handler is now sleeping).
	select {
	case <-firstSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("first dispatch never started")
	}

	// Now enqueue two more events and immediately pause.
	if err := bus.Publish(ctx, mkEvt("2")); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	if err := bus.Publish(ctx, mkEvt("3")); err != nil {
		t.Fatalf("publish 3: %v", err)
	}
	// Tiny pause so the wrap goroutines have time to enqueue.
	time.Sleep(20 * time.Millisecond)
	runner.pauser.pause(corrID)

	// Drain the first dispatch's slot — ID 1 is allowed; nothing else.
	deadline := time.After(2 * time.Second)
collect:
	for {
		select {
		case id := <-dispatched:
			if id != "trigger-1" {
				t.Fatalf("downstream dispatched %s while paused — race not fixed", id)
			}
		case <-time.After(400 * time.Millisecond):
			break collect
		case <-deadline:
			t.Fatal("test deadline exceeded waiting for first dispatch")
		}
	}

	// Items 2 and 3 must have been moved to the blocked list, not dispatched.
	runner.pauser.mu.RLock()
	blocked := len(runner.pauser.blocked[corrID])
	runner.pauser.mu.RUnlock()
	if blocked < 2 {
		t.Fatalf("expected 2 blocked dispatches for %s, got %d", corrID, blocked)
	}
}

// TestPersonaRunner_AdvisoryPause_DownstreamHandlerBlocked is the headline
// integration test for the race. It mirrors the production shape: a
// quality-gate-style handler emits [VerdictRendered{advisory}, PersonaCompleted]
// in one persistAndPublish; the engine derives WorkflowPaused from the
// verdict; a downstream handler subscribed to PersonaCompleted via DAG must
// NOT dispatch between the publish of PersonaCompleted and the engine's
// pauser.pause call.
//
// Without the fix, this test is racy and will flake; with the drain-time
// pause check + sync-ified pause subscription, the downstream handler is
// reliably blocked.
func TestPersonaRunner_AdvisoryPause_DownstreamHandlerBlocked(t *testing.T) {
	// Required includes "developer" because the gate's advisory verdict
	// targets phase "develop" → ResolvePhase → "developer". The aggregate
	// only emits WorkflowPaused on advisory if the target persona is in
	// Required; same gate guards the runner's front-run pre-pause path.
	def := WorkflowDef{
		ID:       "wf-pause-race",
		Required: []string{"developer", "gate", "committer"},
		Graph: map[string][]string{
			"gate":      nil, // root, fires on workflow.started
			"committer": {"gate"},
		},
		PhaseMap:      map[string]string{"develop": "developer"},
		MaxIterations: 3,
	}
	env := newE2EEnv(t, def)

	committerCalled := make(chan event.Envelope, 4)
	committer := &stubHandler{
		name: "committer",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, e event.Envelope) ([]event.Envelope, error) {
			committerCalled <- e
			return nil, nil
		},
	}

	// gate emits VerdictRendered{advisory,fail} — engine.aggregate will pause
	// the workflow on receipt. Returning the verdict from the handler causes
	// PersonaCompleted{gate} to be appended by PersonaRunner; both events are
	// published in the same persistAndPublish batch — exactly the race shape.
	gate := &stubHandler{
		name: "gate",
		subs: []event.Type{event.WorkflowStarted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return []event.Envelope{
				event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
					Phase:       "develop",
					SourcePhase: "gate",
					Outcome:     event.VerdictFail,
					Advisory:    true,
					Summary:     "gate cannot run — escalating",
				})),
			}, nil
		},
	}

	if err := env.reg.Register(gate); err != nil {
		t.Fatalf("register gate: %v", err)
	}
	if err := env.reg.Register(committer); err != nil {
		t.Fatalf("register committer: %v", err)
	}

	ctx := context.Background()
	env.start(ctx)

	corrID := "corr-pause-race"
	env.fireWorkflow(ctx, t, corrID, "wf-pause-race")

	// Watch for committer dispatch within the window where the race could
	// fire. 750ms is well past the engine's verdict→pause handling time,
	// and the gate's PersonaCompleted is published before any pause is set.
	select {
	case e := <-committerCalled:
		t.Fatalf("committer dispatched while workflow was paused — race not fixed (env type=%s)", e.Type)
	case <-time.After(750 * time.Millisecond):
		// Good — committer never fired.
	}

	if !env.runner.pauser.isPaused(corrID) {
		t.Fatalf("workflow %s should be paused after advisory verdict", corrID)
	}
}

// TestPersonaRunner_PauseSubIsSync is a regression guard. The pause
// subscription must use eventbus.WithSync so that pauser.pause runs in the
// publisher's goroutine — guaranteeing that by the time
// bus.Publish(WorkflowPaused) returns, the pause state is visible to the
// drain goroutine's pause check. A future refactor that drops the sync
// option would silently re-introduce the race; this test catches that.
func TestPersonaRunner_PauseSubIsSync(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t)
	runner.Start(context.Background(), reg)

	corrID := "corr-sync-test"

	// We rely on the bus's "sync subscriber runs inline" contract: if the
	// pause subscription is sync, then bus.Publish(WorkflowPaused) returns
	// only after pauser.pause has been called.
	pausedEvt := event.New(event.WorkflowPaused, 1, event.MustMarshal(event.WorkflowPausedPayload{
		Reason: "test",
	})).
		WithAggregate("wf-sync", 1).
		WithCorrelation(corrID).
		WithSource("test:sync")

	if err := bus.Publish(context.Background(), pausedEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// If the subscription is sync, the call is observable immediately.
	// If async, it may arrive later — we'd need a sleep, which is exactly
	// the race we are asserting against.
	if !runner.pauser.isPaused(corrID) {
		t.Fatal("pauser.pause was not visible immediately after Publish(WorkflowPaused) returned — pause subscription must use WithSync")
	}
}
