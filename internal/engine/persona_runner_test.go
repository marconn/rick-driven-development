package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

func newTestPersonaRunner(t *testing.T, opts ...PersonaRunnerOption) (*PersonaRunner, eventstore.Store, eventbus.Bus, *handler.Registry) {
	t.Helper()
	store, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	bus := eventbus.NewChannelBus()
	reg := handler.NewRegistry()
	dispatcher := NewLocalDispatcher(reg)
	logger := slog.Default()
	runner := NewPersonaRunner(store, bus, dispatcher, logger, opts...)
	t.Cleanup(func() {
		_ = runner.Close()
		_ = bus.Close()
		_ = store.Close()
	})
	return runner, store, bus, reg
}

func TestPersonaRunnerReactiveDispatch(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t)

	// Register a reactive handler that subscribes to PersonaCompleted
	handled := make(chan event.Envelope, 1)
	h := &stubHandler{
		name: "documenter",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, env event.Envelope) ([]event.Envelope, error) {
			handled <- env
			return nil, nil
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	runner.Start(context.Background(), reg)

	// Publish a PersonaCompleted event from "developer"
	triggerEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:      "developer",
		Phase:        "develop",
		TriggerEvent: "phase.scheduled",
		TriggerID:    "evt-1",
		Reactive:     false,
		ChainDepth:   0,
	})).
		WithAggregate("wf-1", 10).
		WithCorrelation("corr-1").
		WithSource("engine:dispatcher")

	ctx := context.Background()
	if err := bus.Publish(ctx, triggerEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case env := <-handled:
		if env.Type != event.PersonaCompleted {
			t.Errorf("expected PersonaCompleted trigger, got %s", env.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reactive handler was not called within timeout")
	}
}


func TestPersonaRunnerSelfTriggerPrevention(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t)

	callCount := 0
	var mu sync.Mutex
	h := &stubHandler{
		name: "documenter",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			mu.Lock()
			callCount++
			mu.Unlock()
			return nil, nil
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	runner.Start(context.Background(), reg)

	// PersonaCompleted from documenter itself — should be skipped
	selfEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:    "documenter",
		Reactive:   true,
		ChainDepth: 0,
	})).WithCorrelation("corr-1")

	if err := bus.Publish(context.Background(), selfEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if callCount != 0 {
		t.Errorf("self-trigger should be prevented, got %d calls", callCount)
	}
}

func TestPersonaRunnerChainDepthLimit(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t, WithMaxChainDepth(2))

	handled := make(chan struct{}, 1)
	h := &stubHandler{
		name: "notifier",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			handled <- struct{}{}
			return nil, nil
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	runner.Start(context.Background(), reg)

	// ChainDepth=2 with maxChain=2 → should be skipped (>= maxChain)
	deepEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:    "pr-monitor",
		Reactive:   true,
		ChainDepth: 2,
	})).WithCorrelation("corr-1")

	if err := bus.Publish(context.Background(), deepEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-handled:
		t.Error("should not dispatch when chain depth >= maxChain")
	case <-time.After(200 * time.Millisecond):
		// Expected
	}
}

func TestPersonaRunnerChainDepthAllowed(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t, WithMaxChainDepth(3))

	handled := make(chan struct{}, 1)
	h := &stubHandler{
		name: "notifier",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			handled <- struct{}{}
			return nil, nil
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	runner.Start(context.Background(), reg)

	// ChainDepth=1 with maxChain=3 → allowed (1+1=2 < 3)
	evt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:    "developer",
		Reactive:   false,
		ChainDepth: 1,
	})).WithCorrelation("corr-1")

	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-handled:
		// Expected
	case <-time.After(2 * time.Second):
		t.Fatal("handler should have been called")
	}
}

func TestPersonaRunnerWidthLimit(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t, WithMaxActive(1))

	// Handler that blocks until released
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	h := &stubHandler{
		name: "slow-handler",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			entered <- struct{}{}
			<-release
			return nil, nil
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	runner.Start(context.Background(), reg)

	// First event — should enter handler
	evt1 := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer", ChainDepth: 0,
	})).WithCorrelation("corr-1")

	// Second event — should be width-limited
	evt2 := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "researcher", ChainDepth: 0,
	})).WithCorrelation("corr-2")

	if err := bus.Publish(context.Background(), evt1); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Wait for first to enter
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first handler never entered")
	}

	// Now publish second while first is active
	if err := bus.Publish(context.Background(), evt2); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Give time for second to be dropped
	time.Sleep(200 * time.Millisecond)

	// Release the first handler
	close(release)

	// Only one should have entered
	time.Sleep(100 * time.Millisecond)
	if len(entered) > 0 {
		t.Error("second handler should have been width-limited")
	}
}

func TestPersonaRunnerEventDedup(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t)

	callCount := 0
	var mu sync.Mutex
	h := &stubHandler{
		name: "documenter",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			mu.Lock()
			callCount++
			mu.Unlock()
			return nil, nil
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	runner.Start(context.Background(), reg)

	// Same event published twice (simulating bus retry)
	evt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer", ChainDepth: 0,
	})).WithCorrelation("corr-1")

	for i := 0; i < 3; i++ {
		if err := bus.Publish(context.Background(), evt); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if callCount != 1 {
		t.Errorf("expected exactly 1 dispatch (dedup), got %d", callCount)
	}
}

func TestPersonaRunnerEmitsPersonaCompleted(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t)

	// Reactive handler returns an AIResponseReceived event
	aiRespEvt := event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
		Phase: "docs", TokensUsed: 100,
	}))
	h := &stubHandler{
		name: "documenter",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return []event.Envelope{aiRespEvt}, nil
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	runner.Start(context.Background(), reg)

	// Collect published events
	published := make(chan event.Envelope, 20)
	bus.SubscribeAll(func(_ context.Context, env event.Envelope) error {
		published <- env
		return nil
	})

	triggerEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer", ChainDepth: 0,
	})).WithCorrelation("corr-1")

	if err := bus.Publish(context.Background(), triggerEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Collect events
	time.Sleep(500 * time.Millisecond)
	var personaEvents []event.Envelope
	for {
		select {
		case env := <-published:
			if env.Type == event.PersonaCompleted {
				var pc event.PersonaCompletedPayload
				if err := json.Unmarshal(env.Payload, &pc); err == nil && pc.Persona == "documenter" {
					personaEvents = append(personaEvents, env)
				}
			}
		default:
			goto done
		}
	}
done:
	if len(personaEvents) == 0 {
		t.Fatal("expected PersonaCompleted event from documenter")
	}

	var pc event.PersonaCompletedPayload
	_ = json.Unmarshal(personaEvents[0].Payload, &pc)
	if !pc.Reactive {
		t.Error("expected Reactive=true")
	}
	if pc.ChainDepth != 1 {
		t.Errorf("expected ChainDepth=1, got %d", pc.ChainDepth)
	}
	if pc.OutputRef == "" {
		t.Error("expected OutputRef to be set from AIResponseReceived")
	}
}

