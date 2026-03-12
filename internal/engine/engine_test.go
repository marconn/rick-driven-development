package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// === Test Helpers ===

// stubHandler implements handler.Handler for testing.
type stubHandler struct {
	name   string
	subs   []event.Type
	handle func(ctx context.Context, env event.Envelope) ([]event.Envelope, error)
}

func (s *stubHandler) Name() string                  { return s.name }
func (s *stubHandler) Subscribes() []event.Type      { return s.subs }
func (s *stubHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	return s.handle(ctx, env)
}

func newTestEngine(t *testing.T) (*Engine, eventstore.Store, eventbus.Bus) {
	t.Helper()
	store, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	bus := eventbus.NewChannelBus()
	logger := slog.Default()
	eng := NewEngine(store, bus, logger)
	t.Cleanup(func() {
		eng.Stop()
		_ = bus.Close()
		_ = store.Close()
	})
	return eng, store, bus
}

// === Engine Lifecycle Tests ===

func TestEngineRegisterWorkflow(t *testing.T) {
	eng, _, _ := newTestEngine(t)
	eng.RegisterWorkflow(WorkspaceDevWorkflowDef())
	if _, ok := eng.workflows["workspace-dev"]; !ok {
		t.Error("workflow should be registered")
	}
}

func TestEngineStartStop(t *testing.T) {
	eng, _, _ := newTestEngine(t)
	eng.Start()
	eng.Stop()
}

func TestEngineProcessDecisionWorkflowRequested(t *testing.T) {
	eng, store, bus := newTestEngine(t)
	ctx := context.Background()
	eng.RegisterWorkflow(WorkflowDef{ID: "wf-dag", Required: []string{"developer"}, MaxIterations: 3})

	reqEvt := event.New(event.WorkflowRequested, 1,
		event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "build", WorkflowID: "wf-dag"})).
		WithAggregate("wf-proc", 1).WithCorrelation("corr-proc")
	if err := store.Append(ctx, "wf-proc", 0, []event.Envelope{reqEvt}); err != nil {
		t.Fatalf("store append: %v", err)
	}

	published := make(chan event.Envelope, 10)
	bus.SubscribeAll(func(_ context.Context, env event.Envelope) error {
		published <- env
		return nil
	})

	if err := eng.processDecision(ctx, reqEvt); err != nil {
		t.Fatalf("processDecision: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	select {
	case env := <-published:
		if !event.IsWorkflowStarted(env.Type) {
			t.Errorf("expected WorkflowStarted variant, got %s", env.Type)
		}
	default:
		t.Error("expected at least one published event")
	}
}

// === Aggregate Tests ===

func TestAggregateApplyWorkflowLifecycle(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")

	agg.Apply(event.Envelope{
		Version: 1, Type: event.WorkflowRequested,
		Payload: event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "build", WorkflowID: "workspace-dev"}),
	})
	if agg.Status != StatusRequested {
		t.Errorf("expected requested, got %s", agg.Status)
	}
	if agg.Prompt != "build" {
		t.Errorf("expected prompt 'build', got %s", agg.Prompt)
	}

	agg.Apply(event.Envelope{Version: 2, Type: event.WorkflowStartedFor("test")})
	if agg.Status != StatusRunning {
		t.Errorf("expected running, got %s", agg.Status)
	}

	agg.Apply(event.Envelope{Version: 3, Type: event.WorkflowCompleted})
	if agg.Status != StatusCompleted {
		t.Errorf("expected completed, got %s", agg.Status)
	}
}

func TestAggregateApplyWorkflowFailed(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Apply(event.Envelope{Version: 1, Type: event.WorkflowFailed})
	if agg.Status != StatusFailed {
		t.Errorf("expected failed, got %s", agg.Status)
	}
}

func TestAggregateApplyWorkflowCancelled(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Apply(event.Envelope{Version: 1, Type: event.WorkflowCancelled})
	if agg.Status != StatusCancelled {
		t.Errorf("expected cancelled, got %s", agg.Status)
	}
}

