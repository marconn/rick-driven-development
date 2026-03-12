package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// =============================================================================
// Feedback Loop Boundary Tests
// =============================================================================

// TestE2EFeedbackLoopMaxIterationsTerminates verifies that a permanently-failing
// reviewer causes the workflow to fail after MaxIterations rather than loop forever.
func TestE2EFeedbackLoopMaxIterationsTerminates(t *testing.T) {
	maxIter := 2
	def := WorkflowDef{
		ID: "e2e-maxiter", Required: []string{"developer", "reviewer"}, MaxIterations: maxIter, PhaseMap: corePhaseMap,
		Graph:         map[string][]string{"developer": {}, "reviewer": {"developer"}},
		RetriggeredBy: map[string][]event.Type{"developer": {event.FeedbackGenerated}},
	}
	env := newE2EEnv(t, def)

	var devRuns, reviewRuns atomic.Int32

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "developer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				devRuns.Add(1)
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events: []event.Type{event.WorkflowStartedFor("e2e-maxiter"), event.FeedbackGenerated},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "reviewer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				reviewRuns.Add(1)
				// Always fail — never passes.
				// Uses phase verbs ("develop", not "developer") to match real handler behavior.
				return []event.Envelope{
					event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
						Phase:       "develop",
						SourcePhase: "review",
						Outcome:     event.VerdictFail,
						Summary:     "code is bad",
					})),
				}, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"developer"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-maxiter")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-maxiter", "e2e-maxiter")

	select {
	case got := <-result:
		if got.Type != event.WorkflowFailed {
			t.Fatalf("expected WorkflowFailed, got %s", got.Type)
		}
		var p event.WorkflowFailedPayload
		_ = json.Unmarshal(got.Payload, &p)
		if !strings.Contains(p.Reason, "max iterations") {
			t.Errorf("expected max iterations reason, got: %s", p.Reason)
		}
		t.Logf("dev runs: %d, review runs: %d", devRuns.Load(), reviewRuns.Load())
	case <-time.After(15 * time.Second):
		t.Fatalf("timeout: feedback loop did not terminate (dev=%d, review=%d)",
			devRuns.Load(), reviewRuns.Load())
	}
}

// TestE2EFeedbackLoopConverges verifies that a reviewer that fails N-1 times
// then passes produces WorkflowCompleted.
func TestE2EFeedbackLoopConverges(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-converge", Required: []string{"developer", "reviewer"}, MaxIterations: 5, PhaseMap: corePhaseMap,
		Graph:         map[string][]string{"developer": {}, "reviewer": {"developer"}},
		RetriggeredBy: map[string][]event.Type{"developer": {event.FeedbackGenerated}},
	}
	env := newE2EEnv(t, def)

	var devRuns, reviewRuns atomic.Int32

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "developer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				devRuns.Add(1)
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events: []event.Type{event.WorkflowStartedFor("e2e-converge"), event.FeedbackGenerated},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "reviewer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				n := reviewRuns.Add(1)
				if n <= 2 {
					return []event.Envelope{
						event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
							Phase:       "develop",
							SourcePhase: "review",
							Outcome:     event.VerdictFail,
							Summary:     fmt.Sprintf("iteration %d: needs work", n),
						})),
					}, nil
				}
				// 3rd review: pass
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"developer"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-converge")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-converge", "e2e-converge")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
		if devRuns.Load() < 3 {
			t.Errorf("expected at least 3 dev runs, got %d", devRuns.Load())
		}
		if reviewRuns.Load() < 3 {
			t.Errorf("expected at least 3 review runs, got %d", reviewRuns.Load())
		}
		t.Logf("converged: dev runs=%d, review runs=%d", devRuns.Load(), reviewRuns.Load())
	case <-time.After(15 * time.Second):
		events, _ := env.store.Load(ctx, "wf-converge")
		for _, e := range events {
			t.Logf("  event: %s (v%d)", e.Type, e.Version)
		}
		t.Fatalf("timeout: feedback loop never converged (dev=%d, review=%d)",
			devRuns.Load(), reviewRuns.Load())
	}
}

// =============================================================================
// Aggregate-level feedback boundary tests (unit level)
// =============================================================================

