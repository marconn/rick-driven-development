package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// stubHintHandler implements Handler + TriggeredHandler + Hinter.
type stubHintHandler struct {
	stubHandler
	trigger  handler.Trigger
	hintFn   func(context.Context, event.Envelope) ([]event.Envelope, error)
}

func (s *stubHintHandler) Trigger() handler.Trigger { return s.trigger }
func (s *stubHintHandler) Hint(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	if s.hintFn != nil {
		return s.hintFn(ctx, env)
	}
	return nil, nil
}

// --- Aggregate Tests ---

func TestAggregate_HintEmitted_AutoApprove(t *testing.T) {
	agg := NewWorkflowAggregate("wf-hint")
	agg.Status = StatusRunning
	agg.WorkflowDef = &WorkflowDef{
		ID:            "test",
		Required:      []string{"developer"},
		HintThreshold: 0.5,
	}

	hintEvt := event.New(event.HintEmitted, 1, event.MustMarshal(event.HintEmittedPayload{
		Persona:    "developer",
		Phase:      "develop",
		TriggerID:  "trigger-1",
		Confidence: 0.8,
	})).WithAggregate("wf-hint", 2).WithCorrelation("wf-hint")

	results, err := agg.Decide(hintEvt)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 event, got %d", len(results))
	}
	if results[0].Type != event.HintApproved {
		t.Errorf("expected HintApproved, got %s", results[0].Type)
	}

	var p event.HintApprovedPayload
	_ = json.Unmarshal(results[0].Payload, &p)
	if p.Persona != "developer" {
		t.Errorf("expected persona developer, got %s", p.Persona)
	}
	if p.TriggerID != "trigger-1" {
		t.Errorf("expected trigger_id trigger-1, got %s", p.TriggerID)
	}
}

func TestAggregate_HintEmitted_LowConfidence_Pauses(t *testing.T) {
	agg := NewWorkflowAggregate("wf-hint-low")
	agg.Status = StatusRunning
	agg.WorkflowDef = &WorkflowDef{
		ID:            "test",
		Required:      []string{"developer"},
		HintThreshold: 0.7,
	}

	hintEvt := event.New(event.HintEmitted, 1, event.MustMarshal(event.HintEmittedPayload{
		Persona:    "developer",
		Confidence: 0.3,
	})).WithAggregate("wf-hint-low", 2).WithCorrelation("wf-hint-low")

	results, err := agg.Decide(hintEvt)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 event, got %d", len(results))
	}
	if results[0].Type != event.WorkflowPaused {
		t.Errorf("expected WorkflowPaused, got %s", results[0].Type)
	}
}

func TestAggregate_HintEmitted_Blockers_Pauses(t *testing.T) {
	agg := NewWorkflowAggregate("wf-hint-block")
	agg.Status = StatusRunning
	agg.WorkflowDef = &WorkflowDef{
		ID:            "test",
		Required:      []string{"developer"},
		HintThreshold: 0.5,
	}

	hintEvt := event.New(event.HintEmitted, 1, event.MustMarshal(event.HintEmittedPayload{
		Persona:    "developer",
		Confidence: 0.9,
		Blockers:   []string{"missing API schema"},
	})).WithAggregate("wf-hint-block", 2).WithCorrelation("wf-hint-block")

	results, _ := agg.Decide(hintEvt)
	if results[0].Type != event.WorkflowPaused {
		t.Errorf("expected WorkflowPaused (blockers), got %s", results[0].Type)
	}
}

func TestAggregate_HintEmitted_DefaultThreshold(t *testing.T) {
	agg := NewWorkflowAggregate("wf-def")
	agg.Status = StatusRunning
	agg.WorkflowDef = &WorkflowDef{ID: "test", Required: []string{"a"}}

	// 0.65 < default 0.7 → pause.
	low := event.New(event.HintEmitted, 1, event.MustMarshal(event.HintEmittedPayload{
		Persona: "a", Confidence: 0.65,
	})).WithAggregate("wf-def", 2).WithCorrelation("wf-def")

	r1, _ := agg.Decide(low)
	if r1[0].Type != event.WorkflowPaused {
		t.Errorf("0.65 should pause (default 0.7), got %s", r1[0].Type)
	}

	// 0.75 > default 0.7 → approve.
	high := event.New(event.HintEmitted, 1, event.MustMarshal(event.HintEmittedPayload{
		Persona: "a", Confidence: 0.75,
	})).WithAggregate("wf-def", 3).WithCorrelation("wf-def")

	r2, _ := agg.Decide(high)
	if r2[0].Type != event.HintApproved {
		t.Errorf("0.75 should approve (default 0.7), got %s", r2[0].Type)
	}
}

