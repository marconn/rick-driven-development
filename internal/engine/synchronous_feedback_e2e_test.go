package engine

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// TestE2ESynchronousFeedback_OneRoundMergedRetrigger is the headline behavior
// proof: when BOTH reviewer and qa fail against the same developer iteration,
// the synchronization barrier emits exactly ONE FeedbackGenerated and the
// developer fires exactly TWICE (initial + one retrigger). Without the barrier
// the existing code emits two FeedbackGenerated events (one per failing
// reviewer) and the developer fires three times — that's the velocity bug
// this change fixes.
func TestE2ESynchronousFeedback_OneRoundMergedRetrigger(t *testing.T) {
	def := WorkflowDef{
		ID:                    "e2e-sync-fb",
		Required:              []string{"developer", "reviewer", "qa", "review-consolidator", "committer"},
		MaxIterations:         3,
		EscalateOnMaxIter:     true,
		SynchronousFeedback:   true,
		ConsolidatedReviewers: []string{"reviewer", "qa"},
		ReviewConsolidator:    "review-consolidator",
		Graph: map[string][]string{
			"developer":           {},
			"reviewer":            {"developer"},
			"qa":                  {"developer"},
			"review-consolidator": {"reviewer", "qa"},
			"committer":           {"review-consolidator"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
	}
	env := newE2EEnv(t, def)
	ctx := context.Background()

	var devCalls atomic.Int32
	var revCalls atomic.Int32
	var qaCalls atomic.Int32
	var committerCalls atomic.Int32

	_ = env.reg.Register(&stubHandler{
		name: "developer",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			devCalls.Add(1)
			return nil, nil
		},
	})

	_ = env.reg.Register(&stubHandler{
		name: "reviewer",
		handle: func(_ context.Context, trig event.Envelope) ([]event.Envelope, error) {
			call := revCalls.Add(1)
			outcome := event.VerdictPass
			summary := "passed review"
			var issues []event.Issue
			if call == 1 {
				outcome = event.VerdictFail
				summary = "reviewer says fail (round 1)"
				issues = []event.Issue{
					{Severity: "major", Category: "correctness", Description: "missing nil check"},
				}
			}
			return []event.Envelope{
				event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
					Persona:       "developer",
					SourcePersona: "reviewer",
					Outcome:       outcome,
					Summary:       summary,
					Issues:        issues,
					DevTriggerID:  string(trig.ID),
				})),
			}, nil
		},
	})

	_ = env.reg.Register(&stubHandler{
		name: "qa",
		handle: func(_ context.Context, trig event.Envelope) ([]event.Envelope, error) {
			call := qaCalls.Add(1)
			outcome := event.VerdictPass
			summary := "passed review"
			var issues []event.Issue
			if call == 1 {
				outcome = event.VerdictFail
				summary = "qa says fail (round 1)"
				issues = []event.Issue{
					{Severity: "minor", Category: "testing", Description: "edge case untested"},
				}
			}
			return []event.Envelope{
				event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
					Persona:       "developer",
					SourcePersona: "qa",
					Outcome:       outcome,
					Summary:       summary,
					Issues:        issues,
					DevTriggerID:  string(trig.ID),
				})),
			}, nil
		},
	})

	// Real consolidator handler — the unit under test.
	_ = env.reg.Register(handler.NewReviewConsolidator(handler.ReviewConsolidatorConfig{
		Reviewers:     []string{"reviewer", "qa"},
		TargetPersona: "developer",
		Store:         env.store,
	}))

	_ = env.reg.Register(&stubHandler{
		name: "committer",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			committerCalls.Add(1)
			return nil, nil
		},
	})

	// Count FeedbackGenerated events emitted during the run — the barrier
	// invariant is "exactly one per failing round."
	var feedbackEvents atomic.Int32
	unsub := env.bus.Subscribe(event.FeedbackGenerated, func(_ context.Context, e event.Envelope) error {
		if e.CorrelationID == "wf-sync-fb" {
			feedbackEvents.Add(1)
		}
		return nil
	}, eventbus.WithName("e2e:feedback-counter"))
	t.Cleanup(unsub)

	result := awaitWorkflowResult(t, env.bus, "wf-sync-fb")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-sync-fb", "e2e-sync-fb")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		dumpCorrelation(t, ctx, env, "wf-sync-fb")
		t.Fatal("timeout: workflow did not complete")
	}

	// Allow trailing PersonaCompleted events to drain so committerCalls counts
	// the second iteration correctly.
	time.Sleep(100 * time.Millisecond)

	if got := devCalls.Load(); got != 2 {
		t.Errorf("developer fires = %d, want 2 (initial + one merged retrigger)", got)
	}
	if got := revCalls.Load(); got != 2 {
		t.Errorf("reviewer fires = %d, want 2 (one per round)", got)
	}
	if got := qaCalls.Load(); got != 2 {
		t.Errorf("qa fires = %d, want 2 (one per round)", got)
	}
	if got := feedbackEvents.Load(); got != 1 {
		t.Errorf("FeedbackGenerated count = %d, want 1 (consolidator merges both fails)", got)
	}
	if got := committerCalls.Load(); got != 1 {
		t.Errorf("committer fires = %d, want 1", got)
	}
}