// TestAggregateMaxIterationsEnforced verifies the aggregate emits WorkflowFailed
// when feedback exceeds MaxIterations.
func TestAggregateMaxIterationsEnforced(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning
	agg.MaxIterations = 1
	agg.WorkflowDef = &WorkflowDef{Required: []string{"developer"}, MaxIterations: 1, PhaseMap: corePhaseMap}
	agg.FeedbackCount["developer"] = 1 // already at max

	events, err := agg.Decide(event.Envelope{
		Type: event.VerdictRendered, AggregateID: "wf-1", CorrelationID: "corr-1",
		Payload: event.MustMarshal(event.VerdictPayload{
			Phase: "develop", Outcome: event.VerdictFail, Summary: "still bad",
		}),
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if len(events) != 1 || events[0].Type != event.WorkflowFailed {
		t.Fatalf("expected [WorkflowFailed], got %v", events)
	}

	var p event.WorkflowFailedPayload
	_ = json.Unmarshal(events[0].Payload, &p)
	if !strings.Contains(p.Reason, "max iterations (1) reached") {
		t.Errorf("unexpected reason: %s", p.Reason)
	}
}

// TestAggregateFeedbackCountTrackingAcrossIterations verifies feedback count
// increments correctly across multiple iterations.
func TestAggregateFeedbackCountTrackingAcrossIterations(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning
	agg.MaxIterations = 5

	for i := range 3 {
		// Each feedback generation should increment the count
		agg.Apply(event.Envelope{
			Version: i + 1, Type: event.FeedbackGenerated,
			Payload: event.MustMarshal(event.FeedbackGeneratedPayload{
				TargetPhase: "developer",
				SourcePhase: "reviewer",
				Iteration:   i + 1,
			}),
		})
	}

	if agg.FeedbackCount["developer"] != 3 {
		t.Errorf("expected FeedbackCount=3, got %d", agg.FeedbackCount["developer"])
	}
}

// TestAggregateFeedbackPendingGatesStaleEvents verifies the stale event guard
// prevents premature re-tracking of cleared personas.
func TestAggregateFeedbackPendingGatesStaleEvents(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.CompletedPersonas["developer"] = true
	agg.CompletedPersonas["reviewer"] = true

	// Feedback clears developer+reviewer, gates reviewer→developer
	agg.Apply(event.Envelope{
		Version: 1, Type: event.FeedbackGenerated,
		Payload: event.MustMarshal(event.FeedbackGeneratedPayload{
			TargetPhase: "developer",
			SourcePhase: "reviewer",
		}),
	})

	// reviewer is stale because developer hasn't re-completed
	if !agg.isStaleAfterFeedback("reviewer") {
		t.Error("reviewer should be stale: developer hasn't re-completed yet")
	}

	// developer re-completes
	agg.Apply(event.Envelope{
		Version: 2, Type: event.PersonaCompleted,
		Payload: event.MustMarshal(event.PersonaCompletedPayload{Persona: "developer"}),
	})

	// reviewer is no longer stale
	if agg.isStaleAfterFeedback("reviewer") {
		t.Error("reviewer should NOT be stale: developer has re-completed")
	}
}

// =============================================================================
// Operator Intervention Gap Tests
// =============================================================================

// TestWorkflowCancelledEventExists verifies the event type and aggregate Apply
// exist but demonstrates no mechanism to trigger cancellation.
func TestWorkflowCancelledEventExists(t *testing.T) {
	// The event type is defined
	if event.WorkflowCancelled != "workflow.cancelled" {
		t.Errorf("WorkflowCancelled should be 'workflow.cancelled', got %s", event.WorkflowCancelled)
	}

	// The aggregate handles it correctly
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning
	agg.Apply(event.Envelope{Version: 1, Type: event.WorkflowCancelled})
	if agg.Status != StatusCancelled {
		t.Errorf("expected cancelled, got %s", agg.Status)
	}
}

// TestNoDecideHandlerForCancellation demonstrates that the aggregate's Decide
// method has no handler for WorkflowCancelled — it can only be applied externally.
// This exposes the operator intervention gap.
func TestNoDecideHandlerForCancellation(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning

	// Decide does nothing with a WorkflowCancelled event (returns nil, nil)
	events, err := agg.Decide(event.Envelope{
		Type:    event.WorkflowCancelled,
		Payload: nil,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("Decide produces no events for WorkflowCancelled: got %d events", len(events))
	}
	// WorkflowCancelled is consumed by Apply only — there's no decision path
	// to EMIT a cancel from within the system. An operator would need to:
	// 1. Create a WorkflowCancelled event
	// 2. Append it to the aggregate
	// 3. Publish it to the bus
	// But no CLI command, MCP tool, or API does this today.
}

// TestEngineIgnoresEventsAfterTerminalState verifies that once a workflow is in
// a terminal state (completed/failed/cancelled), further PersonaCompleted events
// are no-ops.
func TestEngineIgnoresEventsAfterTerminalState(t *testing.T) {
	for _, terminal := range []WorkflowStatus{StatusCompleted, StatusFailed, StatusCancelled} {
		t.Run(string(terminal), func(t *testing.T) {
			agg := NewWorkflowAggregate("wf-1")
			agg.Status = terminal
			agg.WorkflowDef = &WorkflowDef{Required: []string{"developer"}}
			agg.CompletedPersonas["developer"] = true

			events, err := agg.Decide(event.Envelope{
				Type:    event.PersonaCompleted,
				Payload: event.MustMarshal(event.PersonaCompletedPayload{Persona: "developer"}),
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(events) != 0 {
				t.Errorf("terminal state %s should produce no events, got %d", terminal, len(events))
			}
		})
	}
}

// =============================================================================
// Context Snapshot in Workflow E2E
// =============================================================================

// TestE2EContextSnapshotInChain verifies that a context-snapshot handler's events
// are visible to downstream personas via the correlation chain.
func TestE2EContextSnapshotInChain(t *testing.T) {
	def := WorkflowDef{
		ID:            "e2e-ctx-snap",
		Required:      []string{"workspace", "context-snapshot", "developer"},
		MaxIterations: 3,
		Graph: map[string][]string{
			"workspace":        {},
			"context-snapshot": {"workspace"},
			"developer":        {"context-snapshot"},
		},
	}
	env := newE2EEnv(t, def)

	// workspace: emits WorkspaceReady
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "workspace",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-ctx-snap")}},
	}); err != nil {
		t.Fatal(err)
	}

	// context-snapshot: emits ContextCodebase event
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "context-snapshot",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				return []event.Envelope{
					event.New(event.ContextCodebase, 1, event.MustMarshal(event.ContextCodebasePayload{
						Language:  "go",
						Framework: "grpc",
						Tree: []event.FileEntry{
							{Path: "main.go", Size: 100, Language: "go"},
							{Path: "go.mod", Size: 50},
						},
						Files: []event.FileSnap{
							{Path: "main.go", Content: "package main\nfunc main() {}\n"},
						},
					})),
				}, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"workspace"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// developer: verifies it can see context-snapshot events
	var developerSawContext atomic.Bool
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "developer",
			handle: func(ctx context.Context, triggerEnv event.Envelope) ([]event.Envelope, error) {
				// Load correlation chain to check for context events
				events, err := env.store.LoadByCorrelation(ctx, triggerEnv.CorrelationID)
				if err != nil {
					return nil, err
				}
				for _, e := range events {
					if e.Type == event.ContextCodebase {
						developerSawContext.Store(true)
					}
				}
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"context-snapshot"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-ctx-snap")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-ctx-snap", "e2e-ctx-snap")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(5 * time.Second):
		events, _ := env.store.Load(ctx, "wf-ctx-snap")
		for _, e := range events {
			t.Logf("  event: %s (v%d, agg=%s)", e.Type, e.Version, e.AggregateID)
		}
		corr, _ := env.store.LoadByCorrelation(ctx, "wf-ctx-snap")
		t.Logf("  --- correlation events ---")
		for _, e := range corr {
			t.Logf("  event: %s (agg=%s)", e.Type, e.AggregateID)
		}
		t.Fatal("timeout: context-snapshot chain never completed")
	}

	if !developerSawContext.Load() {
		t.Error("developer should have seen ContextCodebase event in correlation chain")
	}
}

// TestE2EContextSnapshotWithFeedbackLoop verifies that context-snapshot events
// persist and remain visible across feedback iterations. Context-snapshot runs
// once (after workspace), and its events stay in the correlation chain for all
// subsequent develop→review cycles.
func TestE2EContextSnapshotWithFeedbackLoop(t *testing.T) {
	def := WorkflowDef{
		ID:            "e2e-ctx-feedback",
		Required:      []string{"workspace", "context-snapshot", "developer", "reviewer"},
		MaxIterations: 5,
		Graph: map[string][]string{
			"workspace":        {},
			"context-snapshot": {"workspace"},
			"developer":        {"context-snapshot"},
			"reviewer":         {"developer"},
		},
		RetriggeredBy: map[string][]event.Type{"developer": {event.FeedbackGenerated}},
	}
	env := newE2EEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "workspace",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-ctx-feedback")}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "context-snapshot",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				return []event.Envelope{
					event.New(event.ContextCodebase, 1, event.MustMarshal(event.ContextCodebasePayload{
						Language: "go",
						Tree:     []event.FileEntry{{Path: "main.go", Size: 100}},
					})),
				}, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"workspace"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var devContextChecks atomic.Int32
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "developer",
			handle: func(ctx context.Context, triggerEnv event.Envelope) ([]event.Envelope, error) {
				events, _ := env.store.LoadByCorrelation(ctx, triggerEnv.CorrelationID)
				for _, e := range events {
					if e.Type == event.ContextCodebase {
						devContextChecks.Add(1)
					}
				}
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted, event.FeedbackGenerated},
			AfterPersonas: []string{"context-snapshot"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var reviewCount atomic.Int32
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "reviewer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				n := reviewCount.Add(1)
				if n == 1 {
					return []event.Envelope{
						event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
							Phase:       "developer",
							SourcePhase: "reviewer",
							Outcome:     event.VerdictFail,
							Summary:     "needs work",
						})),
					}, nil
				}
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"developer"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-ctx-feedback")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-ctx-feedback", "e2e-ctx-feedback")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timeout: context-snapshot feedback loop never completed")
	}

	// Developer should have seen the context-snapshot events on EVERY iteration
	if devContextChecks.Load() < 2 {
		t.Errorf("expected developer to see context events on multiple iterations, saw %d", devContextChecks.Load())
	}
}

