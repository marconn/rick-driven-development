package engine

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/handler"
	"github.com/marconn/rick-event-driven-development/internal/projection"
)

// recoveryEnv bundles all components for a recovery test.
type recoveryEnv struct {
	store     eventstore.Store
	bus       *eventbus.ChannelBus
	engine    *Engine
	runner    *PersonaRunner
	reg       *handler.Registry
	workflows *projection.WorkflowStatusProjection
}

func newRecoveryEnv(t *testing.T, def WorkflowDef) *recoveryEnv {
	t.Helper()
	store, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	bus := eventbus.NewChannelBus()
	reg := handler.NewRegistry()
	dispatcher := NewLocalDispatcher(reg)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	eng := NewEngine(store, bus, logger)
	eng.RegisterWorkflow(def)

	runner := NewPersonaRunner(store, bus, dispatcher, logger)
	runner.RegisterWorkflow(def)

	workflows := projection.NewWorkflowStatusProjection()

	t.Cleanup(func() {
		_ = runner.Close()
		eng.Stop()
		_ = bus.Close()
		_ = store.Close()
	})

	return &recoveryEnv{
		store:     store,
		bus:       bus,
		engine:    eng,
		runner:    runner,
		reg:       reg,
		workflows: workflows,
	}
}

// seedWorkflowRequested stores a WorkflowRequested event and feeds it to the projection.
func (e *recoveryEnv) seedWorkflowRequested(t *testing.T, corrID, wfDefID string) {
	t.Helper()
	ctx := context.Background()
	env := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "test",
		WorkflowID: wfDefID,
	})).WithAggregate(corrID, 1).WithCorrelation(corrID).WithSource("test")

	if err := e.store.Append(ctx, corrID, 0, []event.Envelope{env}); err != nil {
		t.Fatalf("seed WorkflowRequested: %v", err)
	}
	if err := e.workflows.Handle(ctx, env); err != nil {
		t.Fatalf("project WorkflowRequested: %v", err)
	}
}

// seedWorkflowStarted stores a workflow.started.* event and feeds it to the projection.
func (e *recoveryEnv) seedWorkflowStarted(t *testing.T, corrID, wfDefID string, version int) {
	t.Helper()
	ctx := context.Background()
	env := event.New(event.WorkflowStartedFor(wfDefID), 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: wfDefID,
		Phases:     []string{"alpha", "beta"},
	})).WithAggregate(corrID, version).WithCorrelation(corrID).WithSource("engine")

	if err := e.store.Append(ctx, corrID, version-1, []event.Envelope{env}); err != nil {
		t.Fatalf("seed WorkflowStarted: %v", err)
	}
	if err := e.workflows.Handle(ctx, env); err != nil {
		t.Fatalf("project WorkflowStarted: %v", err)
	}
}

// seedPersonaTracked stores a PersonaTracked event on the workflow aggregate
// (mirrors what Engine does when a persona completes).
func (e *recoveryEnv) seedPersonaTracked(t *testing.T, corrID, persona string, version int) {
	t.Helper()
	ctx := context.Background()
	env := event.New(event.PersonaTracked, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: persona,
	})).WithAggregate(corrID, version).WithCorrelation(corrID).WithSource("engine")

	if err := e.store.Append(ctx, corrID, version-1, []event.Envelope{env}); err != nil {
		t.Fatalf("seed PersonaTracked: %v", err)
	}
}

// seedWorkflowPaused stores a WorkflowPaused event and feeds it to the projection.
func (e *recoveryEnv) seedWorkflowPaused(t *testing.T, corrID string, version int) {
	t.Helper()
	ctx := context.Background()
	env := event.New(event.WorkflowPaused, 1, nil).
		WithAggregate(corrID, version).WithCorrelation(corrID).WithSource("test")

	if err := e.store.Append(ctx, corrID, version-1, []event.Envelope{env}); err != nil {
		t.Fatalf("seed WorkflowPaused: %v", err)
	}
	if err := e.workflows.Handle(ctx, env); err != nil {
		t.Fatalf("project WorkflowPaused: %v", err)
	}
}