func TestAggregate_HintRejected_Skip(t *testing.T) {
	agg := NewWorkflowAggregate("wf-skip")
	agg.Status = StatusRunning
	agg.WorkflowDef = &WorkflowDef{ID: "test", Required: []string{"developer"}}

	rejEvt := event.New(event.HintRejected, 1, event.MustMarshal(event.HintRejectedPayload{
		Persona: "developer", Reason: "not needed", Action: "skip",
	})).WithAggregate("wf-skip", 2).WithCorrelation("wf-skip")

	results, err := agg.Decide(rejEvt)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if results[0].Type != event.PersonaCompleted {
		t.Errorf("expected PersonaCompleted (skip), got %s", results[0].Type)
	}
	var p event.PersonaCompletedPayload
	_ = json.Unmarshal(results[0].Payload, &p)
	if p.Persona != "developer" {
		t.Errorf("expected persona developer, got %s", p.Persona)
	}
}

func TestAggregate_HintRejected_Fail(t *testing.T) {
	agg := NewWorkflowAggregate("wf-fail")
	agg.Status = StatusRunning
	agg.WorkflowDef = &WorkflowDef{ID: "test", Required: []string{"developer"}}

	rejEvt := event.New(event.HintRejected, 1, event.MustMarshal(event.HintRejectedPayload{
		Persona: "developer", Reason: "critical", Action: "fail",
	})).WithAggregate("wf-fail", 2).WithCorrelation("wf-fail")

	results, _ := agg.Decide(rejEvt)
	if results[0].Type != event.WorkflowFailed {
		t.Errorf("expected WorkflowFailed, got %s", results[0].Type)
	}
}

func TestAggregate_HintEmitted_NotRunning(t *testing.T) {
	agg := NewWorkflowAggregate("wf-p")
	agg.Status = StatusPaused
	agg.WorkflowDef = &WorkflowDef{ID: "test", Required: []string{"a"}}

	hintEvt := event.New(event.HintEmitted, 1, event.MustMarshal(event.HintEmittedPayload{
		Persona: "a", Confidence: 0.9,
	})).WithAggregate("wf-p", 2).WithCorrelation("wf-p")

	results, _ := agg.Decide(hintEvt)
	if len(results) != 0 {
		t.Errorf("expected no events for paused workflow, got %d", len(results))
	}
}

// --- PersonaRunner Integration Tests ---

func newHintTestEnv(t *testing.T) (*Engine, eventbus.Bus, *PersonaRunner, *handler.Registry) {
	t.Helper()
	eng, store, bus := newTestEngine(t)
	reg := handler.NewRegistry()
	dispatcher := NewLocalDispatcher(reg)
	runner := NewPersonaRunner(store, bus, dispatcher, slog.Default(),
		WithMaxChainDepth(7),
	)
	return eng, bus, runner, reg
}