// =============================================================================
// Token Budget Enforcement
// =============================================================================

// =============================================================================
// E2E Pause/Resume Tests
// =============================================================================

// TestE2EPauseBlocksNewDispatches verifies that pausing a workflow prevents
// new persona dispatches while allowing in-flight handlers to complete.
func TestE2EPauseBlocksNewDispatches(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-pause", Required: []string{"alpha", "beta"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}, "beta": {"alpha"}},
	}
	env := newE2EEnv(t, def)

	alphaStarted := make(chan struct{})
	alphaRelease := make(chan struct{})
	var betaFired atomic.Bool

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				close(alphaStarted)
				<-alphaRelease // block until test says go
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-pause")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "beta",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				betaFired.Store(true)
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"alpha"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-pause", "e2e-pause")

	// Wait for alpha to start
	select {
	case <-alphaStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("alpha never started")
	}

	// Pause the workflow while alpha is in-flight
	pauseEvt := event.New(event.WorkflowPaused, 1, event.MustMarshal(event.WorkflowPausedPayload{
		Reason: "test pause",
		Source: "test",
	})).
		WithAggregate("wf-pause", 3). // version after WorkflowRequested(1) + WorkflowStarted(2)
		WithCorrelation("wf-pause").
		WithSource("test:pause")

	if err := env.store.Append(ctx, "wf-pause", 2, []event.Envelope{pauseEvt}); err != nil {
		t.Fatalf("append pause: %v", err)
	}
	if err := env.bus.Publish(ctx, pauseEvt); err != nil {
		t.Fatalf("publish pause: %v", err)
	}

	// Give pause time to propagate
	time.Sleep(100 * time.Millisecond)

	// Release alpha — it completes, PersonaCompleted is published
	close(alphaRelease)

	// Beta should NOT fire because workflow is paused
	time.Sleep(500 * time.Millisecond)
	if betaFired.Load() {
		t.Error("beta should not fire while workflow is paused")
	}
}

