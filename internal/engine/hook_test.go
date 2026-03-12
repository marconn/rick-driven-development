package engine

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// =============================================================================
// Before-Hook: Frontend Enricher intercepts architect → developer chain
// =============================================================================
//
// Topology:
//   researcher → architect → frontend-enricher (hook) → developer → reviewer → committer
//
// Without the hook, developer's trigger is AfterPersonas: ["architect"].
// The hook adds "frontend-enricher" to developer's effective join condition
// without modifying developer's handler code.
//
// The enricher reads architect's output (AIResponseReceived), identifies
// relevant libraries/components, and emits a ContextEnrichment event.
// Developer reads the enrichment from the correlation chain.

func TestE2EBeforeHookFrontendEnricher(t *testing.T) {
	def := WorkflowDef{
		ID:            "e2e-hook",
		Required:      []string{"researcher", "architect", "frontend-enricher", "developer", "reviewer", "committer"},
		MaxIterations: 3,
		Graph: map[string][]string{
			"researcher":       {},
			"architect":        {"researcher"},
			"frontend-enricher": {"architect"},
			"developer":        {"architect"},
			"reviewer":         {"developer"},
			"committer":        {"reviewer"},
		},
	}
	env := newE2EEnv(t, def)
	tracer := newEventTracer(t, env.bus)

	// Replace the PersonaRunner with one that has the before-hook configured.
	// Close the old runner first.
	_ = env.runner.Close()
	env.runner = NewPersonaRunner(env.store, env.bus, NewLocalDispatcher(env.reg), env.engine.logger,
		WithBeforeHook("developer", "frontend-enricher"),
		WithMaxChainDepth(7), // 6-persona chain needs depth > 5
	)
	env.runner.RegisterWorkflow(def)

	var (
		enricherSawArchitect atomic.Bool
		developerSawEnrich   atomic.Bool
		enricherRuns         atomic.Int32
		developerRuns        atomic.Int32
	)

	register := func(name string, events []event.Type, after []string, fn func(context.Context, event.Envelope) ([]event.Envelope, error)) {
		t.Helper()
		if err := env.reg.Register(&stubTriggeredHandler{
			stubHandler: stubHandler{name: name, handle: fn},
			trigger:     handler.Trigger{Events: events, AfterPersonas: after},
		}); err != nil {
			t.Fatal(err)
		}
	}

	noop := func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil }

	register("researcher", []event.Type{event.WorkflowStartedFor("e2e-hook")}, nil, noop)

	// Architect emits an AIResponseReceived with a "plan" that the enricher reads.
	register("architect", []event.Type{event.PersonaCompleted}, []string{"researcher"},
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return []event.Envelope{
				event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
					Phase:   "architect",
					Backend: "claude",
					Output:  json.RawMessage(`"Build a dashboard with data tables, charts, and a form wizard. Use React + TypeScript."`),
				})),
			}, nil
		})

	// Frontend enricher: reads architect's output, suggests libraries.
	// Declared AfterPersonas: ["architect"] — fires after architect completes.
	register("frontend-enricher", []event.Type{event.PersonaCompleted}, []string{"architect"},
		func(ctx context.Context, triggerEnv event.Envelope) ([]event.Envelope, error) {
			enricherRuns.Add(1)

			// Read architect's output from the correlation chain.
			events, err := env.store.LoadByCorrelation(ctx, triggerEnv.CorrelationID)
			if err != nil {
				return nil, err
			}
			for _, e := range events {
				if e.Type == event.AIResponseReceived {
					var p event.AIResponsePayload
					if err := json.Unmarshal(e.Payload, &p); err == nil && p.Phase == "architect" {
						enricherSawArchitect.Store(true)
					}
				}
			}

			// Emit enrichment with library suggestions.
			return []event.Envelope{
				event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
					Source:  "frontend-enricher",
					Kind:    "libraries",
					Summary: "Recommended libraries for React dashboard with tables, charts, and form wizard",
					Items: []event.EnrichmentItem{
						{
							Name:       "tanstack-table",
							Version:    "^8.0.0",
							Reason:     "Headless table library — architect mentions data tables",
							ImportPath: "@tanstack/react-table",
						},
						{
							Name:       "recharts",
							Version:    "^2.0.0",
							Reason:     "React chart library — architect mentions charts",
							ImportPath: "recharts",
						},
						{
							Name:       "react-hook-form",
							Version:    "^7.0.0",
							Reason:     "Form state management — architect mentions form wizard",
							ImportPath: "react-hook-form",
						},
						{
							Name:       "shadcn/ui",
							Reason:     "Component library with accessible primitives — consistent with React+TS stack",
							DocURL:     "https://ui.shadcn.com",
							ImportPath: "@/components/ui",
						},
					},
				})).WithSource("handler:frontend-enricher"),
			}, nil
		})

	// Developer: declared AfterPersonas is ["architect"] only.
	// The WithBeforeHook("developer", "frontend-enricher") adds "frontend-enricher"
	// to the effective join condition, so developer won't fire until BOTH architect
	// AND frontend-enricher have completed.
	register("developer", []event.Type{event.PersonaCompleted, event.FeedbackGenerated}, []string{"architect"},
		func(ctx context.Context, triggerEnv event.Envelope) ([]event.Envelope, error) {
			developerRuns.Add(1)

			// Developer should see the enrichment in the correlation chain.
			events, err := env.store.LoadByCorrelation(ctx, triggerEnv.CorrelationID)
			if err != nil {
				return nil, err
			}
			for _, e := range events {
				if e.Type == event.ContextEnrichment {
					var p event.ContextEnrichmentPayload
					if err := json.Unmarshal(e.Payload, &p); err == nil && p.Source == "frontend-enricher" {
						developerSawEnrich.Store(true)
					}
				}
			}
			return nil, nil
		})

	register("reviewer", []event.Type{event.PersonaCompleted}, []string{"developer"}, noop)
	register("committer", []event.Type{event.PersonaCompleted}, []string{"reviewer"}, noop)

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-hook")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-hook", "e2e-hook")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		events, _ := env.store.Load(ctx, "wf-hook")
		for _, e := range events {
			t.Logf("  event: %s (v%d, agg=%s)", e.Type, e.Version, e.AggregateID)
		}
		corr, _ := env.store.LoadByCorrelation(ctx, "wf-hook")
		t.Logf("  --- correlation events ---")
		for _, e := range corr {
			t.Logf("  event: %s (agg=%s)", e.Type, e.AggregateID)
		}
		t.Fatal("timeout: hook workflow never completed")
	}

	time.Sleep(100 * time.Millisecond)

	// Verify the enricher ran and saw architect's output.
	if !enricherSawArchitect.Load() {
		t.Error("frontend-enricher should have seen architect's AIResponseReceived")
	}
	if enricherRuns.Load() != 1 {
		t.Errorf("frontend-enricher: expected 1 run, got %d", enricherRuns.Load())
	}

	// Verify developer saw the enrichment context.
	if !developerSawEnrich.Load() {
		t.Error("developer should have seen ContextEnrichment from frontend-enricher")
	}
	if developerRuns.Load() != 1 {
		t.Errorf("developer: expected 1 run, got %d", developerRuns.Load())
	}

	tracer.dump(t)
}