// seedFeedbackGenerated stores a FeedbackGenerated event on the workflow aggregate.
func (e *recoveryEnv) seedFeedbackGenerated(t *testing.T, corrID, targetPhase string, iteration, version int) {
	t.Helper()
	ctx := context.Background()
	env := event.New(event.FeedbackGenerated, 1, event.MustMarshal(event.FeedbackGeneratedPayload{
		TargetPhase: targetPhase,
		Iteration:   iteration,
		Summary:     "test feedback",
	})).WithAggregate(corrID, version).WithCorrelation(corrID).WithSource("engine")

	if err := e.store.Append(ctx, corrID, version-1, []event.Envelope{env}); err != nil {
		t.Fatalf("seed FeedbackGenerated: %v", err)
	}
}

// seedHintEmitted stores a HintEmitted event on a persona-scoped aggregate.
func (e *recoveryEnv) seedHintEmitted(t *testing.T, corrID, persona string) {
	t.Helper()
	ctx := context.Background()
	aggID := corrID + ":persona:" + persona
	env := event.New(event.HintEmitted, 1, event.MustMarshal(event.HintEmittedPayload{
		Persona:    persona,
		Confidence: 0.3,
		Plan:       "test plan",
	})).WithAggregate(aggID, 1).WithCorrelation(corrID).WithSource("runner")

	if err := e.store.Append(ctx, aggID, 0, []event.Envelope{env}); err != nil {
		t.Fatalf("seed HintEmitted: %v", err)
	}
}

// seedWorkflowCompleted stores a WorkflowCompleted event and feeds it to the projection.
func (e *recoveryEnv) seedWorkflowCompleted(t *testing.T, corrID string, version int) {
	t.Helper()
	ctx := context.Background()
	env := event.New(event.WorkflowCompleted, 1, nil).
		WithAggregate(corrID, version).WithCorrelation(corrID).WithSource("engine")

	if err := e.store.Append(ctx, corrID, version-1, []event.Envelope{env}); err != nil {
		t.Fatalf("seed WorkflowCompleted: %v", err)
	}
	if err := e.workflows.Handle(ctx, env); err != nil {
		t.Fatalf("project WorkflowCompleted: %v", err)
	}
}

// startForRecovery starts the engine and runner, then runs the recovery scanner.
func (e *recoveryEnv) startForRecovery(t *testing.T) RecoveryResult {
	t.Helper()
	e.engine.Start()
	e.runner.Start(context.Background(), e.reg)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	scanner := NewRecoveryScanner(e.store, e.workflows, e.runner, e.engine, logger)
	return scanner.Recover(context.Background())
}

// --- Tests ---

func TestRecoveryNoRunningWorkflows(t *testing.T) {
	def := WorkflowDef{
		ID: "test-wf", Required: []string{"alpha"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}},
	}
	env := newRecoveryEnv(t, def)

	result := env.startForRecovery(t)

	if result.Recovered != 0 {
		t.Errorf("expected 0 recovered, got %d", result.Recovered)
	}
	if result.PausedRestored != 0 {
		t.Errorf("expected 0 paused restored, got %d", result.PausedRestored)
	}
}

func TestRecoveryCompletedWorkflowSkipped(t *testing.T) {
	def := WorkflowDef{
		ID: "test-wf", Required: []string{"alpha"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}},
	}
	env := newRecoveryEnv(t, def)

	env.seedWorkflowRequested(t, "wf-1", "test-wf")
	env.seedWorkflowStarted(t, "wf-1", "test-wf", 2)
	env.seedPersonaTracked(t, "wf-1", "alpha", 3)
	env.seedWorkflowCompleted(t, "wf-1", 4)

	result := env.startForRecovery(t)

	if result.Recovered != 0 {
		t.Errorf("completed workflow should be skipped, got %d recovered", result.Recovered)
	}
}