// TestE2ESynchronousFeedback_AllPassNoFeedback verifies the happy path:
// when both reviewer and qa pass on the first round, the consolidator emits
// no FeedbackGenerated, the developer fires exactly once, and the workflow
// proceeds straight to committer.
func TestE2ESynchronousFeedback_AllPassNoFeedback(t *testing.T) {
	def := WorkflowDef{
		ID:                    "e2e-sync-fb-pass",
		Required:              []string{"developer", "reviewer", "qa", "review-consolidator", "committer"},
		MaxIterations:         3,
		EscalateOnMaxIter:     true,
		SynchronousFeedback:   true,
		ConsolidatedReviewers: []string{"reviewer", "qa"},
		ReviewConsolidator:    "review-consolidator",
		Graph: map[string][]string{
			"developer":           {},
			"reviewer":            {"developer"},
			"qa":                  {"developer"},
			"review-consolidator": {"reviewer", "qa"},
			"committer":           {"review-consolidator"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
	}
	env := newE2EEnv(t, def)
	ctx := context.Background()

	var devCalls atomic.Int32

	_ = env.reg.Register(&stubHandler{
		name: "developer",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			devCalls.Add(1)
			return nil, nil
		},
	})

	passVerdict := func(source string) func(context.Context, event.Envelope) ([]event.Envelope, error) {
		return func(_ context.Context, trig event.Envelope) ([]event.Envelope, error) {
			return []event.Envelope{
				event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
					Persona:       "developer",
					SourcePersona: source,
					Outcome:       event.VerdictPass,
					Summary:       "passed review",
					DevTriggerID:  string(trig.ID),
				})),
			}, nil
		}
	}
	_ = env.reg.Register(&stubHandler{name: "reviewer", handle: passVerdict("reviewer")})
	_ = env.reg.Register(&stubHandler{name: "qa", handle: passVerdict("qa")})
	_ = env.reg.Register(handler.NewReviewConsolidator(handler.ReviewConsolidatorConfig{
		Reviewers:     []string{"reviewer", "qa"},
		TargetPersona: "developer",
		Store:         env.store,
	}))
	_ = env.reg.Register(&stubHandler{
		name:   "committer",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	})

	var feedbackEvents atomic.Int32
	unsub := env.bus.Subscribe(event.FeedbackGenerated, func(_ context.Context, e event.Envelope) error {
		if e.CorrelationID == "wf-sync-fb-pass" {
			feedbackEvents.Add(1)
		}
		return nil
	}, eventbus.WithName("e2e:feedback-counter"))
	t.Cleanup(unsub)

	result := awaitWorkflowResult(t, env.bus, "wf-sync-fb-pass")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-sync-fb-pass", "e2e-sync-fb-pass")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		dumpCorrelation(t, ctx, env, "wf-sync-fb-pass")
		t.Fatal("timeout: happy-path workflow did not complete")
	}

	if got := devCalls.Load(); got != 1 {
		t.Errorf("developer fires = %d, want 1 (no feedback round)", got)
	}
	if got := feedbackEvents.Load(); got != 0 {
		t.Errorf("FeedbackGenerated count = %d, want 0 (all-pass round)", got)
	}
}