// TestE2EPauseResumeReplaysBlocked verifies that resuming a paused workflow
// replays blocked handler dispatches.
func TestE2EPauseResumeReplaysBlocked(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-resume", Required: []string{"alpha", "beta"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}, "beta": {"alpha"}},
	}
	env := newE2EEnv(t, def)

	alphaStarted := make(chan struct{})
	alphaRelease := make(chan struct{})

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				close(alphaStarted)
				<-alphaRelease
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-resume")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "beta",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"alpha"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-resume")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-resume", "e2e-resume")

	// Wait for alpha to start, then pause
	<-alphaStarted

	pauseEvt := event.New(event.WorkflowPaused, 1, event.MustMarshal(event.WorkflowPausedPayload{
		Reason: "test", Source: "test",
	})).WithAggregate("wf-resume", 3).WithCorrelation("wf-resume").WithSource("test")

	if err := env.store.Append(ctx, "wf-resume", 2, []event.Envelope{pauseEvt}); err != nil {
		t.Fatalf("append pause: %v", err)
	}
	if err := env.bus.Publish(ctx, pauseEvt); err != nil {
		t.Fatalf("publish pause: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Release alpha — its PersonaCompleted arrives while paused, beta is blocked
	close(alphaRelease)
	time.Sleep(500 * time.Millisecond)

	// Now resume — beta should fire from the blocked buffer
	// Need current version after pause + alpha tracking events
	events, _ := env.store.Load(ctx, "wf-resume")
	currentVersion := events[len(events)-1].Version

	resumeEvt := event.New(event.WorkflowResumed, 1, event.MustMarshal(event.WorkflowResumedPayload{
		Reason: "test resume",
	})).WithAggregate("wf-resume", currentVersion+1).WithCorrelation("wf-resume").WithSource("test")

	if err := env.store.Append(ctx, "wf-resume", currentVersion, []event.Envelope{resumeEvt}); err != nil {
		t.Fatalf("append resume: %v", err)
	}
	if err := env.bus.Publish(ctx, resumeEvt); err != nil {
		t.Fatalf("publish resume: %v", err)
	}

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted after resume, got %s", got.Type)
		}
	case <-time.After(5 * time.Second):
		events, _ := env.store.Load(ctx, "wf-resume")
		for _, e := range events {
			t.Logf("  event: %s (v%d, agg=%s)", e.Type, e.Version, e.AggregateID)
		}
		corr, _ := env.store.LoadByCorrelation(ctx, "wf-resume")
		t.Logf("  --- correlation ---")
		for _, e := range corr {
			t.Logf("  event: %s (agg=%s)", e.Type, e.AggregateID)
		}
		t.Fatal("timeout: pause+resume never completed workflow")
	}
}