func TestRecoveryRootHandlerNeverRan(t *testing.T) {
	def := WorkflowDef{
		ID: "test-wf", Required: []string{"alpha"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}},
	}
	env := newRecoveryEnv(t, def)

	handled := make(chan string, 1)
	if err := env.reg.Register(&stubHandler{
		name: "alpha",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			handled <- "alpha"
			return nil, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	env.seedWorkflowRequested(t, "wf-1", "test-wf")
	env.seedWorkflowStarted(t, "wf-1", "test-wf", 2)

	result := env.startForRecovery(t)

	if result.Recovered != 1 {
		t.Errorf("expected 1 recovered, got %d", result.Recovered)
	}

	select {
	case name := <-handled:
		if name != "alpha" {
			t.Errorf("expected alpha to be dispatched, got %s", name)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: alpha was not dispatched")
	}
}

func TestRecoveryMidChain(t *testing.T) {
	def := WorkflowDef{
		ID: "test-wf", Required: []string{"alpha", "beta"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}, "beta": {"alpha"}},
	}
	env := newRecoveryEnv(t, def)

	handled := make(chan string, 1)
	if err := env.reg.Register(&stubHandler{
		name:   "alpha",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubHandler{
		name: "beta",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			handled <- "beta"
			return nil, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	env.seedWorkflowRequested(t, "wf-1", "test-wf")
	env.seedWorkflowStarted(t, "wf-1", "test-wf", 2)
	// Alpha completed — tracked on workflow aggregate.
	env.seedPersonaTracked(t, "wf-1", "alpha", 3)
	// Also seed the PersonaCompleted on persona-scoped aggregate (for LoadByCorrelation).
	seedPersonaCompleted(t, env.store, "wf-1", "alpha")

	result := env.startForRecovery(t)

	if result.Recovered != 1 {
		t.Errorf("expected 1 recovered, got %d", result.Recovered)
	}

	select {
	case name := <-handled:
		if name != "beta" {
			t.Errorf("expected beta, got %s", name)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: beta was not dispatched")
	}
}

func TestRecoveryParallelHandlers(t *testing.T) {
	def := WorkflowDef{
		ID: "test-wf", Required: []string{"alpha", "beta", "gamma"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}, "beta": {"alpha"}, "gamma": {"alpha"}},
	}
	env := newRecoveryEnv(t, def)

	var mu sync.Mutex
	dispatched := make(map[string]bool)
	done := make(chan struct{}, 2)

	for _, name := range []string{"alpha", "beta", "gamma"} {
		n := name
		if err := env.reg.Register(&stubHandler{
			name: n,
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				mu.Lock()
				dispatched[n] = true
				mu.Unlock()
				done <- struct{}{}
				return nil, nil
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	env.seedWorkflowRequested(t, "wf-1", "test-wf")
	env.seedWorkflowStarted(t, "wf-1", "test-wf", 2)
	env.seedPersonaTracked(t, "wf-1", "alpha", 3)
	seedPersonaCompleted(t, env.store, "wf-1", "alpha")

	result := env.startForRecovery(t)

	if result.Recovered != 2 {
		t.Errorf("expected 2 recovered (beta+gamma), got %d", result.Recovered)
	}

	// Wait for both dispatches.
	for range 2 {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("timeout: not all handlers dispatched")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if !dispatched["beta"] {
		t.Error("beta was not dispatched")
	}
	if !dispatched["gamma"] {
		t.Error("gamma was not dispatched")
	}
}

func TestRecoveryJoinGatePartial(t *testing.T) {
	def := WorkflowDef{
		ID: "test-wf", Required: []string{"alpha", "beta", "gate"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}, "beta": {}, "gate": {"alpha", "beta"}},
	}
	env := newRecoveryEnv(t, def)

	// Beta blocks so it never completes — prevents cascade to gate during test.
	betaBlock := make(chan struct{})
	t.Cleanup(func() { close(betaBlock) })

	for _, name := range []string{"alpha"} {
		n := name
		if err := env.reg.Register(&stubHandler{
			name:   n,
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.reg.Register(&stubHandler{
		name: "beta",
		handle: func(ctx context.Context, _ event.Envelope) ([]event.Envelope, error) {
			select {
			case <-betaBlock:
			case <-ctx.Done():
			}
			return nil, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	gateHandled := make(chan struct{}, 1)
	if err := env.reg.Register(&stubHandler{
		name: "gate",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			gateHandled <- struct{}{}
			return nil, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	env.seedWorkflowRequested(t, "wf-1", "test-wf")
	env.seedWorkflowStarted(t, "wf-1", "test-wf", 2)
	// Only alpha completed, beta still running.
	env.seedPersonaTracked(t, "wf-1", "alpha", 3)
	seedPersonaCompleted(t, env.store, "wf-1", "alpha")

	result := env.startForRecovery(t)

	// Beta should be recovered (root handler not completed), gate should NOT
	// be recovered by the scanner (join partial). Beta blocks so no cascade.
	if result.Recovered != 1 {
		t.Errorf("expected 1 recovered (beta only), got %d", result.Recovered)
	}

	select {
	case <-gateHandled:
		t.Error("gate should NOT have been dispatched (join unsatisfied)")
	case <-time.After(200 * time.Millisecond):
		// good: gate was not dispatched
	}
}

func TestRecoveryFeedbackLoop(t *testing.T) {
	def := WorkflowDef{
		ID: "test-wf", Required: []string{"dev", "reviewer"}, MaxIterations: 3,
		Graph: map[string][]string{"dev": {}, "reviewer": {"dev"}},
		RetriggeredBy: map[string][]event.Type{
			"dev": {event.FeedbackGenerated},
		},
	}
	env := newRecoveryEnv(t, def)

	devHandled := make(chan string, 1)
	if err := env.reg.Register(&stubHandler{
		name: "dev",
		handle: func(_ context.Context, env event.Envelope) ([]event.Envelope, error) {
			devHandled <- string(env.Type)
			return nil, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubHandler{
		name:   "reviewer",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate: dev completed, reviewer gave feedback, dev was cleared.
	env.seedWorkflowRequested(t, "wf-1", "test-wf")
	env.seedWorkflowStarted(t, "wf-1", "test-wf", 2)
	env.seedPersonaTracked(t, "wf-1", "dev", 3)
	seedPersonaCompleted(t, env.store, "wf-1", "dev")
	env.seedPersonaTracked(t, "wf-1", "reviewer", 4)
	seedPersonaCompleted(t, env.store, "wf-1", "reviewer")
	// FeedbackGenerated clears dev from CompletedPersonas in aggregate.
	env.seedFeedbackGenerated(t, "wf-1", "dev", 1, 5)

	result := env.startForRecovery(t)

	if result.Recovered != 1 {
		t.Errorf("expected 1 recovered (dev retrigger), got %d", result.Recovered)
	}

	select {
	case triggerType := <-devHandled:
		if triggerType != string(event.FeedbackGenerated) {
			t.Errorf("expected dev triggered by FeedbackGenerated, got %s", triggerType)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: dev was not re-dispatched")
	}
}

func TestRecoveryPausedWorkflow(t *testing.T) {
	def := WorkflowDef{
		ID: "test-wf", Required: []string{"alpha"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}},
	}
	env := newRecoveryEnv(t, def)

	alphaHandled := make(chan struct{}, 1)
	if err := env.reg.Register(&stubHandler{
		name: "alpha",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			alphaHandled <- struct{}{}
			return nil, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	env.seedWorkflowRequested(t, "wf-1", "test-wf")
	env.seedWorkflowStarted(t, "wf-1", "test-wf", 2)
	env.seedWorkflowPaused(t, "wf-1", 3)

	result := env.startForRecovery(t)

	if result.PausedRestored != 1 {
		t.Errorf("expected 1 paused restored, got %d", result.PausedRestored)
	}
	if result.Recovered != 0 {
		t.Errorf("expected 0 recovered for paused workflow, got %d", result.Recovered)
	}

	select {
	case <-alphaHandled:
		t.Error("paused workflow should NOT dispatch handlers")
	case <-time.After(200 * time.Millisecond):
		// good
	}
}

func TestRecoveryHintPending(t *testing.T) {
	def := WorkflowDef{
		ID: "test-wf", Required: []string{"alpha"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}},
	}
	env := newRecoveryEnv(t, def)

	alphaHandled := make(chan struct{}, 1)
	if err := env.reg.Register(&stubHandler{
		name: "alpha",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			alphaHandled <- struct{}{}
			return nil, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	env.seedWorkflowRequested(t, "wf-1", "test-wf")
	env.seedWorkflowStarted(t, "wf-1", "test-wf", 2)
	// Hint emitted but not approved — workflow should stay paused.
	env.seedHintEmitted(t, "wf-1", "alpha")

	result := env.startForRecovery(t)

	if result.PausedRestored != 1 {
		t.Errorf("expected 1 paused restored (hint pending), got %d", result.PausedRestored)
	}
	if result.Recovered != 0 {
		t.Errorf("expected 0 recovered (hint pending), got %d", result.Recovered)
	}

	select {
	case <-alphaHandled:
		t.Error("hint-pending handler should NOT be dispatched")
	case <-time.After(200 * time.Millisecond):
		// good
	}
}

func TestRecoveryAllCompletedNoTerminal(t *testing.T) {
	def := WorkflowDef{
		ID: "test-wf", Required: []string{"alpha"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}},
	}
	env := newRecoveryEnv(t, def)

	if err := env.reg.Register(&stubHandler{
		name:   "alpha",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	}); err != nil {
		t.Fatal(err)
	}

	// All required done but no WorkflowCompleted.
	env.seedWorkflowRequested(t, "wf-1", "test-wf")
	env.seedWorkflowStarted(t, "wf-1", "test-wf", 2)
	env.seedPersonaTracked(t, "wf-1", "alpha", 3)
	seedPersonaCompleted(t, env.store, "wf-1", "alpha")

	// Subscribe for WorkflowCompleted to verify Engine processes the re-published event.
	completed := make(chan struct{}, 1)
	env.bus.Subscribe(event.WorkflowCompleted, func(_ context.Context, e event.Envelope) error {
		if e.AggregateID == "wf-1" {
			completed <- struct{}{}
		}
		return nil
	}, eventbus.WithName("test:completed"))

	result := env.startForRecovery(t)

	// No handlers need dispatch — all completed — but a terminal trigger should be re-published.
	if result.Recovered != 0 {
		t.Errorf("expected 0 recovered (all done), got %d", result.Recovered)
	}

	select {
	case <-completed:
		// Engine processed the re-published PersonaCompleted and emitted WorkflowCompleted.
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: WorkflowCompleted never arrived after recovery")
	}
}

func TestRecoveryCorrelationCacheWarmed(t *testing.T) {
	def := WorkflowDef{
		ID: "test-wf", Required: []string{"alpha"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}},
	}
	env := newRecoveryEnv(t, def)

	if err := env.reg.Register(&stubHandler{
		name:   "alpha",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	}); err != nil {
		t.Fatal(err)
	}

	env.seedWorkflowRequested(t, "wf-1", "test-wf")
	env.seedWorkflowStarted(t, "wf-1", "test-wf", 2)

	_ = env.startForRecovery(t)

	// Verify the correlation cache was populated.
	wfID, ok := env.runner.resolver.resolveWorkflowID("wf-1")
	if !ok {
		t.Fatal("correlation cache not warmed: resolveWorkflowID returned false")
	}
	if wfID != "test-wf" {
		t.Errorf("expected workflow ID 'test-wf', got %q", wfID)
	}
}

func TestRecoveryCompletedHandlerNotReDispatched(t *testing.T) {
	def := WorkflowDef{
		ID: "test-wf", Required: []string{"alpha", "beta"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}, "beta": {"alpha"}},
	}
	env := newRecoveryEnv(t, def)

	alphaCount := 0
	var mu sync.Mutex
	if err := env.reg.Register(&stubHandler{
		name: "alpha",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			mu.Lock()
			alphaCount++
			mu.Unlock()
			return nil, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubHandler{
		name:   "beta",
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	}); err != nil {
		t.Fatal(err)
	}

	// Both alpha and beta completed on the aggregate.
	env.seedWorkflowRequested(t, "wf-1", "test-wf")
	env.seedWorkflowStarted(t, "wf-1", "test-wf", 2)
	env.seedPersonaTracked(t, "wf-1", "alpha", 3)
	env.seedPersonaTracked(t, "wf-1", "beta", 4)
	seedPersonaCompleted(t, env.store, "wf-1", "alpha")
	seedPersonaCompleted(t, env.store, "wf-1", "beta")

	result := env.startForRecovery(t)

	// Allow a brief period for any erroneously dispatched handlers.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if alphaCount != 0 {
		t.Errorf("alpha should NOT be re-dispatched (already completed), got %d calls", alphaCount)
	}
	if result.Recovered != 0 {
		t.Errorf("expected 0 recovered (all handlers done), got %d", result.Recovered)
	}
}