func TestPersonaRunnerEmitsPersonaFailed(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t)

	h := &stubHandler{
		name: "documenter",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return nil, errors.New("template rendering failed")
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	runner.Start(context.Background(), reg)

	published := make(chan event.Envelope, 20)
	bus.SubscribeAll(func(_ context.Context, env event.Envelope) error {
		published <- env
		return nil
	})

	triggerEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer", ChainDepth: 0,
	})).WithCorrelation("corr-1")

	if err := bus.Publish(context.Background(), triggerEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	time.Sleep(500 * time.Millisecond)
	var failEvents []event.Envelope
	for {
		select {
		case env := <-published:
			if env.Type == event.PersonaFailed {
				failEvents = append(failEvents, env)
			}
		default:
			goto done2
		}
	}
done2:
	if len(failEvents) == 0 {
		t.Fatal("expected PersonaFailed event")
	}

	var pf event.PersonaFailedPayload
	_ = json.Unmarshal(failEvents[0].Payload, &pf)
	if pf.Persona != "documenter" {
		t.Errorf("expected persona=documenter, got %s", pf.Persona)
	}
	if pf.Error != "template rendering failed" {
		t.Errorf("unexpected error: %s", pf.Error)
	}
	if !pf.Reactive {
		t.Error("expected Reactive=true")
	}
}

func TestPersonaRunnerDrainTimeout(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t, WithDrainTimeout(100*time.Millisecond))

	// Handler that blocks on its own channel, ignoring context cancellation.
	// This simulates a handler that can't be cancelled cleanly.
	block := make(chan struct{})
	entered := make(chan struct{})
	h := &stubHandler{
		name: "blocker",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			close(entered)
			<-block // blocks until test cleanup
			return nil, nil
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { close(block) })

	runner.Start(context.Background(), reg)

	evt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer", ChainDepth: 0,
	})).WithCorrelation("corr-1")

	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Wait for handler to enter
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never entered")
	}

	// Close should timeout since handler is blocked
	err := runner.Close()
	if err == nil {
		t.Error("expected drain timeout error")
	}
}

func TestPersonaRunnerPersonaScopedAggregate(t *testing.T) {
	runner, store, bus, reg := newTestPersonaRunner(t)

	h := &stubHandler{
		name: "documenter",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return nil, nil
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	runner.Start(context.Background(), reg)

	triggerEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer", ChainDepth: 0,
	})).WithCorrelation("corr-42")

	if err := bus.Publish(context.Background(), triggerEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Verify events stored under persona-scoped aggregate
	ctx := context.Background()
	events, err := store.Load(ctx, "corr-42:persona:documenter")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected events under persona-scoped aggregate")
	}

	// Last event should be PersonaCompleted
	last := events[len(events)-1]
	if last.Type != event.PersonaCompleted {
		t.Errorf("expected PersonaCompleted, got %s", last.Type)
	}
	if last.AggregateID != "corr-42:persona:documenter" {
		t.Errorf("expected persona-scoped aggregate ID, got %s", last.AggregateID)
	}
}

func TestAggregateApplyPersonaEventsNoOp(t *testing.T) {
	agg := NewWorkflowAggregate("wf-1")
	agg.Status = StatusRunning

	// Apply PersonaCompleted — should only update version
	agg.Apply(event.Envelope{
		Version: 5,
		Type:    event.PersonaCompleted,
		Payload: event.MustMarshal(event.PersonaCompletedPayload{Persona: "developer"}),
	})
	if agg.Version != 5 {
		t.Errorf("expected version 5, got %d", agg.Version)
	}
	if agg.Status != StatusRunning {
		t.Errorf("status should still be running, got %s", agg.Status)
	}

	// Apply PersonaFailed — should only update version
	agg.Apply(event.Envelope{
		Version: 6,
		Type:    event.PersonaFailed,
		Payload: event.MustMarshal(event.PersonaFailedPayload{Persona: "documenter"}),
	})
	if agg.Version != 6 {
		t.Errorf("expected version 6, got %d", agg.Version)
	}
	if agg.Status != StatusRunning {
		t.Errorf("status should still be running, got %s", agg.Status)
	}
}

// === Idempotency cache tests ===

func TestIdempotencyCacheBasic(t *testing.T) {
	c := newIdempotencyCache(3)
	if !c.Add("h1", "e1") {
		t.Error("first add should return true")
	}
	if c.Add("h1", "e1") {
		t.Error("duplicate add should return false")
	}
	if !c.Add("h1", "e2") {
		t.Error("different event should return true")
	}
	if !c.Add("h2", "e1") {
		t.Error("different handler same event should return true")
	}
}

func TestIdempotencyCacheEviction(t *testing.T) {
	c := newIdempotencyCache(2)
	c.Add("h", "e1")
	c.Add("h", "e2")
	// Cache is full, next add should evict e1
	c.Add("h", "e3")
	// e1 should be evicted
	if !c.Add("h", "e1") {
		t.Error("e1 should have been evicted and re-addable")
	}
}

// === TriggeredHandler / join condition tests ===

// stubTriggeredHandler is a stubHandler that also implements TriggeredHandler.
type stubTriggeredHandler struct {
	stubHandler
	trigger handler.Trigger
}

func (s *stubTriggeredHandler) Trigger() handler.Trigger { return s.trigger }

// seedPersonaCompleted stores a PersonaCompleted event for the given persona
// under a persona-scoped aggregate with the given correlationID.
func seedPersonaCompleted(t *testing.T, store eventstore.Store, correlationID, persona string) {
	t.Helper()
	aggregateID := correlationID + ":persona:" + persona
	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:    persona,
		Reactive:   false,
		ChainDepth: 0,
	})).
		WithAggregate(aggregateID, 1).
		WithCorrelation(correlationID).
		WithSource("test-seed")
	if err := store.Append(context.Background(), aggregateID, 0, []event.Envelope{env}); err != nil {
		t.Fatalf("seedPersonaCompleted: %v", err)
	}
}

func TestPersonaRunnerJoinConditionMet(t *testing.T) {
	runner, store, bus, reg := newTestPersonaRunner(t)

	handled := make(chan struct{}, 1)
	h := &stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "architect",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				handled <- struct{}{}
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"developer"},
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Seed the required PersonaCompleted{developer} before starting
	seedPersonaCompleted(t, store, "corr-join-met", "developer")

	runner.Start(context.Background(), reg)

	triggerEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer", ChainDepth: 0,
	})).WithCorrelation("corr-join-met")

	if err := bus.Publish(context.Background(), triggerEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-handled:
		// Expected: join condition met, handler fired
	case <-time.After(2 * time.Second):
		t.Fatal("handler should have fired when join condition is met")
	}
}

func TestPersonaRunnerJoinConditionNotMet(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t)

	handled := make(chan struct{}, 1)
	h := &stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "architect",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				handled <- struct{}{}
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"developer"},
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Do NOT seed PersonaCompleted{developer} — join condition is not met
	runner.Start(context.Background(), reg)

	triggerEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "researcher", ChainDepth: 0,
	})).WithCorrelation("corr-join-notmet")

	if err := bus.Publish(context.Background(), triggerEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-handled:
		t.Error("handler must NOT fire when join condition is not met")
	case <-time.After(300 * time.Millisecond):
		// Expected: handler was silently skipped
	}
}