func TestPersonaRunner_HintThenApprove(t *testing.T) {
	eng, bus, runner, reg := newHintTestEnv(t)

	hintCalled := make(chan struct{}, 1)
	handleCalled := make(chan struct{}, 1)

	h := &stubHintHandler{
		stubHandler: stubHandler{
			name: "hinting-dev",
			subs: []event.Type{event.WorkflowStartedFor("hint-test")},
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				handleCalled <- struct{}{}
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("hint-test")}},
		hintFn: func(_ context.Context, env event.Envelope) ([]event.Envelope, error) {
			hintCalled <- struct{}{}
			return []event.Envelope{
				event.New(event.HintEmitted, 1, event.MustMarshal(event.HintEmittedPayload{
					Persona:    "hinting-dev",
					Phase:      "develop",
					TriggerID:  string(env.ID),
					Confidence: 0.9,
				})),
			}, nil
		},
	}

	if err := reg.Register(h); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	def := WorkflowDef{
		ID:            "hint-test",
		Required:      []string{"hinting-dev"},
		HintThreshold: 0.7,
	}
	eng.RegisterWorkflow(def)
	eng.Start()

	runner.Start(ctx, reg)
	defer func() { _ = runner.Close() }()

	// Fire workflow via store + publish (same as engine_test pattern).
	wfID := "wf-hint-e2e"
	store := eng.store
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "test",
		WorkflowID: "hint-test",
	})).WithAggregate(wfID, 1).WithCorrelation(wfID).WithSource("test")

	if err := store.Append(ctx, wfID, 0, []event.Envelope{reqEvt}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := bus.Publish(ctx, reqEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Hint should be called first.
	select {
	case <-hintCalled:
		t.Log("hint called")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for hint")
	}

	// Handle should be called after auto-approval.
	select {
	case <-handleCalled:
		t.Log("handle called after hint approval")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for handle after hint approval")
	}
}

func TestPersonaRunner_HintLowConfidence_Pauses(t *testing.T) {
	eng, bus, runner, reg := newHintTestEnv(t)

	hintCalled := make(chan struct{}, 1)
	handleCalled := make(chan struct{}, 1)

	h := &stubHintHandler{
		stubHandler: stubHandler{
			name: "cautious-dev",
			subs: []event.Type{event.WorkflowStartedFor("hint-pause")},
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				handleCalled <- struct{}{}
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("hint-pause")}},
		hintFn: func(_ context.Context, env event.Envelope) ([]event.Envelope, error) {
			hintCalled <- struct{}{}
			return []event.Envelope{
				event.New(event.HintEmitted, 1, event.MustMarshal(event.HintEmittedPayload{
					Persona:    "cautious-dev",
					Phase:      "develop",
					TriggerID:  string(env.ID),
					Confidence: 0.2,
					Blockers:   []string{"missing requirements"},
				})),
			}, nil
		},
	}

	if err := reg.Register(h); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	eng.RegisterWorkflow(WorkflowDef{
		ID: "hint-pause", Required: []string{"cautious-dev"}, HintThreshold: 0.7,
	})
	eng.Start()

	pausedCh := make(chan event.Envelope, 1)
	unsub := bus.Subscribe(event.WorkflowPaused, func(_ context.Context, env event.Envelope) error {
		if env.CorrelationID == "wf-hint-pause" {
			pausedCh <- env
		}
		return nil
	}, eventbus.WithName("test:paused"))
	defer unsub()

	runner.Start(ctx, reg)
	defer func() { _ = runner.Close() }()

	wfID := "wf-hint-pause"
	store := eng.store
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "hint-pause",
	})).WithAggregate(wfID, 1).WithCorrelation(wfID).WithSource("test")

	_ = store.Append(ctx, wfID, 0, []event.Envelope{reqEvt})
	_ = bus.Publish(ctx, reqEvt)

	select {
	case <-hintCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for hint")
	}

	select {
	case env := <-pausedCh:
		var p event.WorkflowPausedPayload
		_ = json.Unmarshal(env.Payload, &p)
		t.Logf("workflow paused: %s", p.Reason)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for WorkflowPaused")
	}

	// Handle should NOT have been called.
	select {
	case <-handleCalled:
		t.Error("Handle called despite low-confidence hint")
	default:
	}
}