// TestE2EBeforeHookDoesNotFireDeveloperEarly verifies that the before-hook
// actually gates developer. Without the enricher completing, developer must
// not fire even though architect has completed.
func TestE2EBeforeHookDoesNotFireDeveloperEarly(t *testing.T) {
	def := WorkflowDef{
		ID:            "e2e-hook-gate",
		Required:      []string{"architect", "developer"},
		MaxIterations: 3,
		Graph: map[string][]string{
			"architect": {},
			"developer": {"architect"},
		},
	}
	env := newE2EEnv(t, def)

	_ = env.runner.Close()
	env.runner = NewPersonaRunner(env.store, env.bus, NewLocalDispatcher(env.reg), env.engine.logger,
		WithBeforeHook("developer", "slow-enricher"),
	)
	env.runner.RegisterWorkflow(def)

	var developerFired atomic.Bool

	register := func(name string, events []event.Type, after []string, fn func(context.Context, event.Envelope) ([]event.Envelope, error)) {
		t.Helper()
		if err := env.reg.Register(&stubTriggeredHandler{
			stubHandler: stubHandler{name: name, handle: fn},
			trigger:     handler.Trigger{Events: events, AfterPersonas: after},
		}); err != nil {
			t.Fatal(err)
		}
	}

	register("architect", []event.Type{event.WorkflowStartedFor("e2e-hook-gate")}, nil,
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil })

	// slow-enricher: registered but never completes in this test (simulated by not registering it)
	// Developer should NOT fire because slow-enricher hasn't completed.

	register("developer", []event.Type{event.PersonaCompleted, event.FeedbackGenerated}, []string{"architect"},
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			developerFired.Store(true)
			return nil, nil
		})

	ctx := context.Background()
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-hook-gate", "e2e-hook-gate")

	// Give the system time to process architect → (developer should be gated).
	time.Sleep(500 * time.Millisecond)

	if developerFired.Load() {
		t.Error("developer should NOT fire — slow-enricher hasn't completed and is a before-hook")
	}
}

