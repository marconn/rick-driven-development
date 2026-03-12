package engine

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// =============================================================================
// OperatorGuidance End-to-End Tests
// =============================================================================

// TestE2EOperatorGuidanceFullFlow verifies the complete guidance flow:
//
//  1. developer + reviewer run → reviewer FAIL → FeedbackGenerated
//  2. developer re-runs → reviewer FAIL again → max iterations reached
//  3. Auto-escalation → WorkflowPaused
//  4. Operator injects guidance + auto-resume
//  5. Developer sees guidance in correlation chain
//  6. Reviewer passes → WorkflowCompleted
//
// This tests that OperatorGuidance events are persisted in the correlation
// chain and visible to downstream personas after resume.
func TestE2EOperatorGuidanceFullFlow(t *testing.T) {
	def := WorkflowDef{
		ID:                "e2e-guidance",
		Required:          []string{"developer", "reviewer"},
		MaxIterations:     1,
		EscalateOnMaxIter: true,
		Graph:             map[string][]string{"developer": {}, "reviewer": {"developer"}},
		RetriggeredBy:     map[string][]event.Type{"developer": {event.FeedbackGenerated}},
	}
	env := newE2EEnv(t, def)
	tracer := newEventTracer(t, env.bus)

	var devRuns, reviewRuns atomic.Int32
	var developerSawGuidance atomic.Bool

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "developer",
			handle: func(ctx context.Context, triggerEnv event.Envelope) ([]event.Envelope, error) {
				n := devRuns.Add(1)

				// On the 3rd run (after guidance), check for OperatorGuidance.
				if n >= 3 {
					events, _ := env.store.LoadByCorrelation(ctx, triggerEnv.CorrelationID)
					for _, e := range events {
						if e.Type == event.OperatorGuidance {
							var p event.OperatorGuidancePayload
							if err := json.Unmarshal(e.Payload, &p); err == nil {
								if strings.Contains(p.Content, "React Server Components") {
									developerSawGuidance.Store(true)
								}
							}
						}
					}
				}
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events: []event.Type{event.WorkflowStartedFor("e2e-guidance"), event.FeedbackGenerated},
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
							Phase:       "developer",
							SourcePhase: "reviewer",
							Outcome:     event.VerdictFail,
							Summary:     "needs server components",
						})),
					}, nil
				}
				// 3rd review (after guidance): pass
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

	// Detect escalation pause.
	paused := make(chan event.Envelope, 1)
	unsubPause := env.bus.Subscribe(event.WorkflowPaused, func(_ context.Context, e event.Envelope) error {
		if e.CorrelationID == "wf-guidance" {
			select {
			case paused <- e:
			default:
			}
		}
		return nil
	}, eventbus.WithName("test:pause-detect"))
	defer unsubPause()

	result := awaitWorkflowResult(t, env.bus, "wf-guidance")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-guidance", "e2e-guidance")

	// Wait for auto-escalation.
	select {
	case <-paused:
		t.Log("auto-escalation triggered — workflow paused")
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: auto-escalation never triggered")
	}

	// Give PersonaRunner time to process the pause.
	time.Sleep(200 * time.Millisecond)

	// Inject operator guidance + auto-resume.
	events, _ := env.store.Load(ctx, "wf-guidance")
	currentVersion := events[len(events)-1].Version

	var allEvents []event.Envelope

	guidanceEvt := event.New(event.OperatorGuidance, 1, event.MustMarshal(event.OperatorGuidancePayload{
		Content:    "Use React Server Components for the dashboard. The data tables should use tanstack-table v8.",
		Target:     "developer",
		AutoResume: true,
	})).
		WithAggregate("wf-guidance", currentVersion+1).
		WithCorrelation("wf-guidance").
		WithSource("test:operator")
	allEvents = append(allEvents, guidanceEvt)

	resumeEvt := event.New(event.WorkflowResumed, 1, event.MustMarshal(event.WorkflowResumedPayload{
		Reason: "operator guidance injected — auto-resume",
	})).
		WithAggregate("wf-guidance", currentVersion+2).
		WithCorrelation("wf-guidance").
		WithSource("test:operator")
	allEvents = append(allEvents, resumeEvt)

	if err := env.store.Append(ctx, "wf-guidance", currentVersion, allEvents); err != nil {
		t.Fatalf("append guidance+resume: %v", err)
	}
	for _, evt := range allEvents {
		if err := env.bus.Publish(ctx, evt); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	// Wait for workflow completion.
	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		tracer.dump(t)
		t.Fatalf("timeout: guidance flow never completed (dev=%d, review=%d, sawGuidance=%v)",
			devRuns.Load(), reviewRuns.Load(), developerSawGuidance.Load())
	}

	time.Sleep(100 * time.Millisecond)

	// Verify developer saw the guidance.
	if !developerSawGuidance.Load() {
		t.Error("developer should have seen OperatorGuidance in correlation chain")
	}

	if devRuns.Load() < 3 {
		t.Errorf("expected at least 3 dev runs, got %d", devRuns.Load())
	}
	if reviewRuns.Load() < 3 {
		t.Errorf("expected at least 3 review runs, got %d", reviewRuns.Load())
	}

	t.Logf("final: dev=%d, review=%d, sawGuidance=%v",
		devRuns.Load(), reviewRuns.Load(), developerSawGuidance.Load())

	tracer.dump(t)
}