func TestPersonaRunnerJoinMultiplePersonas(t *testing.T) {
	runner, store, bus, reg := newTestPersonaRunner(t)

	handled := make(chan struct{}, 2)
	h := &stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "committer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				handled <- struct{}{}
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted},
			AfterPersonas: []string{"reviewer", "qa"},
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Seed only reviewer — not both required personas
	seedPersonaCompleted(t, store, "corr-multi", "reviewer")
	runner.Start(context.Background(), reg)

	// First trigger: only reviewer present → should NOT fire
	evt1 := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "reviewer", ChainDepth: 0,
	})).WithCorrelation("corr-multi")

	if err := bus.Publish(context.Background(), evt1); err != nil {
		t.Fatalf("publish evt1: %v", err)
	}

	select {
	case <-handled:
		t.Error("handler must NOT fire with only reviewer completed")
	case <-time.After(300 * time.Millisecond):
		// Expected: skipped
	}

	// Seed qa — now both required personas are present
	seedPersonaCompleted(t, store, "corr-multi", "qa")

	// Second trigger: fresh event ID so dedup doesn't suppress it
	evt2 := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "qa", ChainDepth: 0,
	})).WithCorrelation("corr-multi")

	if err := bus.Publish(context.Background(), evt2); err != nil {
		t.Fatalf("publish evt2: %v", err)
	}

	select {
	case <-handled:
		// Expected: both conditions met, handler fires
	case <-time.After(2 * time.Second):
		t.Fatal("handler should have fired once both join conditions are met")
	}
}

func TestPersonaRunnerTriggeredHandlerInterface(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t)

	// stubTriggeredHandler declares Events: [WorkflowStarted] via Trigger(),
	// but Subscribes() returns [PersonaCompleted]. PersonaRunner must use Trigger().
	handled := make(chan event.Type, 2)
	h := &stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "triggered-handler",
			// Subscribes returns a different event type — must be ignored
			subs: []event.Type{event.PersonaCompleted},
			handle: func(_ context.Context, env event.Envelope) ([]event.Envelope, error) {
				handled <- env.Type
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.WorkflowStarted},
			AfterPersonas: []string{}, // no join condition
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	runner.Start(context.Background(), reg)

	// Publish WorkflowStarted — should fire (subscribed via Trigger.Events)
	startEvt := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "wf-trigger-test",
	})).WithCorrelation("corr-trigger")

	if err := bus.Publish(context.Background(), startEvt); err != nil {
		t.Fatalf("publish WorkflowStarted: %v", err)
	}

	select {
	case et := <-handled:
		if et != event.WorkflowStarted {
			t.Errorf("expected WorkflowStarted trigger, got %s", et)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler should have fired on WorkflowStarted (Trigger.Events)")
	}

	// Publish PersonaCompleted — must NOT fire (Subscribes() is ignored)
	pcEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "other", ChainDepth: 0,
	})).WithCorrelation("corr-trigger")

	if err := bus.Publish(context.Background(), pcEvt); err != nil {
		t.Fatalf("publish PersonaCompleted: %v", err)
	}

	select {
	case et := <-handled:
		t.Errorf("handler must NOT fire on PersonaCompleted when Trigger.Events=[WorkflowStarted], got %s", et)
	case <-time.After(300 * time.Millisecond):
		// Expected: not subscribed to PersonaCompleted
	}
}

// === ErrIncomplete tests ===

func TestPersonaRunnerErrIncomplete_PersistsResultsWithoutPersonaCompleted(t *testing.T) {
	runner, store, bus, reg := newTestPersonaRunner(t)

	// Register a handler that returns result events + ErrIncomplete.
	resultEvt := event.New("context.enrichment", 1, []byte(`{"kind":"batch-status"}`))
	h := &stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "task-dispatcher",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				return []event.Envelope{resultEvt}, handler.ErrIncomplete
			},
		},
		trigger: handler.Trigger{
			Events: []event.Type{event.WorkflowStartedFor("test-incomplete")},
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Track bus events to assert no PersonaCompleted/PersonaFailed.
	var busEvents []event.Type
	var mu sync.Mutex
	bus.Subscribe(event.PersonaCompleted, func(_ context.Context, env event.Envelope) error {
		mu.Lock()
		busEvents = append(busEvents, env.Type)
		mu.Unlock()
		return nil
	}, eventbus.WithName("test:pc"))
	bus.Subscribe(event.PersonaFailed, func(_ context.Context, env event.Envelope) error {
		mu.Lock()
		busEvents = append(busEvents, env.Type)
		mu.Unlock()
		return nil
	}, eventbus.WithName("test:pf"))

	enrichmentCh := make(chan event.Envelope, 1)
	bus.Subscribe("context.enrichment", func(_ context.Context, env event.Envelope) error {
		enrichmentCh <- env
		return nil
	}, eventbus.WithName("test:enrichment"))

	ctx := context.Background()
	runner.Start(ctx, reg)

	// Trigger the handler.
	triggerEvt := event.New(event.WorkflowStartedFor("test-incomplete"), 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "test-incomplete",
	})).WithCorrelation("corr-incomplete")

	if err := bus.Publish(ctx, triggerEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Wait for enrichment event to be published (proves handler ran).
	select {
	case env := <-enrichmentCh:
		if env.CorrelationID != "corr-incomplete" {
			t.Errorf("expected correlation corr-incomplete, got %s", env.CorrelationID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: enrichment event not published")
	}

	// Short wait to ensure no PersonaCompleted/Failed arrives.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	if len(busEvents) > 0 {
		t.Errorf("expected no PersonaCompleted/Failed, got: %v", busEvents)
	}
	mu.Unlock()

	// Verify result events persisted to persona-scoped aggregate.
	aggregateID := "corr-incomplete:persona:task-dispatcher"
	stored, err := store.Load(ctx, aggregateID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(stored) == 0 {
		t.Fatal("expected result events to be persisted")
	}
	// The stored event should be the enrichment, not PersonaCompleted.
	found := false
	for _, e := range stored {
		if e.Type == "context.enrichment" {
			found = true
		}
		if e.Type == event.PersonaCompleted || e.Type == event.PersonaFailed {
			t.Errorf("unexpected lifecycle event persisted: %s", e.Type)
		}
	}
	if !found {
		t.Error("enrichment event not found in persona aggregate")
	}
}

func TestPersonaRunnerErrIncomplete_RetriggerOnSubsequentEvent(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t)

	var callCount int
	var mu sync.Mutex
	completedCh := make(chan struct{}, 1)

	// Handler returns ErrIncomplete on first call, nil on second.
	h := &stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "multi-cycle",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				mu.Lock()
				callCount++
				n := callCount
				mu.Unlock()
				if n == 1 {
					return nil, handler.ErrIncomplete
				}
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events: []event.Type{
				event.WorkflowStartedFor("test-retrigger"),
				event.ChildWorkflowCompleted,
			},
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	bus.Subscribe(event.PersonaCompleted, func(_ context.Context, env event.Envelope) error {
		var pc event.PersonaCompletedPayload
		if err := json.Unmarshal(env.Payload, &pc); err == nil && pc.Persona == "multi-cycle" {
			completedCh <- struct{}{}
		}
		return nil
	}, eventbus.WithName("test:completed"))

	ctx := context.Background()
	runner.Start(ctx, reg)

	// First trigger → ErrIncomplete (no PersonaCompleted).
	triggerEvt := event.New(event.WorkflowStartedFor("test-retrigger"), 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "test-retrigger",
	})).WithCorrelation("corr-retrigger")
	if err := bus.Publish(ctx, triggerEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Brief wait to let incomplete dispatch settle.
	time.Sleep(200 * time.Millisecond)

	// Second trigger → ChildWorkflowCompleted → handler returns nil → PersonaCompleted.
	childEvt := event.New(event.ChildWorkflowCompleted, 1, event.MustMarshal(event.ChildWorkflowCompletedPayload{
		ParentCorrelation: "corr-retrigger",
		ChildCorrelation:  "child-1",
		Status:            "completed",
	})).WithCorrelation("corr-retrigger")
	if err := bus.Publish(ctx, childEvt); err != nil {
		t.Fatalf("publish child: %v", err)
	}

	select {
	case <-completedCh:
		// Expected: PersonaCompleted emitted on second call.
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: expected PersonaCompleted after second dispatch")
	}

	mu.Lock()
	if callCount != 2 {
		t.Errorf("expected handler called 2 times, got %d", callCount)
	}
	mu.Unlock()
}