// TestE2EBeforeHookMultipleHooks verifies that multiple hooks can be stacked
// on a single persona.
func TestE2EBeforeHookMultipleHooks(t *testing.T) {
	def := WorkflowDef{
		ID:            "e2e-multi-hook",
		Required:      []string{"architect", "hook-a", "hook-b", "developer"},
		MaxIterations: 3,
		Graph: map[string][]string{
			"architect": {},
			"hook-a":    {"architect"},
			"hook-b":    {"architect"},
			"developer": {"architect"},
		},
	}
	env := newE2EEnv(t, def)

	_ = env.runner.Close()
	env.runner = NewPersonaRunner(env.store, env.bus, NewLocalDispatcher(env.reg), env.engine.logger,
		WithBeforeHook("developer", "hook-a", "hook-b"),
	)
	env.runner.RegisterWorkflow(def)

	register := func(name string, events []event.Type, after []string, fn func(context.Context, event.Envelope) ([]event.Envelope, error)) {
		t.Helper()
		if err := env.reg.Register(&stubTriggeredHandler{
			stubHandler: stubHandler{name: name, handle: fn},
			trigger:     handler.Trigger{Events: events, AfterPersonas: after},
		}); err != nil {
			t.Fatal(err)
		}
	}

	noop := func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil }

	register("architect", []event.Type{event.WorkflowStartedFor("e2e-multi-hook")}, nil, noop)
	register("hook-a", []event.Type{event.PersonaCompleted}, []string{"architect"}, noop)
	register("hook-b", []event.Type{event.PersonaCompleted}, []string{"architect"}, noop)
	register("developer", []event.Type{event.PersonaCompleted}, []string{"architect"}, noop)

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-multi-hook")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-multi-hook", "e2e-multi-hook")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: multi-hook workflow never completed")
	}
}

// =============================================================================
// Before-Hook: handler subscribing to workflow.started.* (not persona.completed)
// =============================================================================
//
// Reproduces the bug where github-pr-fetcher (hook) gates feedback-analyzer,
// but feedback-analyzer only subscribes to workflow.started.pr-feedback.
// Without the fix, feedback-analyzer never fires because:
//   1. On workflow.started.pr-feedback → hook join not satisfied → skipped
//   2. On PersonaCompleted{hook} → not subscribed → never re-evaluated
//
// The fix auto-adds a persona.completed subscription when hooks exist.