func TestAggregateApplyPersonaCompleted(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Apply(event.Envelope{
		Version: 1, Type: event.PersonaCompleted,
		Payload: event.MustMarshal(event.PersonaCompletedPayload{Persona: "developer"}),
	})
	if !agg.CompletedPersonas["developer"] {
		t.Error("developer should be in CompletedPersonas")
	}
	agg.Apply(event.Envelope{
		Version: 2, Type: event.PersonaCompleted,
		Payload: event.MustMarshal(event.PersonaCompletedPayload{Persona: "reviewer"}),
	})
	if !agg.CompletedPersonas["reviewer"] {
		t.Error("reviewer should be in CompletedPersonas")
	}
}

func TestAggregateApplyFeedbackResetsPersonas(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.CompletedPersonas["developer"] = true
	agg.CompletedPersonas["reviewer"] = true

	agg.Apply(event.Envelope{
		Version: 1, Type: event.FeedbackGenerated,
		Payload: event.MustMarshal(event.FeedbackGeneratedPayload{
			TargetPhase: "developer",
			SourcePhase: "reviewer",
			Iteration:   1,
		}),
	})

	if agg.CompletedPersonas["developer"] {
		t.Error("developer should be cleared after feedback")
	}
	if agg.CompletedPersonas["reviewer"] {
		t.Error("reviewer (source) should be cleared after feedback")
	}
	if agg.FeedbackCount["developer"] != 1 {
		t.Errorf("expected FeedbackCount=1, got %d", agg.FeedbackCount["developer"])
	}
}

func TestAggregateTokenTracking(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Apply(event.Envelope{
		Version: 1, Type: event.AIResponseReceived,
		Payload: event.MustMarshal(event.AIResponsePayload{TokensUsed: 500}),
	})
	agg.Apply(event.Envelope{
		Version: 2, Type: event.AIResponseReceived,
		Payload: event.MustMarshal(event.AIResponsePayload{TokensUsed: 300}),
	})
	if agg.TokensUsed != 800 {
		t.Errorf("expected 800, got %d", agg.TokensUsed)
	}
}

func TestAggregateVersionTracking(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Apply(event.Envelope{Version: 5, Type: event.WorkflowStartedFor("test")})
	if agg.Version != 5 {
		t.Errorf("expected 5, got %d", agg.Version)
	}
}

func TestAggregateApplyUnknownEventsAreNoOps(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning

	// Unknown event types should not crash Apply — they're no-ops for version tracking only
	agg.Apply(event.Envelope{Version: 1, Type: "some.unknown.event"})
	agg.Apply(event.Envelope{Version: 2, Type: "another.unknown"})

	if agg.Version != 2 {
		t.Errorf("expected version 2, got %d", agg.Version)
	}
	if agg.Status != StatusRunning {
		t.Errorf("unknown events should not change status, got %s", agg.Status)
	}
}

// === Decide Tests ===

func TestAggregateDecideWorkflowRequestedNoDef(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	_, err := agg.Decide(event.Envelope{
		Type:    event.WorkflowRequested,
		Payload: event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "test"}),
	})
	if err == nil {
		t.Error("expected error when WorkflowDef is nil")
	}
}

func TestAggregateDecideWorkflowRequested(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.WorkflowID = "workspace-dev"
	agg.WorkflowDef = &WorkflowDef{ID: "workspace-dev", Required: []string{"developer"}, MaxIterations: 3}

	env := event.Envelope{
		ID: "evt-1", Type: event.WorkflowRequested,
		AggregateID: "wf-1", CorrelationID: "corr-1",
		Payload: event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "test", WorkflowID: "workspace-dev"}),
	}
	events, err := agg.Decide(env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 || !event.IsWorkflowStarted(events[0].Type) {
		t.Errorf("expected [WorkflowStarted variant], got %v", events)
	}
}