func TestPersonaRunnerErrIncomplete_MultiJoinRetrigger(t *testing.T) {
	runner, store, bus, reg := newTestPersonaRunner(t)

	var callCount int
	var mu sync.Mutex
	completedCh := make(chan struct{}, 1)

	// Handler with multi-join (AfterPersonas: [alpha, beta]) that returns
	// ErrIncomplete on first call and nil on second. This tests that the
	// join-gate dedup doesn't block the ChildWorkflowCompleted re-trigger.
	h := &stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "multi-join-incomplete",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				mu.Lock()
				callCount++
				n := callCount
				mu.Unlock()
				if n == 1 {
					return nil, handler.ErrIncomplete
				}
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events: []event.Type{
				event.PersonaCompleted,
				event.ChildWorkflowCompleted,
			},
			AfterPersonas: []string{"alpha", "beta"},
		},
	}
	if err := reg.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}

	bus.Subscribe(event.PersonaCompleted, func(_ context.Context, env event.Envelope) error {
		var pc event.PersonaCompletedPayload
		if err := json.Unmarshal(env.Payload, &pc); err == nil && pc.Persona == "multi-join-incomplete" {
			completedCh <- struct{}{}
		}
		return nil
	}, eventbus.WithName("test:completed"))

	// Seed both required PersonaCompleted events.
	seedPersonaCompleted(t, store, "corr-multijoin", "alpha")
	seedPersonaCompleted(t, store, "corr-multijoin", "beta")

	ctx := context.Background()
	runner.Start(ctx, reg)

	// First trigger: PersonaCompleted{beta} satisfies join → ErrIncomplete.
	triggerEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "beta", ChainDepth: 0,
	})).WithCorrelation("corr-multijoin")
	if err := bus.Publish(ctx, triggerEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Second trigger: ChildWorkflowCompleted — should NOT be blocked by join-gate dedup.
	childEvt := event.New(event.ChildWorkflowCompleted, 1, event.MustMarshal(event.ChildWorkflowCompletedPayload{
		ParentCorrelation: "corr-multijoin",
		ChildCorrelation:  "child-1",
		Status:            "completed",
	})).WithCorrelation("corr-multijoin")
	if err := bus.Publish(ctx, childEvt); err != nil {
		t.Fatalf("publish child: %v", err)
	}

	select {
	case <-completedCh:
		// Expected: PersonaCompleted emitted on second call.
	case <-time.After(2 * time.Second):
		mu.Lock()
		t.Fatalf("timeout: expected PersonaCompleted after ChildWorkflowCompleted re-trigger (calls=%d)", callCount)
		mu.Unlock()
	}

	mu.Lock()
	if callCount != 2 {
		t.Errorf("expected handler called 2 times, got %d", callCount)
	}
	mu.Unlock()
}

func TestChildWorkflowCompletedPriority(t *testing.T) {
	p := eventPriority(event.ChildWorkflowCompleted)
	if p != PriorityFeedbackGenerated {
		t.Errorf("expected ChildWorkflowCompleted priority %d, got %d", PriorityFeedbackGenerated, p)
	}
}

// === DAG cross-workflow isolation tests ===

// TestDAGRelevance_CrossWorkflowBlocked verifies that a handler scoped to
// workflow-a is NOT dispatched when workflow-b's persona completes. This
// is the regression test for the bug where isDAGRelevant fell back to
// TestJoinConditionBlockedByFailVerdict verifies that a predecessor whose latest
// VerdictRendered is "fail" does NOT satisfy the downstream join. This prevents
// the committer from dispatching when quality-gate fails and triggers a feedback
// loop back to the developer.
func TestJoinConditionBlockedByFailVerdict(t *testing.T) {
	runner, store, bus, reg := newTestPersonaRunner(t)

	runner.RegisterWorkflow(WorkflowDef{
		ID:       "test-verdict",
		Required: []string{"developer", "quality-gate", "committer"},
		Graph: map[string][]string{
			"developer":    {},
			"quality-gate": {"developer"},
			"committer":    {"quality-gate"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
		PhaseMap: map[string]string{"develop": "developer"},
	})

	committerFired := make(chan struct{}, 1)
	committer := &stubHandler{
		name: "committer",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			committerFired <- struct{}{}
			return nil, nil
		},
	}
	if err := reg.Register(committer); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx := context.Background()
	corrID := "corr-verdict-block"

	// Populate the corrMap by publishing a workflow.started event.
	wsEvt := event.New(event.WorkflowStartedFor("test-verdict"), 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "test-verdict",
	})).WithCorrelation(corrID).WithAggregate(corrID, 1)
	if err := store.Append(ctx, corrID, 0, []event.Envelope{wsEvt}); err != nil {
		t.Fatalf("append workflow started: %v", err)
	}

	runner.Start(ctx, reg)

	// Publish workflow.started to populate corrMap (SubscribeAll).
	if err := bus.Publish(ctx, wsEvt); err != nil {
		t.Fatalf("publish ws: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let corrMap populate

	// Seed quality-gate results: VerdictRendered{fail} + PersonaCompleted.
	// This mimics what executeDispatch does for a handler that returns a fail verdict.
	qgAgg := corrID + ":persona:quality-gate"
	verdictEvt := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase:       "develop",
		SourcePhase: "quality-gate",
		Outcome:     event.VerdictFail,
		Summary:     "lint failed",
	})).WithAggregate(qgAgg, 1).WithCorrelation(corrID)
	pcEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:    "quality-gate",
		ChainDepth: 2,
	})).WithAggregate(qgAgg, 2).WithCorrelation(corrID)
	if err := store.Append(ctx, qgAgg, 0, []event.Envelope{verdictEvt, pcEvt}); err != nil {
		t.Fatalf("append quality-gate events: %v", err)
	}

	// Publish PersonaCompleted{quality-gate} — committer must NOT fire.
	if err := bus.Publish(ctx, pcEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-committerFired:
		t.Error("committer must NOT fire when quality-gate's latest verdict is fail")
	case <-time.After(500 * time.Millisecond):
		// Expected: join blocked by fail verdict.
	}
}

