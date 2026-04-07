package engine

// Regression test for the develop-only deadlock observed under correlation
// 8af108dd-70eb-44c1-a7ae-44c1c6350911.
//
// Root cause: DevelopOnlyWorkflowDef was missing RetriggeredBy["developer"].
// The committer's workspaceHasChanges pre-check correctly emitted
// VerdictRendered{outcome=fail, phase=develop, source_phase=commit} when the
// developer hallucinated edits without writing any files. The aggregate
// responded correctly — it emitted FeedbackGenerated and cleared
// CompletedPersonas["developer"]. However, PersonaRunner called
// resolver.isRetriggerable("developer", ...) which checked
// def.RetriggeredBy["developer"] — nil in the old def — returned false,
// and the developer never re-fired. The workflow was permanently stuck.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// TestDevelopOnlyDeadlockRegression simulates the exact deadlock scenario:
// committer detects zero workspace changes and emits VerdictRendered{fail},
// which must cause the developer to be re-dispatched (feedback loop), not stuck.
func TestDevelopOnlyDeadlockRegression(t *testing.T) {
	def := DevelopOnlyWorkflowDef()
	env := newE2EEnv(t, def)

	const wfID = "wf-deadlock-regression"

	// devCallCount tracks how many times the developer stub fires.
	// We need it to be called at least twice: once initially, once after
	// FeedbackGenerated fires in response to the committer's VerdictFail.
	var devCallCount atomic.Int32

	// workspace: root handler, fires on WorkflowStarted.
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "workspace",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("develop-only")}},
	}); err != nil {
		t.Fatal(err)
	}

	// developer: fires on WorkflowStarted-equivalent via DAG (after workspace)
	// AND on FeedbackGenerated (the retrigger). We count each invocation.
	// On the second+ call it returns nil (simulates "now wrote actual code").
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "developer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				devCallCount.Add(1)
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			// The DAG drives dispatch, but TriggeredHandler.Trigger is the legacy
			// fallback — we set it correctly so the stub integrates with PersonaRunner.
			Events: []event.Type{event.PersonaCompleted, event.FeedbackGenerated},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// reviewer: always passes (PASS path — reviewer is not the failure source here).
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "reviewer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"developer"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// committer: on first call emits VerdictRendered{fail, phase=develop, source_phase=commit}
	// to simulate the "no workspace changes" pre-check. On subsequent calls it passes
	// (simulates developer actually writing code the second time).
	var committerCallCount atomic.Int32
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "committer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				n := committerCallCount.Add(1)
				if n == 1 {
					// First call: simulate workspaceHasChanges() == false.
					// Emit VerdictRendered targeting the develop phase so the aggregate
					// generates FeedbackGenerated and re-triggers the developer.
					return []event.Envelope{
						event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
							Phase:       "develop",
							SourcePhase: "commit",
							Outcome:     event.VerdictFail,
							Summary:     "no code changes detected in workspace",
						})),
					}, nil
				}
				// Second+ call: workspace has changes, commit succeeds.
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"reviewer"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, wfID)
	env.start(ctx)
	env.fireWorkflow(ctx, t, wfID, "develop-only")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Errorf("expected WorkflowCompleted, got %s", got.Type)
		}
		// Assert: developer was called more than once — feedback loop fired.
		if devCallCount.Load() < 2 {
			t.Errorf("developer must be called at least twice (initial + retrigger after FeedbackGenerated); got %d", devCallCount.Load())
		}
		// Assert: committer was called more than once — second pass succeeded.
		if committerCallCount.Load() < 2 {
			t.Errorf("committer must be called at least twice (fail + success); got %d", committerCallCount.Load())
		}
		t.Logf("regression passed: developer calls=%d, committer calls=%d",
			devCallCount.Load(), committerCallCount.Load())

	case <-time.After(20 * time.Second):
		// Dump workflow events for post-mortem.
		events, _ := env.store.Load(ctx, wfID)
		for _, e := range events {
			t.Logf("  event: %s (v%d, agg=%s)", e.Type, e.Version, e.AggregateID)
		}
		t.Fatalf("timeout: workflow deadlocked (developer calls=%d, committer calls=%d) — FeedbackGenerated likely not triggering developer retrigger",
			devCallCount.Load(), committerCallCount.Load())
	}
}