// TestE2ECancelStopsWorkflow verifies that cancelling a workflow prevents
// new personas and results in no WorkflowCompleted.
func TestE2ECancelStopsWorkflow(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-cancel", Required: []string{"alpha", "beta"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}, "beta": {"alpha"}},
	}
	env := newE2EEnv(t, def)

	alphaStarted := make(chan struct{})
	alphaRelease := make(chan struct{})
	var betaFired atomic.Bool

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				close(alphaStarted)
				<-alphaRelease
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-cancel")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "beta",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				betaFired.Store(true)
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"alpha"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-cancel", "e2e-cancel")

	// Wait for alpha to start
	<-alphaStarted

	// Cancel the workflow
	cancelEvt := event.New(event.WorkflowCancelled, 1, event.MustMarshal(event.WorkflowCancelledPayload{
		Reason: "test cancel",
		Source: "test",
	})).WithAggregate("wf-cancel", 3).WithCorrelation("wf-cancel").WithSource("test")

	if err := env.store.Append(ctx, "wf-cancel", 2, []event.Envelope{cancelEvt}); err != nil {
		t.Fatalf("append cancel: %v", err)
	}
	if err := env.bus.Publish(ctx, cancelEvt); err != nil {
		t.Fatalf("publish cancel: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Release alpha
	close(alphaRelease)

	// Give system time to process
	time.Sleep(1 * time.Second)

	// Beta should never fire
	if betaFired.Load() {
		t.Error("beta should not fire after cancel")
	}

	// Verify workflow status is cancelled
	events, _ := env.store.Load(ctx, "wf-cancel")
	agg := NewWorkflowAggregate("wf-cancel")
	for _, e := range events {
		agg.Apply(e)
	}
	if agg.Status != StatusCancelled {
		t.Errorf("expected cancelled, got %s", agg.Status)
	}
}

// =============================================================================
// Aggregate Pause/Resume Unit Tests
// =============================================================================

func TestAggregatePauseResumeCycle(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning

	agg.Apply(event.Envelope{Version: 1, Type: event.WorkflowPaused})
	if agg.Status != StatusPaused {
		t.Errorf("expected paused, got %s", agg.Status)
	}

	agg.Apply(event.Envelope{Version: 2, Type: event.WorkflowResumed})
	if agg.Status != StatusRunning {
		t.Errorf("expected running after resume, got %s", agg.Status)
	}
}

func TestAggregateDecideNoFeedbackWhilePaused(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusPaused
	agg.MaxIterations = 5

	events, err := agg.Decide(event.Envelope{
		Type: event.VerdictRendered,
		Payload: event.MustMarshal(event.VerdictPayload{
			Phase: "develop", Outcome: event.VerdictFail, Summary: "bad",
		}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("should produce no events while paused, got %d", len(events))
	}
}

func TestAggregateDecideNoCompletionWhilePaused(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusPaused
	agg.WorkflowDef = &WorkflowDef{Required: []string{"developer"}}
	agg.CompletedPersonas["developer"] = true

	events, err := agg.Decide(event.Envelope{
		Type:    event.PersonaCompleted,
		Payload: event.MustMarshal(event.PersonaCompletedPayload{Persona: "developer"}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("should produce no WorkflowCompleted while paused, got %d", len(events))
	}
}

// =============================================================================
// Auto-Escalation (Phase 4)
// =============================================================================

// TestAggregateEscalateOnMaxIterPausesInsteadOfFail verifies that when
// EscalateOnMaxIter is set, the aggregate emits WorkflowPaused instead of
// WorkflowFailed when max iterations are reached.
func TestAggregateEscalateOnMaxIterPausesInsteadOfFail(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning
	agg.MaxIterations = 1
	agg.FeedbackCount["developer"] = 1 // at max
	agg.WorkflowDef = &WorkflowDef{
		Required:          []string{"developer", "reviewer"},
		MaxIterations:     1,
		EscalateOnMaxIter: true,
		PhaseMap:          corePhaseMap,
	}

	events, err := agg.Decide(event.Envelope{
		Type: event.VerdictRendered, AggregateID: "wf-1", CorrelationID: "corr-1",
		Payload: event.MustMarshal(event.VerdictPayload{
			Phase: "develop", Outcome: event.VerdictFail, Summary: "still bad",
		}),
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != event.WorkflowPaused {
		t.Fatalf("expected WorkflowPaused (escalation), got %s", events[0].Type)
	}

	var p event.WorkflowPausedPayload
	_ = json.Unmarshal(events[0].Payload, &p)
	if !strings.Contains(p.Reason, "escalated to operator") {
		t.Errorf("expected escalation reason, got: %s", p.Reason)
	}
	if p.Source != "engine:auto-escalation" {
		t.Errorf("expected source engine:auto-escalation, got: %s", p.Source)
	}
}

// TestAggregateEscalateOffStillFails verifies that without EscalateOnMaxIter,
// max iterations still produces WorkflowFailed (backwards compatibility).
func TestAggregateEscalateOffStillFails(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning
	agg.MaxIterations = 1
	agg.FeedbackCount["developer"] = 1
	agg.WorkflowDef = &WorkflowDef{
		Required:          []string{"developer", "reviewer"},
		MaxIterations:     1,
		EscalateOnMaxIter: false,
		PhaseMap:          corePhaseMap,
	}

	events, err := agg.Decide(event.Envelope{
		Type: event.VerdictRendered, AggregateID: "wf-1", CorrelationID: "corr-1",
		Payload: event.MustMarshal(event.VerdictPayload{
			Phase: "develop", Outcome: event.VerdictFail, Summary: "still bad",
		}),
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if len(events) != 1 || events[0].Type != event.WorkflowFailed {
		t.Fatalf("expected WorkflowFailed, got %v", events)
	}
}

// TestE2EAutoEscalationPausesThenResumes verifies the full auto-escalation flow:
// reviewer always fails → max iterations → WorkflowPaused → operator resumes → completes.
func TestE2EAutoEscalationPausesThenResumes(t *testing.T) {
	def := WorkflowDef{
		ID:                "e2e-escalate",
		Required:          []string{"developer", "reviewer"},
		MaxIterations:     1,
		EscalateOnMaxIter: true,
		PhaseMap:          corePhaseMap,
		Graph:             map[string][]string{"developer": {}, "reviewer": {"developer"}},
		RetriggeredBy:     map[string][]event.Type{"developer": {event.FeedbackGenerated}},
	}
	env := newE2EEnv(t, def)

	var devRuns, reviewRuns atomic.Int32

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "developer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				devRuns.Add(1)
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events: []event.Type{event.WorkflowStartedFor("e2e-escalate"), event.FeedbackGenerated},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "reviewer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				n := reviewRuns.Add(1)
				if n <= 2 {
					// First 2 reviews: fail.
					// Uses phase verbs ("develop", not "developer") to match real handler behavior.
					return []event.Envelope{
						event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
							Phase:       "develop",
							SourcePhase: "review",
							Outcome:     event.VerdictFail,
							Summary:     fmt.Sprintf("iteration %d: bad", n),
						})),
					}, nil
				}
				// 3rd review (after resume): pass
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"developer"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// Subscribe to WorkflowPaused to detect escalation
	paused := make(chan event.Envelope, 1)
	unsubPause := env.bus.Subscribe(event.WorkflowPaused, func(_ context.Context, e event.Envelope) error {
		if e.CorrelationID == "wf-escalate" {
			select {
			case paused <- e:
			default:
			}
		}
		return nil
	}, eventbus.WithName("test:pause-detect"))
	defer unsubPause()

	result := awaitWorkflowResult(t, env.bus, "wf-escalate")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-escalate", "e2e-escalate")

	// Wait for auto-escalation pause
	select {
	case pauseEvt := <-paused:
		var p event.WorkflowPausedPayload
		_ = json.Unmarshal(pauseEvt.Payload, &p)
		if !strings.Contains(p.Reason, "escalated to operator") {
			t.Errorf("expected escalation reason, got: %s", p.Reason)
		}
		t.Logf("auto-escalation triggered: %s", p.Reason)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: auto-escalation never triggered")
	}

	// Give PersonaRunner time to process the pause
	time.Sleep(200 * time.Millisecond)

	// Operator resumes — bump MaxIterations so the feedback loop can continue
	events, _ := env.store.Load(ctx, "wf-escalate")
	currentVersion := events[len(events)-1].Version

	resumeEvt := event.New(event.WorkflowResumed, 1, event.MustMarshal(event.WorkflowResumedPayload{
		Reason: "operator intervention: increasing max iterations",
	})).WithAggregate("wf-escalate", currentVersion+1).
		WithCorrelation("wf-escalate").
		WithSource("test:operator")

	if err := env.store.Append(ctx, "wf-escalate", currentVersion, []event.Envelope{resumeEvt}); err != nil {
		t.Fatalf("append resume: %v", err)
	}
	if err := env.bus.Publish(ctx, resumeEvt); err != nil {
		t.Fatalf("publish resume: %v", err)
	}

	// After resume, decideWorkflowResumed bumps MaxIterations and re-emits
	// FeedbackGenerated. The developer re-fires, reviewer runs a 3rd time
	// (passes), and the workflow completes.
	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted after resume, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timeout: workflow never completed after resume (dev=%d, review=%d)",
			devRuns.Load(), reviewRuns.Load())
	}

	t.Logf("dev runs=%d, review runs=%d", devRuns.Load(), reviewRuns.Load())
}

// TestWorkspaceDevWorkflowDefHasEscalation verifies the workspace-dev workflow
// has EscalateOnMaxIter enabled by default.
func TestWorkspaceDevWorkflowDefHasEscalation(t *testing.T) {
	def := WorkspaceDevWorkflowDef()
	if !def.EscalateOnMaxIter {
		t.Error("workspace-dev should have EscalateOnMaxIter enabled")
	}
}

// =============================================================================
// Token Budget Enforcement
// =============================================================================

// =============================================================================
// Feedback Loop with Parallel Re-Fire
// =============================================================================

// TestE2EFeedbackParallelRefire verifies the trickiest real-world feedback path:
//
//	developer → reviewer + qa + documenter (parallel fan-out)
//	         → reviewer fails → FeedbackGenerated
//	         → developer re-triggers
//	         → reviewer + qa + documenter ALL re-fire
//	         → reviewer passes → committer (join: reviewer+qa) → WorkflowCompleted
//
// This tests that:
// 1. Parallel personas re-fire correctly after feedback resets the chain
// 2. The join-gate dedup allows committer to fire in round 2 (different fingerprint)
// 3. Iteration counts are correct across all personas
func TestE2EFeedbackParallelRefire(t *testing.T) {
	def := WorkflowDef{
		ID:            "e2e-parallel-refire",
		Required:      []string{"developer", "reviewer", "qa", "committer"},
		MaxIterations: 3,
		Graph: map[string][]string{
			"developer":  {},
			"reviewer":   {"developer"},
			"qa":         {"developer"},
			"documenter": {"developer"},
			"committer":  {"reviewer", "qa"},
		},
		RetriggeredBy: map[string][]event.Type{"developer": {event.FeedbackGenerated}},
	}
	env := newE2EEnv(t, def)

	var devRuns, reviewerRuns, qaRuns, committerRuns, documenterRuns atomic.Int32

	// developer: triggered by WorkflowStarted and FeedbackGenerated
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "developer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				devRuns.Add(1)
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events: []event.Type{event.WorkflowStartedFor("e2e-parallel-refire"), event.FeedbackGenerated},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// reviewer: after developer. Fails first time, passes second.
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "reviewer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				n := reviewerRuns.Add(1)
				if n == 1 {
					return []event.Envelope{
						event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
							Phase:       "developer",
							SourcePhase: "reviewer",
							Outcome:     event.VerdictFail,
							Summary:     "needs error handling",
							Issues:      []event.Issue{{Severity: "major", Category: "correctness", Description: "missing error handling"}},
						})),
					}, nil
				}
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"developer"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// qa: after developer (parallel with reviewer)
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "qa",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				qaRuns.Add(1)
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"developer"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// documenter: after developer (parallel, NOT required — fire-and-forget)
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "documenter",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				documenterRuns.Add(1)
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"developer"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// committer: join gate on reviewer + qa
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "committer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				committerRuns.Add(1)
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"reviewer", "qa"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-parallel-refire")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-parallel-refire", "e2e-parallel-refire")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(15 * time.Second):
		events, _ := env.store.Load(ctx, "wf-parallel-refire")
		for _, e := range events {
			t.Logf("  event: %s (v%d)", e.Type, e.Version)
		}
		t.Fatalf("timeout: parallel refire never completed (dev=%d, reviewer=%d, qa=%d, committer=%d, documenter=%d)",
			devRuns.Load(), reviewerRuns.Load(), qaRuns.Load(), committerRuns.Load(), documenterRuns.Load())
	}

	// Let in-flight async handlers finish before asserting counts.
	time.Sleep(200 * time.Millisecond)

	// developer: exactly 2 runs (initial + after feedback)
	if d := devRuns.Load(); d != 2 {
		t.Errorf("developer: expected 2 runs, got %d", d)
	}
	// reviewer: exactly 2 runs (fail + pass)
	if r := reviewerRuns.Load(); r != 2 {
		t.Errorf("reviewer: expected 2 runs, got %d", r)
	}
	// qa: at least 2 runs (initial + after developer re-completes)
	if q := qaRuns.Load(); q < 2 {
		t.Errorf("qa: expected at least 2 runs, got %d", q)
	}
	// documenter: at least 2 runs (initial + after developer re-completes).
	// Fire-and-forget — not required for completion.
	if d := documenterRuns.Load(); d < 2 {
		t.Errorf("documenter: expected at least 2 runs, got %d", d)
	}
	// committer: fires once per join-round where reviewer+qa both completed.
	// Round 1: reviewer+qa complete → committer fires (even though reviewer
	// issued FAIL — PersonaRunner is decoupled from verdict semantics).
	// Round 2: reviewer+qa re-complete → committer fires again.
	// The Engine's feedback-aware tracking prevents premature WorkflowCompleted.
	if c := committerRuns.Load(); c < 1 {
		t.Errorf("committer: expected at least 1 run, got %d", c)
	}

	t.Logf("final counts: dev=%d, reviewer=%d, qa=%d, documenter=%d, committer=%d",
		devRuns.Load(), reviewerRuns.Load(), qaRuns.Load(), committerRuns.Load(), documenterRuns.Load())
}

// TestAggregateTokenBudgetTerminatesWorkflow verifies that exceeding the token
// budget produces WorkflowFailed.
func TestAggregateTokenBudgetTerminatesWorkflow(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning
	agg.TokenBudget = 1000
	agg.TokensUsed = 1200

	events, err := agg.Decide(event.Envelope{
		ID: "budget-1", Type: event.TokenBudgetExceeded,
		AggregateID: "wf-1", CorrelationID: "corr-1",
		Payload: event.MustMarshal(event.TokenBudgetExceededPayload{TotalUsed: 1200, Budget: 1000}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 || events[0].Type != event.WorkflowFailed {
		t.Fatalf("expected [WorkflowFailed], got %v", events)
	}

	var p event.WorkflowFailedPayload
	_ = json.Unmarshal(events[0].Payload, &p)
	if !strings.Contains(p.Reason, "token budget") {
		t.Errorf("expected token budget reason, got: %s", p.Reason)
	}
}