// TestJoinConditionNotBlockedWithoutFeedbackLoop verifies that fail verdicts
// do NOT block joins in workflows without RetriggeredBy (e.g., pr-review).
// Fail verdicts are informational in review-only workflows.
func TestJoinConditionNotBlockedWithoutFeedbackLoop(t *testing.T) {
	runner, store, bus, reg := newTestPersonaRunner(t)

	runner.RegisterWorkflow(WorkflowDef{
		ID:       "test-review-only",
		Required: []string{"reviewer", "qa", "consolidator"},
		Graph: map[string][]string{
			"reviewer":     {},
			"qa":           {},
			"consolidator": {"reviewer", "qa"},
		},
		// No RetriggeredBy — no feedback loops.
	})

	consolidatorFired := make(chan struct{}, 1)
	consolidator := &stubHandler{
		name: "consolidator",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			consolidatorFired <- struct{}{}
			return nil, nil
		},
	}
	if err := reg.Register(consolidator); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx := context.Background()
	corrID := "corr-review-no-feedback"

	wsEvt := event.New(event.WorkflowStartedFor("test-review-only"), 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "test-review-only",
	})).WithCorrelation(corrID).WithAggregate(corrID, 1)
	if err := store.Append(ctx, corrID, 0, []event.Envelope{wsEvt}); err != nil {
		t.Fatalf("append workflow started: %v", err)
	}

	runner.Start(ctx, reg)

	if err := bus.Publish(ctx, wsEvt); err != nil {
		t.Fatalf("publish ws: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Seed reviewer: pass verdict + PersonaCompleted.
	revAgg := corrID + ":persona:reviewer"
	revPC := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:    "reviewer",
		ChainDepth: 0,
	})).WithAggregate(revAgg, 1).WithCorrelation(corrID)
	if err := store.Append(ctx, revAgg, 0, []event.Envelope{revPC}); err != nil {
		t.Fatalf("append reviewer: %v", err)
	}

	// Seed qa: fail verdict + PersonaCompleted.
	qaAgg := corrID + ":persona:qa"
	qaVerdict := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase:       "develop",
		SourcePhase: "qa",
		Outcome:     event.VerdictFail,
		Summary:     "missing tests",
	})).WithAggregate(qaAgg, 1).WithCorrelation(corrID)
	qaPC := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:    "qa",
		ChainDepth: 0,
	})).WithAggregate(qaAgg, 2).WithCorrelation(corrID)
	if err := store.Append(ctx, qaAgg, 0, []event.Envelope{qaVerdict, qaPC}); err != nil {
		t.Fatalf("append qa: %v", err)
	}

	// Publish PersonaCompleted{qa} — consolidator SHOULD fire despite fail verdict.
	if err := bus.Publish(ctx, qaPC); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-consolidatorFired:
		// Expected: fail verdict does not block in review-only workflows.
	case <-time.After(500 * time.Millisecond):
		t.Error("consolidator must fire in review-only workflow despite fail verdict from qa")
	}
}

// TestJoinConditionAllowedAfterPassVerdict verifies that when quality-gate
// re-runs with a passing verdict after a feedback loop, the join IS satisfied.
func TestJoinConditionAllowedAfterPassVerdict(t *testing.T) {
	runner, store, bus, reg := newTestPersonaRunner(t)

	runner.RegisterWorkflow(WorkflowDef{
		ID:       "test-verdict-pass",
		Required: []string{"developer", "quality-gate", "committer"},
		Graph: map[string][]string{
			"developer":    {},
			"quality-gate": {"developer"},
			"committer":    {"quality-gate"},
		},
		PhaseMap: map[string]string{"develop": "developer"},
	})

	committerFired := make(chan struct{}, 1)
	committer := &stubHandler{
		name: "committer",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			committerFired <- struct{}{}
			return nil, nil
		},
	}
	if err := reg.Register(committer); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx := context.Background()
	corrID := "corr-verdict-pass"

	wsEvt := event.New(event.WorkflowStartedFor("test-verdict-pass"), 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "test-verdict-pass",
	})).WithCorrelation(corrID).WithAggregate(corrID, 1)
	if err := store.Append(ctx, corrID, 0, []event.Envelope{wsEvt}); err != nil {
		t.Fatalf("append workflow started: %v", err)
	}

	runner.Start(ctx, reg)

	if err := bus.Publish(ctx, wsEvt); err != nil {
		t.Fatalf("publish ws: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Seed: first run fail, then re-run pass (simulating completed feedback loop).
	qgAgg := corrID + ":persona:quality-gate"
	failVerdict := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase: "develop", SourcePhase: "quality-gate", Outcome: event.VerdictFail,
	})).WithAggregate(qgAgg, 1).WithCorrelation(corrID)
	failPC := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "quality-gate", ChainDepth: 2,
	})).WithAggregate(qgAgg, 2).WithCorrelation(corrID)
	// After feedback loop, quality-gate re-runs and passes.
	passVerdict := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase: "develop", SourcePhase: "quality-gate", Outcome: event.VerdictPass,
	})).WithAggregate(qgAgg, 3).WithCorrelation(corrID)
	passPC := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "quality-gate", ChainDepth: 2,
	})).WithAggregate(qgAgg, 4).WithCorrelation(corrID)

	if err := store.Append(ctx, qgAgg, 0, []event.Envelope{failVerdict, failPC, passVerdict, passPC}); err != nil {
		t.Fatalf("append quality-gate events: %v", err)
	}

	if err := bus.Publish(ctx, passPC); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-committerFired:
		// Expected: latest verdict is pass, join satisfied.
	case <-time.After(2 * time.Second):
		t.Error("committer should fire when quality-gate's latest verdict is pass")
	}
}