// TestE2ESynchronousFeedback_LegacyPathParityRegression locks in the legacy
// behavior: when SynchronousFeedback is OFF, the existing per-verdict feedback
// path still emits two FeedbackGenerated events (one per failing reviewer).
// This is the regression guard — if a future change accidentally generalizes
// the gating to the non-sync path, this test catches it.
//
// Test uses a streamlined topology: just developer + reviewer + qa with the
// committer joining directly on both. The point is to prove the aggregate's
// emission path is unchanged when the flag is off, NOT to test the full
// workflow shape.
func TestE2ESynchronousFeedback_LegacyPathParityRegression(t *testing.T) {
	def := WorkflowDef{
		ID:                "e2e-legacy-fb",
		Required:          []string{"developer", "reviewer", "qa", "committer"},
		MaxIterations:     5,
		EscalateOnMaxIter: true,
		// SynchronousFeedback intentionally left FALSE.
		Graph: map[string][]string{
			"developer": {},
			"reviewer":  {"developer"},
			"qa":        {"developer"},
			"committer": {"reviewer", "qa"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
	}
	env := newE2EEnv(t, def)
	ctx := context.Background()

	var revCalls, qaCalls atomic.Int32

	_ = env.reg.Register(&stubHandler{
		name:   "developer",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	})

	_ = env.reg.Register(&stubHandler{
		name: "reviewer",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			call := revCalls.Add(1)
			outcome := event.VerdictPass
			summary := "passed review"
			var issues []event.Issue
			if call == 1 {
				outcome = event.VerdictFail
				summary = "reviewer says fail"
				issues = []event.Issue{{Severity: "major", Category: "correctness", Description: "issue r1"}}
			}
			return []event.Envelope{
				event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
					Persona: "developer", SourcePersona: "reviewer",
					Outcome: outcome, Summary: summary, Issues: issues,
				})),
			}, nil
		},
	})

	_ = env.reg.Register(&stubHandler{
		name: "qa",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			call := qaCalls.Add(1)
			outcome := event.VerdictPass
			summary := "passed review"
			var issues []event.Issue
			if call == 1 {
				outcome = event.VerdictFail
				summary = "qa says fail"
				issues = []event.Issue{{Severity: "minor", Category: "testing", Description: "issue q1"}}
			}
			return []event.Envelope{
				event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
					Persona: "developer", SourcePersona: "qa",
					Outcome: outcome, Summary: summary, Issues: issues,
				})),
			}, nil
		},
	})

	_ = env.reg.Register(&stubHandler{
		name:   "committer",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	})

	var fbMu sync.Mutex
	var fbSources []string
	unsub := env.bus.Subscribe(event.FeedbackGenerated, func(_ context.Context, e event.Envelope) error {
		if e.CorrelationID != "wf-legacy-fb" {
			return nil
		}
		var p event.FeedbackGeneratedPayload
		if err := json.Unmarshal(e.Payload, &p); err == nil {
			fbMu.Lock()
			fbSources = append(fbSources, p.SourcePersona)
			fbMu.Unlock()
		}
		return nil
	}, eventbus.WithName("e2e:legacy-feedback-tap"))
	t.Cleanup(unsub)

	result := awaitWorkflowResult(t, env.bus, "wf-legacy-fb")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-legacy-fb", "e2e-legacy-fb")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		dumpCorrelation(t, ctx, env, "wf-legacy-fb")
		t.Fatal("timeout: legacy workflow never completed")
	}

	time.Sleep(100 * time.Millisecond)

	fbMu.Lock()
	defer fbMu.Unlock()
	// Legacy path: each failing verdict emits its own FeedbackGenerated, so we
	// should see two — one each from reviewer and qa. The exact ordering depends
	// on dispatch interleaving but both must appear.
	if len(fbSources) != 2 {
		t.Fatalf("legacy path must emit 2 FeedbackGenerated (one per failing reviewer), got %d: %v", len(fbSources), fbSources)
	}
	seen := map[string]bool{}
	for _, s := range fbSources {
		seen[s] = true
	}
	if !seen["reviewer"] || !seen["qa"] {
		t.Errorf("legacy path: want feedback from both reviewer + qa, got %v", fbSources)
	}
}

// dumpCorrelation logs every event in the correlation chain. Used on timeout
// to surface why a test stalled.
func dumpCorrelation(t *testing.T, ctx context.Context, env *e2eEnv, corr string) {
	t.Helper()
	events, _ := env.store.LoadByCorrelation(ctx, corr)
	t.Logf("=== correlation %s (%d events) ===", corr, len(events))
	for _, e := range events {
		payload := string(e.Payload)
		if len(payload) > 120 {
			payload = payload[:120] + "..."
		}
		t.Logf("  %s (agg=%s) %s", e.Type, e.AggregateID, payload)
	}
}