// TestE2EBeforeHookStaticGatedByWorkflowStarted tests that a handler which
// subscribes only to workflow.started.* (not persona.completed) correctly fires
// after its before-hook completes. Uses WithBeforeHook (static, before Start).
func TestE2EBeforeHookStaticGatedByWorkflowStarted(t *testing.T) {
	def := WorkflowDef{
		ID:            "e2e-hook-wfstart",
		Required:      []string{"pr-fetcher", "analyzer"},
		MaxIterations: 3,
		Graph:         map[string][]string{"pr-fetcher": {}, "analyzer": {}},
	}
	env := newE2EEnv(t, def)

	_ = env.runner.Close()
	env.runner = NewPersonaRunner(env.store, env.bus, NewLocalDispatcher(env.reg), env.engine.logger,
		WithBeforeHook("analyzer", "pr-fetcher"),
	)
	env.runner.RegisterWorkflow(def)

	var analyzerRuns atomic.Int32

	register := func(name string, events []event.Type, after []string, fn func(context.Context, event.Envelope) ([]event.Envelope, error)) {
		t.Helper()
		if err := env.reg.Register(&stubTriggeredHandler{
			stubHandler: stubHandler{name: name, handle: fn},
			trigger:     handler.Trigger{Events: events, AfterPersonas: after},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// pr-fetcher: subscribes to workflow.started, no join condition.
	register("pr-fetcher", []event.Type{event.WorkflowStartedFor("e2e-hook-wfstart")}, nil,
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return nil, nil
		})

	// analyzer: subscribes ONLY to workflow.started (NOT persona.completed).
	// Before the fix, this handler would never fire because:
	//   - On workflow.started → pr-fetcher hasn't completed → join fails
	//   - On PersonaCompleted{pr-fetcher} → not subscribed → never sees it
	register("analyzer", []event.Type{event.WorkflowStartedFor("e2e-hook-wfstart")}, nil,
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			analyzerRuns.Add(1)
			return nil, nil
		})

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-hook-wfstart")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-hook-wfstart", "e2e-hook-wfstart")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		corr, _ := env.store.LoadByCorrelation(ctx, "wf-hook-wfstart")
		for _, e := range corr {
			t.Logf("  event: %s (agg=%s)", e.Type, e.AggregateID)
		}
		t.Fatal("timeout: workflow never completed — analyzer not re-evaluated after hook")
	}

	if runs := analyzerRuns.Load(); runs != 1 {
		t.Errorf("analyzer: expected 1 run, got %d", runs)
	}
}

// TestE2EBeforeHookDynamicGatedByWorkflowStarted tests the same scenario but
// with RegisterHook (dynamic, after Start). This reproduces the exact
// github-pr-fetcher / feedback-analyzer bug from production.
func TestE2EBeforeHookDynamicGatedByWorkflowStarted(t *testing.T) {
	def := WorkflowDef{
		ID:            "e2e-hook-dynamic-wfstart",
		Required:      []string{"fetcher", "analyzer"},
		MaxIterations: 3,
		Graph:         map[string][]string{"fetcher": {}, "analyzer": {}},
	}
	env := newE2EEnv(t, def)

	var analyzerRuns atomic.Int32

	register := func(name string, events []event.Type, after []string, fn func(context.Context, event.Envelope) ([]event.Envelope, error)) {
		t.Helper()
		if err := env.reg.Register(&stubTriggeredHandler{
			stubHandler: stubHandler{name: name, handle: fn},
			trigger:     handler.Trigger{Events: events, AfterPersonas: after},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// analyzer: subscribes ONLY to workflow.started (no persona.completed).
	register("analyzer", []event.Type{event.WorkflowStartedFor("e2e-hook-dynamic-wfstart")}, nil,
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			analyzerRuns.Add(1)
			return nil, nil
		})

	ctx := context.Background()
	env.start(ctx)

	// Simulate gRPC handler connecting after Start():
	// 1. Register the fetcher handler dynamically (both in registry for
	//    dispatch and in PersonaRunner for bus subscription).
	fetcherHandler := &stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "fetcher",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-hook-dynamic-wfstart")}},
	}
	if err := env.reg.Register(fetcherHandler); err != nil {
		t.Fatal(err)
	}
	unsub := env.runner.RegisterHandler(fetcherHandler)
	t.Cleanup(unsub)

	// 2. Register the before-hook (fetcher gates analyzer).
	env.runner.RegisterHook("analyzer", "fetcher")

	result := awaitWorkflowResult(t, env.bus, "wf-hook-dynamic-wfstart")
	env.fireWorkflow(ctx, t, "wf-hook-dynamic-wfstart", "e2e-hook-dynamic-wfstart")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		corr, _ := env.store.LoadByCorrelation(ctx, "wf-hook-dynamic-wfstart")
		for _, e := range corr {
			t.Logf("  event: %s (agg=%s)", e.Type, e.AggregateID)
		}
		t.Fatal("timeout: workflow never completed — dynamic hook subscription missing")
	}

	if runs := analyzerRuns.Load(); runs != 1 {
		t.Errorf("analyzer: expected 1 run, got %d", runs)
	}
}