// isTriggerRelevant on corrMap miss and fired handlers in the wrong workflow.
func TestDAGRelevance_CrossWorkflowBlocked(t *testing.T) {
	runner, store, bus, reg := newTestPersonaRunner(t)
	_ = store

	// workflow-a: root-a → handler-a
	runner.RegisterWorkflow(WorkflowDef{
		ID:       "workflow-a",
		Required: []string{"root-a", "handler-a"},
		Graph: map[string][]string{
			"root-a":   {},
			"handler-a": {"root-a"},
		},
	})

	// workflow-b: root-b → handler-b
	runner.RegisterWorkflow(WorkflowDef{
		ID:       "workflow-b",
		Required: []string{"root-b", "handler-b"},
		Graph: map[string][]string{
			"root-b":   {},
			"handler-b": {"root-b"},
		},
	})

	// handler-a: must NOT fire for workflow-b events
	handlerAFired := make(chan struct{}, 1)
	hA := &stubHandler{
		name: "handler-a",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			handlerAFired <- struct{}{}
			return nil, nil
		},
	}
	if err := reg.Register(hA); err != nil {
		t.Fatalf("register handler-a: %v", err)
	}

	// handler-b: should fire when root-b completes in workflow-b
	handlerBFired := make(chan struct{}, 1)
	hB := &stubHandler{
		name: "handler-b",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			handlerBFired <- struct{}{}
			return nil, nil
		},
	}
	if err := reg.Register(hB); err != nil {
		t.Fatalf("register handler-b: %v", err)
	}

	// root-a and root-b are no-op handlers (roots fire on WorkflowStarted, not PersonaCompleted)
	hRootA := &stubHandler{
		name: "root-a",
		subs: []event.Type{event.WorkflowStartedFor("workflow-a")},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return nil, nil
		},
	}
	if err := reg.Register(hRootA); err != nil {
		t.Fatalf("register root-a: %v", err)
	}
	hRootB := &stubHandler{
		name: "root-b",
		subs: []event.Type{event.WorkflowStartedFor("workflow-b")},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return nil, nil
		},
	}
	if err := reg.Register(hRootB); err != nil {
		t.Fatalf("register root-b: %v", err)
	}

	ctx := context.Background()
	runner.Start(ctx, reg)

	// Populate corrMap and trigger the dispatch chain: publish
	// workflow.started.workflow-b so the runner caches corr-b → workflow-b
	// and fires root-b (subscribed to this event). root-b completes, the
	// runner emits PersonaCompleted{root-b}, which triggers handler-b.
	startEvt := event.New(event.WorkflowStartedFor("workflow-b"), 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "workflow-b",
	})).WithCorrelation("corr-b")
	if err := bus.Publish(ctx, startEvt); err != nil {
		t.Fatalf("publish workflow-started: %v", err)
	}

	// handler-b must fire — it's in workflow-b and root-b is its predecessor.
	select {
	case <-handlerBFired:
		// Expected: handler-b correctly dispatched within workflow-b.
	case <-time.After(2 * time.Second):
		t.Fatal("handler-b should have fired after root-b completed in workflow-b")
	}

	// handler-a must NOT fire — it belongs to workflow-a, not workflow-b.
	select {
	case <-handlerAFired:
		t.Error("handler-a must NOT fire for workflow-b's persona completion")
	case <-time.After(300 * time.Millisecond):
		// Expected: cross-workflow dispatch was correctly blocked.
	}
}

// TestDAGRelevance_CacheMissBlocksGraphHandler verifies that when the corrMap
// has no entry for a correlation (e.g., workflow.started.* was not yet seen),
// Graph-managed handlers are blocked instead of falling back to the legacy
// trigger path (which would fire them indiscriminately).
func TestDAGRelevance_CacheMissBlocksGraphHandler(t *testing.T) {
	runner, _, bus, reg := newTestPersonaRunner(t)

	// Register workflow-a so handler-a appears in a Graph.
	runner.RegisterWorkflow(WorkflowDef{
		ID:       "workflow-a",
		Required: []string{"root-a", "handler-a"},
		Graph: map[string][]string{
			"root-a":   {},
			"handler-a": {"root-a"},
		},
	})

	handlerAFired := make(chan struct{}, 1)
	hA := &stubHandler{
		name: "handler-a",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			handlerAFired <- struct{}{}
			return nil, nil
		},
	}
	if err := reg.Register(hA); err != nil {
		t.Fatalf("register handler-a: %v", err)
	}
	hRootA := &stubHandler{
		name: "root-a",
		subs: []event.Type{event.WorkflowStartedFor("workflow-a")},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return nil, nil
		},
	}
	if err := reg.Register(hRootA); err != nil {
		t.Fatalf("register root-a: %v", err)
	}

	ctx := context.Background()
	runner.Start(ctx, reg)

	// Intentionally do NOT publish any workflow.started.* event.
	// corrMap has no entry for "corr-unknown", simulating the race where the
	// cache hasn't been populated yet (or the event was missed).

	// Publish PersonaCompleted{root-a} with an unknown correlation.
	pcEvt := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:    "root-a",
		ChainDepth: 0,
	})).WithCorrelation("corr-unknown")
	if err := bus.Publish(ctx, pcEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// handler-a is in a Graph, so it must be blocked on cache miss — it should
	// NOT fall through to the legacy trigger path and fire spuriously.
	select {
	case <-handlerAFired:
		t.Error("handler-a must NOT fire when corrMap has no entry for the correlation")
	case <-time.After(300 * time.Millisecond):
		// Expected: Graph-managed handler correctly suppressed on cache miss.
	}
}

// ---------------------------------------------------------------------------
// Regression tests for production workflow failures (2026-03-24)
// ---------------------------------------------------------------------------

// TestThreeWayJoinWithFailVerdict_PRReview reproduces the pr-review failure
// where pr-consolidator never dispatched because qa's fail verdict blocked
// the 3-way join. In review-only workflows (no RetriggeredBy), fail verdicts
// are informational and must NOT block downstream joins.
func TestThreeWayJoinWithFailVerdict_PRReview(t *testing.T) {
	runner, store, bus, reg := newTestPersonaRunner(t)

	runner.RegisterWorkflow(WorkflowDef{
		ID: "pr-review",
		Required: []string{
			"pr-workspace", "pr-jira-context",
			"architect", "reviewer", "qa",
			"pr-consolidator", "pr-cleanup",
		},
		Graph: map[string][]string{
			"pr-workspace":    {},
			"pr-jira-context": {"pr-workspace"},
			"architect":       {"pr-jira-context"},
			"reviewer":        {"pr-jira-context"},
			"qa":              {"pr-jira-context"},
			"pr-consolidator": {"architect", "reviewer", "qa"},
			"pr-cleanup":      {"pr-consolidator"},
		},
		MaxIterations: 1,
		// No RetriggeredBy — review-only workflow.
	})

	consolidatorFired := make(chan struct{}, 1)
	consolidator := &stubHandler{
		name: "pr-consolidator",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			consolidatorFired <- struct{}{}
			return nil, nil
		},
	}
	if err := reg.Register(consolidator); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx := context.Background()
	corrID := "corr-pr-review-3way"

	// Seed workflow.started
	wsEvt := event.New(event.WorkflowStartedFor("pr-review"), 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "pr-review",
	})).WithCorrelation(corrID).WithAggregate(corrID, 1)
	if err := store.Append(ctx, corrID, 0, []event.Envelope{wsEvt}); err != nil {
		t.Fatalf("append ws: %v", err)
	}

	runner.Start(ctx, reg)

	if err := bus.Publish(ctx, wsEvt); err != nil {
		t.Fatalf("publish ws: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Seed architect: pass (no verdict event).
	archAgg := corrID + ":persona:architect"
	archPC := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "architect", ChainDepth: 2,
	})).WithAggregate(archAgg, 1).WithCorrelation(corrID)
	if err := store.Append(ctx, archAgg, 0, []event.Envelope{archPC}); err != nil {
		t.Fatalf("append architect: %v", err)
	}

	// Seed reviewer: pass verdict + PersonaCompleted.
	revAgg := corrID + ":persona:reviewer"
	revVerdict := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase: "develop", SourcePhase: "review", Outcome: event.VerdictPass,
		Summary: "looks good",
	})).WithAggregate(revAgg, 1).WithCorrelation(corrID)
	revPC := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "reviewer", ChainDepth: 2,
	})).WithAggregate(revAgg, 2).WithCorrelation(corrID)
	if err := store.Append(ctx, revAgg, 0, []event.Envelope{revVerdict, revPC}); err != nil {
		t.Fatalf("append reviewer: %v", err)
	}

	// Seed qa: FAIL verdict + PersonaCompleted — the condition that blocked pr-consolidator.
	qaAgg := corrID + ":persona:qa"
	qaVerdict := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase: "develop", SourcePhase: "qa", Outcome: event.VerdictFail,
		Summary: "missing tests", Issues: []event.Issue{{Severity: "minor", Category: "testing", Description: "no tests"}},
	})).WithAggregate(qaAgg, 1).WithCorrelation(corrID)
	qaPC := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "qa", ChainDepth: 2,
	})).WithAggregate(qaAgg, 2).WithCorrelation(corrID)
	if err := store.Append(ctx, qaAgg, 0, []event.Envelope{qaVerdict, qaPC}); err != nil {
		t.Fatalf("append qa: %v", err)
	}

	// Publish PersonaCompleted{qa} — the last predecessor. pr-consolidator MUST fire.
	if err := bus.Publish(ctx, qaPC); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-consolidatorFired:
		// Expected: 3-way join satisfied despite qa's fail verdict.
	case <-time.After(2 * time.Second):
		t.Error("pr-consolidator must fire in review-only workflow despite qa fail verdict (3-way join)")
	}
}