// TestAggregateDecideWorkflowRequestedAlreadyCancelled verifies that a
// WorkflowRequested event does not emit WorkflowStarted if the workflow
// was cancelled before the Engine processed it (cancel-before-start race).
func TestAggregateDecideWorkflowRequestedAlreadyCancelled(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.WorkflowID = "workspace-dev"
	agg.WorkflowDef = &WorkflowDef{ID: "workspace-dev", Required: []string{"developer"}, MaxIterations: 3}
	// Simulate: WorkflowRequested applied, then WorkflowCancelled applied before
	// the Engine's Decide runs.
	agg.Apply(event.Envelope{Type: event.WorkflowRequested, Version: 1,
		Payload: event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "test"})})
	agg.Apply(event.Envelope{Type: event.WorkflowCancelled, Version: 2,
		Payload: event.MustMarshal(event.WorkflowCancelledPayload{Reason: "pre-emptive"})})

	if agg.Status != StatusCancelled {
		t.Fatalf("expected cancelled, got %s", agg.Status)
	}

	events, err := agg.Decide(event.Envelope{
		Type:    event.WorkflowRequested,
		Payload: event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "test"}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected no events (workflow already cancelled), got %d: %v", len(events), events)
	}
}

func TestAggregateDecidePersonaCompletedPartial(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning
	agg.WorkflowDef = &WorkflowDef{Required: []string{"developer", "reviewer"}}
	agg.CompletedPersonas["developer"] = true // only one of two

	events, err := agg.Decide(event.Envelope{
		Type: event.PersonaCompleted, AggregateID: "wf-1", CorrelationID: "corr-1",
		Payload: event.MustMarshal(event.PersonaCompletedPayload{Persona: "developer"}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected no events (not all required done), got %d", len(events))
	}
}

func TestAggregateDecidePersonaCompletedAll(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning
	agg.WorkflowDef = &WorkflowDef{Required: []string{"developer", "reviewer"}}
	agg.CompletedPersonas["developer"] = true
	agg.CompletedPersonas["reviewer"] = true

	events, err := agg.Decide(event.Envelope{
		Type: event.PersonaCompleted, AggregateID: "wf-1", CorrelationID: "corr-1",
		Payload: event.MustMarshal(event.PersonaCompletedPayload{Persona: "reviewer"}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 || events[0].Type != event.WorkflowCompleted {
		t.Fatalf("expected [WorkflowCompleted], got %v", events)
	}
}

func TestAggregateDecidePersonaFailed(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning
	agg.WorkflowDef = &WorkflowDef{Required: []string{"developer"}}

	events, err := agg.Decide(event.Envelope{
		Type: event.PersonaFailed, AggregateID: "wf-1", CorrelationID: "corr-1",
		Payload: event.MustMarshal(event.PersonaFailedPayload{Persona: "developer", Error: "build failed"}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 || events[0].Type != event.WorkflowFailed {
		t.Fatalf("expected [WorkflowFailed], got %v", events)
	}
	var p event.WorkflowFailedPayload
	_ = json.Unmarshal(events[0].Payload, &p)
	if p.Phase != "developer" {
		t.Errorf("expected phase=developer, got %s", p.Phase)
	}
}

func TestAggregateDecidePersonaFailedNonRequired(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning
	agg.WorkflowDef = &WorkflowDef{Required: []string{"developer", "reviewer"}}

	// A non-required handler failure (e.g., before-hook) should NOT fail the workflow.
	events, err := agg.Decide(event.Envelope{
		Type: event.PersonaFailed, AggregateID: "wf-1", CorrelationID: "corr-1",
		Payload: event.MustMarshal(event.PersonaFailedPayload{Persona: "enricher", Error: "boom"}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("non-required persona failure should not emit events, got %d", len(events))
	}
}

func TestAggregateDecidePersonaFailedAlreadyTerminal(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusCompleted
	agg.WorkflowDef = &WorkflowDef{Required: []string{"developer"}}

	// A PersonaFailed on an already-completed workflow should be a no-op.
	events, err := agg.Decide(event.Envelope{
		Type: event.PersonaFailed,
		Payload: event.MustMarshal(event.PersonaFailedPayload{Persona: "developer", Error: "stale"}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("PersonaFailed on completed workflow should be no-op, got %d events", len(events))
	}
}

func TestAggregateDecideVerdictFailNonRequiredPhase(t *testing.T) {
	// Review-only workflows: VerdictRendered{fail} targeting a non-existent phase
	// should be informational only — no FeedbackGenerated.
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning
	agg.MaxIterations = 1
	agg.WorkflowDef = &WorkflowDef{Required: []string{"pr-architect", "pr-reviewer", "pr-qa"}, MaxIterations: 1}

	events, err := agg.Decide(event.Envelope{
		Type: event.VerdictRendered,
		Payload: event.MustMarshal(event.VerdictPayload{
			Phase: "developer", SourcePhase: "pr-reviewer", Outcome: event.VerdictFail,
		}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("verdict on non-required phase should be no-op, got %d events (%s)", len(events), events[0].Type)
	}
}

func TestAggregateDecideVerdictFail(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning
	agg.MaxIterations = 3
	agg.WorkflowDef = &WorkflowDef{
		Required:      []string{"developer", "reviewer"},
		MaxIterations: 3,
		PhaseMap:       corePhaseMap,
	}

	env := event.Envelope{
		ID: "verdict-1", Type: event.VerdictRendered,
		AggregateID: "wf-1", CorrelationID: "corr-1",
		Payload: event.MustMarshal(event.VerdictPayload{
			Phase: "develop", SourcePhase: "review",
			Outcome: event.VerdictFail, Summary: "needs work",
			Issues: []event.Issue{{Severity: "major", Description: "missing error handling"}},
		}),
	}

	events, err := agg.Decide(env)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if len(events) != 1 || events[0].Type != event.FeedbackGenerated {
		t.Fatalf("expected [FeedbackGenerated], got %v", events)
	}

	var fb event.FeedbackGeneratedPayload
	_ = json.Unmarshal(events[0].Payload, &fb)
	// FeedbackGenerated should use persona names, not phase verbs.
	if fb.TargetPhase != "developer" {
		t.Errorf("expected target=developer, got %s", fb.TargetPhase)
	}
	if fb.SourcePhase != "reviewer" {
		t.Errorf("expected source=reviewer, got %s", fb.SourcePhase)
	}
}

func TestAggregateDecideVerdictFailMaxIterations(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning
	agg.MaxIterations = 2
	agg.WorkflowDef = &WorkflowDef{Required: []string{"developer"}, MaxIterations: 2, PhaseMap: corePhaseMap}
	agg.FeedbackCount["developer"] = 2 // already at max

	env := event.Envelope{
		Type: event.VerdictRendered, AggregateID: "wf-1", CorrelationID: "corr-1",
		Payload: event.MustMarshal(event.VerdictPayload{
			Phase: "develop", Outcome: event.VerdictFail, Summary: "still bad",
		}),
	}

	events, err := agg.Decide(env)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if len(events) != 1 || events[0].Type != event.WorkflowFailed {
		t.Fatalf("expected [WorkflowFailed], got %v", events)
	}
}

func TestAggregateDecideVerdictPass(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	events, err := agg.Decide(event.Envelope{
		Type: event.VerdictRendered,
		Payload: event.MustMarshal(event.VerdictPayload{
			Phase: "develop", Outcome: event.VerdictPass,
		}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("VerdictPass should produce no events, got %d", len(events))
	}
}

func TestAggregateDecideTokenBudgetExceeded(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	events, err := agg.Decide(event.Envelope{
		ID: "budget-1", Type: event.TokenBudgetExceeded,
		AggregateID: "wf-1", CorrelationID: "corr-1",
		Payload: event.MustMarshal(event.TokenBudgetExceededPayload{TotalUsed: 10000, Budget: 8000}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 || events[0].Type != event.WorkflowFailed {
		t.Fatalf("expected [WorkflowFailed], got %v", events)
	}
}

func TestAggregateDecideUnknownEvent(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	events, err := agg.Decide(event.Envelope{Type: event.AIResponseReceived})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("unknown event should produce no decisions, got %d", len(events))
	}
}

// === Aggregate Resolution + Tracking Tests ===

func TestResolveWorkflowAggregateID(t *testing.T) {
	eng, _, _ := newTestEngine(t)

	// WorkflowRequested uses AggregateID directly
	env := event.Envelope{Type: event.WorkflowRequested, AggregateID: "wf-1", CorrelationID: "wf-1"}
	if got := eng.resolveWorkflowAggregateID(env); got != "wf-1" {
		t.Errorf("WorkflowRequested: want wf-1, got %s", got)
	}

	// PersonaCompleted from persona-scoped aggregate resolves via CorrelationID
	env = event.Envelope{Type: event.PersonaCompleted, AggregateID: "wf-1:persona:dev", CorrelationID: "wf-1"}
	if got := eng.resolveWorkflowAggregateID(env); got != "wf-1" {
		t.Errorf("PersonaCompleted: want wf-1, got %s", got)
	}

	// PersonaFailed resolves via CorrelationID
	env = event.Envelope{Type: event.PersonaFailed, AggregateID: "wf-1:persona:dev", CorrelationID: "wf-1"}
	if got := eng.resolveWorkflowAggregateID(env); got != "wf-1" {
		t.Errorf("PersonaFailed: want wf-1, got %s", got)
	}

	// VerdictRendered resolves via CorrelationID
	env = event.Envelope{Type: event.VerdictRendered, AggregateID: "wf-1:persona:reviewer", CorrelationID: "wf-1"}
	if got := eng.resolveWorkflowAggregateID(env); got != "wf-1" {
		t.Errorf("VerdictRendered: want wf-1, got %s", got)
	}
}

func TestEngineTracksPersonaCompletionOnWorkflow(t *testing.T) {
	eng, store, bus := newTestEngine(t)
	ctx := context.Background()

	def := WorkflowDef{ID: "test-wf", Required: []string{"developer", "reviewer"}, MaxIterations: 3}
	eng.RegisterWorkflow(def)

	// Seed the workflow aggregate: WorkflowRequested + WorkflowStarted
	wfID := "wf-track-test"
	reqEvt := event.New(event.WorkflowRequested, 1,
		event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "build", WorkflowID: "test-wf"})).
		WithAggregate(wfID, 1).WithCorrelation(wfID)
	startEvt := event.New(event.WorkflowStartedFor("test-wf"), 1,
		event.MustMarshal(event.WorkflowStartedPayload{WorkflowID: "test-wf"})).
		WithAggregate(wfID, 2).WithCorrelation(wfID)
	if err := store.Append(ctx, wfID, 0, []event.Envelope{reqEvt, startEvt}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Collect published events
	published := make(chan event.Envelope, 10)
	bus.SubscribeAll(func(_ context.Context, env event.Envelope) error {
		published <- env
		return nil
	})

	// Simulate PersonaCompleted{developer} from a persona-scoped aggregate
	devCompleted := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer", ChainDepth: 0,
	})).
		WithAggregate(wfID+":persona:developer", 1).
		WithCorrelation(wfID)

	if err := eng.processDecision(ctx, devCompleted); err != nil {
		t.Fatalf("processDecision (developer): %v", err)
	}

	// Verify tracking event stored on workflow aggregate
	events, err := store.Load(ctx, wfID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var tracked bool
	for _, e := range events {
		if e.Type == event.PersonaTracked {
			var pc event.PersonaCompletedPayload
			_ = json.Unmarshal(e.Payload, &pc)
			if pc.Persona == "developer" && e.AggregateID == wfID {
				tracked = true
			}
		}
	}
	if !tracked {
		t.Fatal("PersonaTracked{developer} should be tracked on workflow aggregate")
	}

	// Not all required done yet — no WorkflowCompleted should be published
	time.Sleep(50 * time.Millisecond)
	select {
	case env := <-published:
		t.Fatalf("should not publish any events yet, got %s", env.Type)
	default:
		// Expected
	}

	// Now send PersonaCompleted{reviewer}
	revCompleted := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "reviewer", ChainDepth: 0,
	})).
		WithAggregate(wfID+":persona:reviewer", 1).
		WithCorrelation(wfID)

	if err := eng.processDecision(ctx, revCompleted); err != nil {
		t.Fatalf("processDecision (reviewer): %v", err)
	}

	// Now all required done — WorkflowCompleted should be published
	time.Sleep(50 * time.Millisecond)
	select {
	case env := <-published:
		if env.Type != event.WorkflowCompleted {
			t.Errorf("expected WorkflowCompleted, got %s", env.Type)
		}
		if env.AggregateID != wfID {
			t.Errorf("expected aggregate ID %s, got %s", wfID, env.AggregateID)
		}
	default:
		t.Fatal("expected WorkflowCompleted after all required personas done")
	}
}

func TestEngineTrackingIdempotent(t *testing.T) {
	eng, store, _ := newTestEngine(t)
	ctx := context.Background()

	def := WorkflowDef{ID: "test-wf", Required: []string{"developer"}, MaxIterations: 3}
	eng.RegisterWorkflow(def)

	wfID := "wf-idemp"
	reqEvt := event.New(event.WorkflowRequested, 1,
		event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "build", WorkflowID: "test-wf"})).
		WithAggregate(wfID, 1).WithCorrelation(wfID)
	startEvt := event.New(event.WorkflowStartedFor("test-wf"), 1,
		event.MustMarshal(event.WorkflowStartedPayload{WorkflowID: "test-wf"})).
		WithAggregate(wfID, 2).WithCorrelation(wfID)
	if err := store.Append(ctx, wfID, 0, []event.Envelope{reqEvt, startEvt}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	devCompleted := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer",
	})).
		WithAggregate(wfID+":persona:developer", 1).
		WithCorrelation(wfID)

	// First call tracks + emits WorkflowCompleted
	if err := eng.processDecision(ctx, devCompleted); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Second call with same persona — tracking is idempotent (already tracked)
	if err := eng.processDecision(ctx, devCompleted); err != nil {
		t.Fatalf("second call should be idempotent: %v", err)
	}
}

func TestEngineVerdictFromPersonaScopedAggregate(t *testing.T) {
	eng, store, bus := newTestEngine(t)
	ctx := context.Background()

	def := WorkflowDef{ID: "test-wf", Required: []string{"developer", "reviewer"}, MaxIterations: 3}
	eng.RegisterWorkflow(def)

	wfID := "wf-verdict"
	reqEvt := event.New(event.WorkflowRequested, 1,
		event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "build", WorkflowID: "test-wf"})).
		WithAggregate(wfID, 1).WithCorrelation(wfID)
	startEvt := event.New(event.WorkflowStartedFor("test-wf"), 1,
		event.MustMarshal(event.WorkflowStartedPayload{WorkflowID: "test-wf"})).
		WithAggregate(wfID, 2).WithCorrelation(wfID)
	if err := store.Append(ctx, wfID, 0, []event.Envelope{reqEvt, startEvt}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	published := make(chan event.Envelope, 10)
	bus.SubscribeAll(func(_ context.Context, env event.Envelope) error {
		published <- env
		return nil
	})

	// VerdictRendered from reviewer's persona-scoped aggregate
	verdict := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase: "developer", SourcePhase: "reviewer", Outcome: event.VerdictFail, Summary: "needs work",
	})).
		WithAggregate(wfID+":persona:reviewer", 5).
		WithCorrelation(wfID)

	if err := eng.processDecision(ctx, verdict); err != nil {
		t.Fatalf("processDecision: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	select {
	case env := <-published:
		if env.Type != event.FeedbackGenerated {
			t.Errorf("expected FeedbackGenerated, got %s", env.Type)
		}
		if env.AggregateID != wfID {
			t.Errorf("expected aggregate %s, got %s", wfID, env.AggregateID)
		}
	default:
		t.Fatal("expected FeedbackGenerated from VerdictRendered{fail}")
	}
}