// TestPersonaRunner_HintApproved_ErrIncomplete verifies that a handler returning
// ErrIncomplete through the hint-approved dispatch path persists result events
// but does NOT emit PersonaCompleted.
func TestPersonaRunner_HintApproved_ErrIncomplete(t *testing.T) {
	eng, bus, runner, reg := newHintTestEnv(t)

	hintCalled := make(chan struct{}, 1)
	handleCalled := make(chan struct{}, 1)
	enrichmentCh := make(chan event.Envelope, 1)

	h := &stubHintHandler{
		stubHandler: stubHandler{
			name: "task-dispatcher",
			subs: []event.Type{event.WorkflowStartedFor("hint-incomplete")},
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				handleCalled <- struct{}{}
				enrichment := event.New("context.enrichment", 1,
					event.MustMarshal(event.ContextEnrichmentPayload{
						Source: "dispatcher", Kind: "batch-status", Summary: "dispatched",
					}))
				return []event.Envelope{enrichment}, handler.ErrIncomplete
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("hint-incomplete")}},
		hintFn: func(_ context.Context, env event.Envelope) ([]event.Envelope, error) {
			hintCalled <- struct{}{}
			return []event.Envelope{
				event.New(event.HintEmitted, 1, event.MustMarshal(event.HintEmittedPayload{
					Persona:    "task-dispatcher",
					Phase:      "dispatch",
					TriggerID:  string(env.ID),
					Confidence: 0.9,
				})),
			}, nil
		},
	}

	if err := reg.Register(h); err != nil {
		t.Fatal(err)
	}

	// Track bus events — should NOT see PersonaCompleted for task-dispatcher.
	bus.Subscribe("context.enrichment", func(_ context.Context, env event.Envelope) error {
		enrichmentCh <- env
		return nil
	}, eventbus.WithName("test:enrichment"))

	pcCh := make(chan string, 2)
	bus.Subscribe(event.PersonaCompleted, func(_ context.Context, env event.Envelope) error {
		var pc event.PersonaCompletedPayload
		if err := json.Unmarshal(env.Payload, &pc); err == nil {
			pcCh <- pc.Persona
		}
		return nil
	}, eventbus.WithName("test:pc"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	def := WorkflowDef{
		ID:            "hint-incomplete",
		Required:      []string{"task-dispatcher"},
		HintThreshold: 0.7, // auto-approve at 0.9
	}
	eng.RegisterWorkflow(def)
	eng.Start()
	runner.Start(ctx, reg)
	defer func() { _ = runner.Close() }()

	wfID := "wf-hint-incomplete"
	store := eng.store
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "test",
		WorkflowID: "hint-incomplete",
	})).WithAggregate(wfID, 1).WithCorrelation(wfID).WithSource("test")

	if err := store.Append(ctx, wfID, 0, []event.Envelope{reqEvt}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := bus.Publish(ctx, reqEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// 1. Hint should fire first.
	select {
	case <-hintCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for hint")
	}

	// 2. Handle should fire after auto-approval.
	select {
	case <-handleCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for handle after hint approval")
	}

	// 3. Enrichment event should be published (proves handler ran and results persisted).
	select {
	case env := <-enrichmentCh:
		if env.CorrelationID != wfID {
			t.Errorf("expected correlation %s, got %s", wfID, env.CorrelationID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for enrichment event")
	}

	// 4. PersonaCompleted should NOT be emitted for task-dispatcher.
	time.Sleep(300 * time.Millisecond)
	select {
	case persona := <-pcCh:
		t.Errorf("unexpected PersonaCompleted for %s — ErrIncomplete should suppress it", persona)
	default:
		// Expected: no PersonaCompleted
	}
}

func TestPersonaRunner_NonHintHandler_ExecutesDirectly(t *testing.T) {
	eng, bus, runner, reg := newHintTestEnv(t)

	handleCalled := make(chan struct{}, 1)

	h := &stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "plain",
			subs: []event.Type{event.WorkflowStartedFor("no-hint")},
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				handleCalled <- struct{}{}
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("no-hint")}},
	}

	if err := reg.Register(h); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	eng.RegisterWorkflow(WorkflowDef{ID: "no-hint", Required: []string{"plain"}})
	eng.Start()

	runner.Start(ctx, reg)
	defer func() { _ = runner.Close() }()

	wfID := "wf-no-hint"
	store := eng.store
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "no-hint",
	})).WithAggregate(wfID, 1).WithCorrelation(wfID).WithSource("test")

	_ = store.Append(ctx, wfID, 0, []event.Envelope{reqEvt})
	_ = bus.Publish(ctx, reqEvt)

	select {
	case <-handleCalled:
		t.Log("non-hint handler executed directly")
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for direct handle")
	}
}