// TestFeedbackLoopParallelRefire verifies that after a feedback loop, BOTH
// parallel handlers (reviewer AND qa) re-fire when developer re-completes.
// Reproduces the jira-dev bug where qa was silently dropped after iteration 3.
func TestFeedbackLoopParallelRefire(t *testing.T) {
	runner, store, bus, reg := newTestPersonaRunner(t)

	runner.RegisterWorkflow(WorkflowDef{
		ID:       "ci-fix",
		Required: []string{"workspace", "developer", "quality-gate", "reviewer", "qa", "committer"},
		Graph: map[string][]string{
			"workspace":    {},
			"developer":    {"workspace"},
			"reviewer":     {"developer"},
			"qa":           {"developer"},
			"quality-gate": {"reviewer", "qa"},
			"committer":    {"quality-gate"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
		MaxIterations:     2,
		EscalateOnMaxIter: true,
		PhaseMap:          map[string]string{"develop": "developer"},
	})

	var mu sync.Mutex
	firedHandlers := map[string]int{}
	makeFiringHandler := func(name string) handler.Handler {
		return &stubHandler{
			name: name,
			subs: []event.Type{event.PersonaCompleted},
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				mu.Lock()
				firedHandlers[name]++
				mu.Unlock()
				return nil, nil
			},
		}
	}

	for _, name := range []string{"reviewer", "qa", "committer"} {
		if err := reg.Register(makeFiringHandler(name)); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	ctx := context.Background()
	corrID := "corr-parallel-refire"

	// Seed workflow.started
	wsEvt := event.New(event.WorkflowStartedFor("ci-fix"), 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "ci-fix",
	})).WithCorrelation(corrID).WithAggregate(corrID, 1)
	if err := store.Append(ctx, corrID, 0, []event.Envelope{wsEvt}); err != nil {
		t.Fatalf("append ws: %v", err)
	}

	runner.Start(ctx, reg)
	if err := bus.Publish(ctx, wsEvt); err != nil {
		t.Fatalf("publish ws: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// === Iteration 0 ===
	// Seed developer PersonaCompleted. Don't register developer as a handler —
	// we only observe reviewer/qa/committer.
	devAgg := corrID + ":persona:developer"
	devPC1 := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer", ChainDepth: 1,
	})).WithAggregate(devAgg, 1).WithCorrelation(corrID)
	if err := store.Append(ctx, devAgg, 0, []event.Envelope{devPC1}); err != nil {
		t.Fatalf("append dev1: %v", err)
	}
	if err := bus.Publish(ctx, devPC1); err != nil {
		t.Fatalf("publish dev1: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Both reviewer and qa should have fired.
	mu.Lock()
	if firedHandlers["reviewer"] != 1 {
		t.Errorf("iteration 0: reviewer should fire once, fired %d", firedHandlers["reviewer"])
	}
	if firedHandlers["qa"] != 1 {
		t.Errorf("iteration 0: qa should fire once, fired %d", firedHandlers["qa"])
	}
	mu.Unlock()

	// Wait for PersonaRunner to persist PersonaCompleted for reviewer/qa,
	// then seed quality-gate fail + FeedbackGenerated.
	time.Sleep(50 * time.Millisecond)

	qgAgg := corrID + ":persona:quality-gate"
	qgVerdict := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase: "develop", SourcePhase: "quality-gate", Outcome: event.VerdictFail,
		Summary: "lint failed",
	})).WithAggregate(qgAgg, 1).WithCorrelation(corrID)
	qgPC1 := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "quality-gate", ChainDepth: 3,
	})).WithAggregate(qgAgg, 2).WithCorrelation(corrID)
	if err := store.Append(ctx, qgAgg, 0, []event.Envelope{qgVerdict, qgPC1}); err != nil {
		t.Fatalf("append qg1: %v", err)
	}

	fbEvt := event.New(event.FeedbackGenerated, 1, event.MustMarshal(event.FeedbackGeneratedPayload{
		TargetPhase: "developer", SourcePhase: "quality-gate", Iteration: 1,
	})).WithAggregate(corrID, 2).WithCorrelation(corrID)
	if err := store.Append(ctx, corrID, 1, []event.Envelope{fbEvt}); err != nil {
		t.Fatalf("append feedback: %v", err)
	}

	// === Iteration 1 ===
	// Reset counters, developer re-completes after feedback.
	mu.Lock()
	firedHandlers = map[string]int{}
	mu.Unlock()

	devPC2 := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer", ChainDepth: 0,
	})).WithAggregate(devAgg, 2).WithCorrelation(corrID)
	if err := store.Append(ctx, devAgg, 1, []event.Envelope{devPC2}); err != nil {
		t.Fatalf("append dev2: %v", err)
	}
	if err := bus.Publish(ctx, devPC2); err != nil {
		t.Fatalf("publish dev2: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	if firedHandlers["reviewer"] != 1 {
		t.Errorf("iteration 1: reviewer should re-fire once, fired %d", firedHandlers["reviewer"])
	}
	if firedHandlers["qa"] != 1 {
		t.Errorf("iteration 1: qa should re-fire once, fired %d", firedHandlers["qa"])
	}
	if firedHandlers["committer"] != 0 {
		t.Errorf("iteration 1: committer must NOT fire while quality-gate has fail verdict, fired %d", firedHandlers["committer"])
	}
	mu.Unlock()
}