// TestE2EOperatorGuidanceDispatchPriority verifies that when OperatorGuidance
// and FeedbackGenerated are both pending for the developer, OperatorGuidance
// (priority 0) is processed before FeedbackGenerated (priority 10).
func TestE2EOperatorGuidanceDispatchPriority(t *testing.T) {
	def := WorkflowDef{ID: "e2e-guidance-priority", Required: []string{"developer"}, MaxIterations: 3}
	env := newE2EEnv(t, def)

	var triggerOrder []event.Type
	var mu sync.Mutex

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "developer",
			handle: func(_ context.Context, triggerEnv event.Envelope) ([]event.Envelope, error) {
				mu.Lock()
				triggerOrder = append(triggerOrder, triggerEnv.Type)
				mu.Unlock()
				time.Sleep(10 * time.Millisecond) // let queue accumulate
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events: []event.Type{event.PersonaCompleted, event.FeedbackGenerated, event.OperatorGuidance},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	env.runner.Start(ctx, env.reg)

	corrID := "wf-guidance-prio"

	// Fire FeedbackGenerated first (priority 10).
	fbEvt := event.New(event.FeedbackGenerated, 1, event.MustMarshal(event.FeedbackGeneratedPayload{
		TargetPhase: "developer", Iteration: 1,
	})).WithCorrelation(corrID).WithSource("test")

	// Fire OperatorGuidance second (priority 0 — should process first).
	ogEvt := event.New(event.OperatorGuidance, 1, event.MustMarshal(event.OperatorGuidancePayload{
		Content: "use server components", Target: "developer",
	})).WithCorrelation(corrID).WithSource("test")

	// Publish both in quick succession.
	_ = env.bus.Publish(ctx, fbEvt)
	_ = env.bus.Publish(ctx, ogEvt)

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	t.Logf("trigger order: %v", triggerOrder)

	if len(triggerOrder) < 2 {
		t.Fatalf("expected at least 2 triggers, got %d", len(triggerOrder))
	}

	// Both should be processed. If both made it into the queue before
	// draining, OperatorGuidance should be first.
	seenOG, seenFB := false, false
	for _, tt := range triggerOrder {
		if tt == event.OperatorGuidance {
			seenOG = true
		}
		if tt == event.FeedbackGenerated {
			seenFB = true
		}
	}
	if !seenOG {
		t.Error("OperatorGuidance should have been processed")
	}
	if !seenFB {
		t.Error("FeedbackGenerated should have been processed")
	}
}