// TestDuplicateQualityGatePrevention verifies that after FeedbackGenerated,
// quality-gate does NOT fire a second time from stale reviewer/qa completions.
// Reproduces the jira-dev/ci-fix bug where duplicate quality-gate executions
// corrupted iteration counts and orphaned downstream handlers.
func TestDuplicateQualityGatePrevention(t *testing.T) {
	runner, store, bus, reg := newTestPersonaRunner(t)

	runner.RegisterWorkflow(WorkflowDef{
		ID:       "test-dup-qg",
		Required: []string{"developer", "reviewer", "qa", "quality-gate", "committer"},
		Graph: map[string][]string{
			"developer":    {},
			"reviewer":     {"developer"},
			"qa":           {"developer"},
			"quality-gate": {"reviewer", "qa"},
			"committer":    {"quality-gate"},
		},
		RetriggeredBy: map[string][]event.Type{
			"developer": {event.FeedbackGenerated},
		},
		PhaseMap: map[string]string{"develop": "developer"},
	})

	var mu sync.Mutex
	qgFires := 0
	committerFires := 0
	qg := &stubHandler{
		name: "quality-gate",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			mu.Lock()
			qgFires++
			mu.Unlock()
			return nil, nil
		},
	}
	committer := &stubHandler{
		name: "committer",
		subs: []event.Type{event.PersonaCompleted},
		handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			mu.Lock()
			committerFires++
			mu.Unlock()
			return nil, nil
		},
	}
	if err := reg.Register(qg); err != nil {
		t.Fatalf("register qg: %v", err)
	}
	if err := reg.Register(committer); err != nil {
		t.Fatalf("register committer: %v", err)
	}

	ctx := context.Background()
	corrID := "corr-dup-qg"

	wsEvt := event.New(event.WorkflowStartedFor("test-dup-qg"), 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "test-dup-qg",
	})).WithCorrelation(corrID).WithAggregate(corrID, 1)
	if err := store.Append(ctx, corrID, 0, []event.Envelope{wsEvt}); err != nil {
		t.Fatalf("append ws: %v", err)
	}

	runner.Start(ctx, reg)
	if err := bus.Publish(ctx, wsEvt); err != nil {
		t.Fatalf("publish ws: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// === Iteration 0: seed reviewer + qa completions → quality-gate fires ===
	// quality-gate and committer are registered; reviewer/qa are NOT — we
	// manually seed their PersonaCompleted events to avoid aggregate conflicts.
	revAgg := corrID + ":persona:reviewer"
	qaAgg := corrID + ":persona:qa"

	revPC := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "reviewer", ChainDepth: 1,
	})).WithAggregate(revAgg, 1).WithCorrelation(corrID)
	if err := store.Append(ctx, revAgg, 0, []event.Envelope{revPC}); err != nil {
		t.Fatalf("append rev: %v", err)
	}

	qaPC := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "qa", ChainDepth: 1,
	})).WithAggregate(qaAgg, 1).WithCorrelation(corrID)
	if err := store.Append(ctx, qaAgg, 0, []event.Envelope{qaPC}); err != nil {
		t.Fatalf("append qa: %v", err)
	}

	// Publish qa's PersonaCompleted — this triggers quality-gate's join check.
	if err := bus.Publish(ctx, qaPC); err != nil {
		t.Fatalf("publish qa: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	if qgFires != 1 {
		t.Fatalf("iteration 0: quality-gate should fire once, fired %d", qgFires)
	}
	mu.Unlock()

	// Wait for PersonaRunner to persist quality-gate's PersonaCompleted, then
	// read the aggregate version so we can append verdict + feedback on top.
	time.Sleep(50 * time.Millisecond)
	qgAgg := corrID + ":persona:quality-gate"
	qgEvents, loadErr := store.Load(ctx, qgAgg)
	if loadErr != nil {
		t.Fatalf("load qg agg: %v", loadErr)
	}
	qgVersion := 0
	if len(qgEvents) > 0 {
		qgVersion = qgEvents[len(qgEvents)-1].Version
	}

	// Seed quality-gate's fail verdict on its existing aggregate.
	qgVerdict := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase: "develop", SourcePhase: "quality-gate", Outcome: event.VerdictFail,
		Summary: "lint failed",
	})).WithAggregate(qgAgg, qgVersion+1).WithCorrelation(corrID)
	if err := store.Append(ctx, qgAgg, qgVersion, []event.Envelope{qgVerdict}); err != nil {
		t.Fatalf("append qg verdict: %v", err)
	}

	fbEvt := event.New(event.FeedbackGenerated, 1, event.MustMarshal(event.FeedbackGeneratedPayload{
		TargetPhase: "developer", SourcePhase: "quality-gate", Iteration: 1,
	})).WithAggregate(corrID, 2).WithCorrelation(corrID)
	if err := store.Append(ctx, corrID, 1, []event.Envelope{fbEvt}); err != nil {
		t.Fatalf("append fb: %v", err)
	}

	// Reset fire counts
	mu.Lock()
	qgFires = 0
	committerFires = 0
	mu.Unlock()

	// === Publish reviewer's PersonaCompleted again (stale) ===
	// This simulates a stale event reaching quality-gate's wrap() after
	// feedback. DownstreamOf invalidation must prevent quality-gate from
	// re-firing with old reviewer + old qa.
	if err := bus.Publish(ctx, revPC); err != nil {
		t.Fatalf("publish stale revPC: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	if qgFires != 0 {
		t.Errorf("quality-gate must NOT re-fire from stale completions after feedback, fired %d", qgFires)
	}
	if committerFires != 0 {
		t.Errorf("committer must NOT fire when quality-gate has fail verdict, fired %d", committerFires)
	}
	mu.Unlock()

	// === Verify: fresh reviewer/qa completions DO trigger quality-gate ===
	mu.Lock()
	qgFires = 0
	mu.Unlock()

	revPC2 := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "reviewer", ChainDepth: 1,
	})).WithAggregate(revAgg, 2).WithCorrelation(corrID)
	if err := store.Append(ctx, revAgg, 1, []event.Envelope{revPC2}); err != nil {
		t.Fatalf("append rev2: %v", err)
	}

	qaPC2 := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "qa", ChainDepth: 1,
	})).WithAggregate(qaAgg, 2).WithCorrelation(corrID)
	if err := store.Append(ctx, qaAgg, 1, []event.Envelope{qaPC2}); err != nil {
		t.Fatalf("append qa2: %v", err)
	}

	if err := bus.Publish(ctx, qaPC2); err != nil {
		t.Fatalf("publish qa2: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	if qgFires != 1 {
		t.Errorf("quality-gate should fire exactly once from fresh completions, fired %d", qgFires)
	}
	mu.Unlock()
}
