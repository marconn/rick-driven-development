package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/handler"
	"github.com/marconn/rick-event-driven-development/internal/projection"
)

// e2eEnv bundles all components needed for an end-to-end choreography test.
type e2eEnv struct {
	store  eventstore.Store
	bus    *eventbus.ChannelBus
	engine *Engine
	runner *PersonaRunner
	reg    *handler.Registry
}

func newE2EEnv(t *testing.T, def WorkflowDef) *e2eEnv {
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

	t.Cleanup(func() {
		_ = runner.Close()
		eng.Stop()
		_ = bus.Close()
		_ = store.Close()
	})

	return &e2eEnv{store: store, bus: bus, engine: eng, runner: runner, reg: reg}
}

// start wires engine + persona runner and begins event processing.
func (e *e2eEnv) start(ctx context.Context) {
	e.engine.Start()
	e.runner.Start(ctx, e.reg)
}

// fireWorkflow publishes a WorkflowRequested event with correlationID == aggregateID.
func (e *e2eEnv) fireWorkflow(ctx context.Context, t *testing.T, wfID, defID string) {
	t.Helper()
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "e2e test",
		WorkflowID: defID,
	})).
		WithAggregate(wfID, 1).
		WithCorrelation(wfID). // convention: correlationID == aggregateID
		WithSource("e2e-test")

	if err := e.store.Append(ctx, wfID, 0, []event.Envelope{reqEvt}); err != nil {
		t.Fatalf("store append: %v", err)
	}
	if err := e.bus.Publish(ctx, reqEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// awaitWorkflowResult subscribes for WorkflowCompleted or WorkflowFailed on the
// given workflow ID. Must be called BEFORE publishing the trigger event.
func awaitWorkflowResult(t *testing.T, bus eventbus.Bus, wfID string) <-chan event.Envelope {
	t.Helper()
	ch := make(chan event.Envelope, 2)
	unsub1 := bus.Subscribe(event.WorkflowCompleted, func(_ context.Context, env event.Envelope) error {
		if env.AggregateID == wfID {
			ch <- env
		}
		return nil
	}, eventbus.WithName("e2e:completed"))
	unsub2 := bus.Subscribe(event.WorkflowFailed, func(_ context.Context, env event.Envelope) error {
		if env.AggregateID == wfID {
			ch <- env
		}
		return nil
	}, eventbus.WithName("e2e:failed"))
	t.Cleanup(func() { unsub1(); unsub2() })
	return ch
}

// === Happy Path: WorkflowRequested → alpha → beta → WorkflowCompleted ===

func TestE2EHappyPath(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-happy", Required: []string{"alpha", "beta"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}, "beta": {"alpha"}},
	}
	env := newE2EEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-happy")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "beta",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"alpha"}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus,"wf-happy")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-happy", "e2e-happy")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Errorf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(5 * time.Second):
		// Dump workflow events for debugging
		events, _ := env.store.Load(ctx, "wf-happy")
		for _, e := range events {
			t.Logf("  event: %s (v%d, agg=%s)", e.Type, e.Version, e.AggregateID)
		}
		t.Fatal("timeout: WorkflowCompleted never arrived")
	}
}

// === Parallel Dispatch: alpha → beta + gamma simultaneously → WorkflowCompleted ===

func TestE2EParallelDispatch(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-parallel", Required: []string{"alpha", "beta", "gamma"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}, "beta": {"alpha"}, "gamma": {"alpha"}},
	}
	env := newE2EEnv(t, def)

	var betaFired, gammaFired atomic.Bool

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-parallel")}},
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
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"alpha"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "gamma",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				gammaFired.Store(true)
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"alpha"}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus,"wf-parallel")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-parallel", "e2e-parallel")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Errorf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout: beta=%v, gamma=%v", betaFired.Load(), gammaFired.Load())
	}

	if !betaFired.Load() {
		t.Error("beta should have fired")
	}
	if !gammaFired.Load() {
		t.Error("gamma should have fired")
	}
}

// === Handler Failure: alpha fails → WorkflowFailed ===

func TestE2EHandlerFailure(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-fail", Required: []string{"alpha"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}},
	}
	env := newE2EEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				return nil, fmt.Errorf("compilation error")
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-fail")}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus,"wf-fail")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-fail", "e2e-fail")

	select {
	case got := <-result:
		if got.Type != event.WorkflowFailed {
			t.Errorf("expected WorkflowFailed, got %s", got.Type)
		}
		var p event.WorkflowFailedPayload
		_ = json.Unmarshal(got.Payload, &p)
		if p.Phase != "alpha" {
			t.Errorf("expected phase=alpha, got %s", p.Phase)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: WorkflowFailed never arrived")
	}
}

// === Deep Chain: alpha → beta → gamma → delta → WorkflowCompleted ===

func TestE2EDeepChain(t *testing.T) {
	def := WorkflowDef{
		ID:            "e2e-chain",
		Required:      []string{"alpha", "beta", "gamma", "delta"},
		MaxIterations: 3,
		Graph: map[string][]string{
			"alpha": {},
			"beta":  {"alpha"},
			"gamma": {"beta"},
			"delta": {"gamma"},
		},
	}
	env := newE2EEnv(t, def)

	handlers := []struct {
		name  string
		event event.Type
		after []string
	}{
		{"alpha", event.WorkflowStartedFor("e2e-chain"), nil},
		{"beta", event.PersonaCompleted, []string{"alpha"}},
		{"gamma", event.PersonaCompleted, []string{"beta"}},
		{"delta", event.PersonaCompleted, []string{"gamma"}},
	}

	for _, h := range handlers {
		if err := env.reg.Register(&stubTriggeredHandler{
			stubHandler: stubHandler{
				name:   h.name,
				handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
			},
			trigger: handler.Trigger{Events: []event.Type{h.event}, AfterPersonas: h.after},
		}); err != nil {
			t.Fatal(err)
		}
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus,"wf-chain")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-chain", "e2e-chain")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Errorf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(5 * time.Second):
		events, _ := env.store.Load(ctx, "wf-chain")
		for _, e := range events {
			t.Logf("  event: %s (v%d)", e.Type, e.Version)
		}
		t.Fatal("timeout: deep chain never completed")
	}
}

// === Join Gate: delta waits for both beta AND gamma ===

func TestE2EJoinGate(t *testing.T) {
	def := WorkflowDef{
		ID:            "e2e-join",
		Required:      []string{"alpha", "beta", "gamma", "delta"},
		MaxIterations: 3,
		Graph: map[string][]string{
			"alpha": {},
			"beta":  {"alpha"},
			"gamma": {"alpha"},
			"delta": {"beta", "gamma"},
		},
	}
	env := newE2EEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-join")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "beta",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"alpha"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "gamma",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"alpha"}},
	}); err != nil {
		t.Fatal(err)
	}
	// delta fires only after BOTH beta and gamma have completed
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "delta",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"beta", "gamma"}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus,"wf-join")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-join", "e2e-join")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Errorf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(5 * time.Second):
		events, _ := env.store.Load(ctx, "wf-join")
		for _, e := range events {
			t.Logf("  event: %s (v%d)", e.Type, e.Version)
		}
		corr, _ := env.store.LoadByCorrelation(ctx, "wf-join")
		t.Logf("  --- correlation events ---")
		for _, e := range corr {
			t.Logf("  event: %s (agg=%s)", e.Type, e.AggregateID)
		}
		t.Fatal("timeout: join gate never satisfied")
	}
}

// === Feedback Loop: developer → reviewer(fail) → FeedbackGenerated → developer → reviewer(pass) → WorkflowCompleted ===

func TestE2EFeedbackLoop(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-feedback", Required: []string{"developer", "reviewer"}, MaxIterations: 3, PhaseMap: corePhaseMap,
		Graph: map[string][]string{"developer": {}, "reviewer": {"developer"}},
		RetriggeredBy: map[string][]event.Type{"developer": {event.FeedbackGenerated}},
	}
	env := newE2EEnv(t, def)

	var reviewCount atomic.Int32

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "developer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{
			Events: []event.Type{event.WorkflowStartedFor("e2e-feedback"), event.FeedbackGenerated},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "reviewer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				n := reviewCount.Add(1)
				if n == 1 {
					// First review: fail verdict.
					// Uses phase verbs ("develop", not "developer") to match real handler behavior.
					return []event.Envelope{
						event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
							Phase:       "develop",
							SourcePhase: "review",
							Outcome:     event.VerdictFail,
							Summary:     "needs work",
						})),
					}, nil
				}
				// Subsequent reviews: pass (no verdict events)
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
	result := awaitWorkflowResult(t, env.bus,"wf-feedback")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-feedback", "e2e-feedback")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Errorf("expected WorkflowCompleted, got %s", got.Type)
		}
		if reviewCount.Load() < 2 {
			t.Errorf("expected at least 2 review rounds, got %d", reviewCount.Load())
		}
	case <-time.After(10 * time.Second):
		events, _ := env.store.Load(ctx, "wf-feedback")
		for _, e := range events {
			t.Logf("  event: %s (v%d)", e.Type, e.Version)
		}
		t.Fatalf("timeout: feedback loop never completed (reviews=%d)", reviewCount.Load())
	}
}

// =============================================================================
// Scenario 2: Projections — read models subscribed via SubscribeAll
// =============================================================================

// TestE2EProjectionSeesAllEvents verifies that a projection runner wired to the
// same bus receives every event emitted during a full workflow lifecycle.
func TestE2EProjectionSeesAllEvents(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-proj", Required: []string{"alpha", "beta"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}, "beta": {"alpha"}},
	}
	env := newE2EEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-proj")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "beta",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"alpha"}},
	}); err != nil {
		t.Fatal(err)
	}

	// Wire WorkflowStatusProjection to the same bus — it should see lifecycle events.
	statusProj := projection.NewWorkflowStatusProjection()
	projRunner := projection.NewRunner(env.store, env.bus, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	projRunner.Register(statusProj)

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-proj")
	if err := projRunner.Start(ctx); err != nil {
		t.Fatalf("start projection: %v", err)
	}
	t.Cleanup(projRunner.Stop)

	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-proj", "e2e-proj")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: workflow never completed")
	}

	// Give async projection a moment to process the final event.
	time.Sleep(50 * time.Millisecond)

	ws, ok := statusProj.Get("wf-proj")
	if !ok {
		t.Fatal("projection has no record for wf-proj")
	}
	if ws.Status != "completed" {
		t.Errorf("projection status: want completed, got %s", ws.Status)
	}
	if ws.WorkflowID != "e2e-proj" {
		t.Errorf("projection workflowID: want e2e-proj, got %s", ws.WorkflowID)
	}
}

// TestE2EProjectionTracksFailure verifies that the projection correctly records
// a failed workflow status when a persona fails.
func TestE2EProjectionTracksFailure(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-proj-fail", Required: []string{"alpha"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}},
	}
	env := newE2EEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				return nil, fmt.Errorf("boom")
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-proj-fail")}},
	}); err != nil {
		t.Fatal(err)
	}

	statusProj := projection.NewWorkflowStatusProjection()
	projRunner := projection.NewRunner(env.store, env.bus, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	projRunner.Register(statusProj)

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-proj-fail")
	if err := projRunner.Start(ctx); err != nil {
		t.Fatalf("start projection: %v", err)
	}
	t.Cleanup(projRunner.Stop)

	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-proj-fail", "e2e-proj-fail")

	select {
	case got := <-result:
		if got.Type != event.WorkflowFailed {
			t.Fatalf("expected WorkflowFailed, got %s", got.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: WorkflowFailed never arrived")
	}

	time.Sleep(50 * time.Millisecond)

	ws, ok := statusProj.Get("wf-proj-fail")
	if !ok {
		t.Fatal("projection has no record for wf-proj-fail")
	}
	if ws.Status != "failed" {
		t.Errorf("projection status: want failed, got %s", ws.Status)
	}
	if ws.FailReason == "" {
		t.Error("projection should have a fail reason")
	}
}

// =============================================================================
// Scenario 3: Side systems — direct bus.Subscribe for external integrations
// =============================================================================

// TestE2ESideSystemNotifier verifies that a plain bus subscriber (e.g., Slack
// notifier, metrics collector) receives workflow events without affecting the
// choreography. The side system is NOT a persona — it doesn't emit
// PersonaCompleted and is not in the Required list.
func TestE2ESideSystemNotifier(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-side", Required: []string{"alpha"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}},
	}
	env := newE2EEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-side")}},
	}); err != nil {
		t.Fatal(err)
	}

	// Side system: simple subscriber that records notifications.
	var notifications []string
	var notifMu sync.Mutex

	unsub := env.bus.Subscribe(event.WorkflowCompleted, func(_ context.Context, e event.Envelope) error {
		if e.AggregateID != "wf-side" {
			return nil
		}
		notifMu.Lock()
		notifications = append(notifications, fmt.Sprintf("completed:%s", e.AggregateID))
		notifMu.Unlock()
		return nil
	}, eventbus.WithName("side:notifier"))
	t.Cleanup(unsub)

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-side")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-side", "e2e-side")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: workflow never completed")
	}

	time.Sleep(50 * time.Millisecond)

	notifMu.Lock()
	defer notifMu.Unlock()
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d: %v", len(notifications), notifications)
	}
	if notifications[0] != "completed:wf-side" {
		t.Errorf("unexpected notification: %s", notifications[0])
	}
}

// TestE2ESideSystemAuditLog verifies that SubscribeAll receives every event type
// emitted during a workflow — enabling audit logging, metrics, or debugging.
func TestE2ESideSystemAuditLog(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-audit", Required: []string{"alpha", "beta"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}, "beta": {"alpha"}},
	}
	env := newE2EEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-audit")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "beta",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"alpha"}},
	}); err != nil {
		t.Fatal(err)
	}

	// Audit log: SubscribeAll captures every event type flowing through the bus.
	var auditLog []event.Type
	var auditMu sync.Mutex

	unsub := env.bus.SubscribeAll(func(_ context.Context, e event.Envelope) error {
		if e.CorrelationID != "wf-audit" && e.AggregateID != "wf-audit" {
			return nil
		}
		auditMu.Lock()
		auditLog = append(auditLog, e.Type)
		auditMu.Unlock()
		return nil
	}, eventbus.WithName("side:audit"))
	t.Cleanup(unsub)

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-audit")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-audit", "e2e-audit")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: workflow never completed")
	}

	time.Sleep(50 * time.Millisecond)

	auditMu.Lock()
	defer auditMu.Unlock()

	// Verify we captured the essential lifecycle events.
	typeSet := make(map[event.Type]bool)
	for _, et := range auditLog {
		typeSet[et] = true
	}

	// Check non-WorkflowStarted required types via exact match.
	for _, et := range []event.Type{event.WorkflowRequested, event.PersonaCompleted, event.WorkflowCompleted} {
		if !typeSet[et] {
			t.Errorf("audit log missing %s (captured: %v)", et, auditLog)
		}
	}
	// WorkflowStarted is now emitted as a scoped type (e.g. workflow.started.e2e-audit).
	var sawWorkflowStarted bool
	for _, et := range auditLog {
		if event.IsWorkflowStarted(et) {
			sawWorkflowStarted = true
			break
		}
	}
	if !sawWorkflowStarted {
		t.Errorf("audit log missing WorkflowStarted variant (captured: %v)", auditLog)
	}
}

// TestE2ESideSystemDoesNotBlockWorkflow verifies that a slow side-system
// subscriber does not block the workflow from completing. Side systems run
// async (default bus dispatch), so a slow handler can't stall the choreography.
func TestE2ESideSystemDoesNotBlockWorkflow(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-slow-side", Required: []string{"alpha"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}},
	}
	env := newE2EEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-slow-side")}},
	}); err != nil {
		t.Fatal(err)
	}

	// Slow side system: sleeps 2s on PersonaCompleted. Should NOT block workflow.
	var slowFired atomic.Bool
	unsub := env.bus.Subscribe(event.PersonaCompleted, func(_ context.Context, e event.Envelope) error {
		if e.CorrelationID != "wf-slow-side" {
			return nil
		}
		time.Sleep(2 * time.Second)
		slowFired.Store(true)
		return nil
	}, eventbus.WithName("side:slow"))
	t.Cleanup(unsub)

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-slow-side")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-slow-side", "e2e-slow-side")

	// Workflow should complete well before the slow subscriber finishes.
	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: workflow blocked by slow side system")
	}
}

// TestE2ESideSystemErrorDoesNotFailWorkflow verifies that a side-system
// subscriber returning an error does not cause the workflow to fail. Errors
// in side systems are logged (dead letter) but isolated from the choreography.
func TestE2ESideSystemErrorDoesNotFailWorkflow(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-err-side", Required: []string{"alpha"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}},
	}
	env := newE2EEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-err-side")}},
	}); err != nil {
		t.Fatal(err)
	}

	// Failing side system: returns error on every event. Must not crash the workflow.
	unsub := env.bus.Subscribe(event.WorkflowStartedFor("e2e-err-side"), func(_ context.Context, e event.Envelope) error {
		if e.AggregateID == "wf-err-side" {
			return fmt.Errorf("side system exploded")
		}
		return nil
	}, eventbus.WithName("side:broken"))
	t.Cleanup(unsub)

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-err-side")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-err-side", "e2e-err-side")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted despite broken side system, got %s", got.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: workflow affected by broken side system")
	}
}

// TestE2ESideSystemMultipleSubscribers verifies that multiple independent side
// systems can subscribe to the same event type and all receive it.
func TestE2ESideSystemMultipleSubscribers(t *testing.T) {
	def := WorkflowDef{
		ID: "e2e-multi-side", Required: []string{"alpha"}, MaxIterations: 3,
		Graph: map[string][]string{"alpha": {}},
	}
	env := newE2EEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "alpha",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-multi-side")}},
	}); err != nil {
		t.Fatal(err)
	}

	// Three independent side systems all subscribing to WorkflowCompleted.
	var slackFired, metricsFired, webhookFired atomic.Bool

	unsub1 := env.bus.Subscribe(event.WorkflowCompleted, func(_ context.Context, e event.Envelope) error {
		if e.AggregateID == "wf-multi-side" {
			slackFired.Store(true)
		}
		return nil
	}, eventbus.WithName("side:slack"))

	unsub2 := env.bus.Subscribe(event.WorkflowCompleted, func(_ context.Context, e event.Envelope) error {
		if e.AggregateID == "wf-multi-side" {
			metricsFired.Store(true)
		}
		return nil
	}, eventbus.WithName("side:metrics"))

	unsub3 := env.bus.Subscribe(event.WorkflowCompleted, func(_ context.Context, e event.Envelope) error {
		if e.AggregateID == "wf-multi-side" {
			webhookFired.Store(true)
		}
		return nil
	}, eventbus.WithName("side:webhook"))

	t.Cleanup(func() { unsub1(); unsub2(); unsub3() })

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-multi-side")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-multi-side", "e2e-multi-side")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: workflow never completed")
	}

	time.Sleep(50 * time.Millisecond)

	if !slackFired.Load() {
		t.Error("slack side system never fired")
	}
	if !metricsFired.Load() {
		t.Error("metrics side system never fired")
	}
	if !webhookFired.Load() {
		t.Error("webhook side system never fired")
	}
}

// =============================================================================
// Scenario 4: Full Rick Workflow — 6 personas with fan-out + join
// =============================================================================

// TestE2EFullRickWorkflow mirrors the actual Rick topology:
//
//	researcher → architect → developer → reviewer (parallel) → committer → WorkflowCompleted
//	                                   → qa       (parallel) ↗
//	                                   → documenter (parallel, not required)
//
// This verifies the complete graph: sequential chain, parallel fan-out after
// developer, join gate on committer (requires reviewer+qa), and a non-required
// persona (documenter) that fires but doesn't gate completion.
func TestE2EFullRickWorkflow(t *testing.T) {
	def := WorkflowDef{
		ID:            "e2e-full-rick",
		Required:      []string{"researcher", "architect", "developer", "reviewer", "qa", "committer"},
		MaxIterations: 3,
		Graph: map[string][]string{
			"researcher": {},
			"architect":  {"researcher"},
			"developer":  {"architect"},
			"reviewer":   {"developer"},
			"qa":         {"developer"},
			"documenter": {"developer"},
			"committer":  {"reviewer", "qa"},
		},
		RetriggeredBy: map[string][]event.Type{"developer": {event.FeedbackGenerated}},
	}
	env := newE2EEnv(t, def)

	var fired sync.Map // persona name → call count

	incFired := func(name string) {
		v, _ := fired.LoadOrStore(name, new(atomic.Int32))
		v.(*atomic.Int32).Add(1)
	}
	getFired := func(name string) int32 {
		v, ok := fired.Load(name)
		if !ok {
			return 0
		}
		return v.(*atomic.Int32).Load()
	}

	// researcher: triggered by WorkflowStarted, no join
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "researcher",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				incFired("researcher")
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-full-rick")}},
	}); err != nil {
		t.Fatal(err)
	}

	// architect: after researcher
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "architect",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				incFired("architect")
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"researcher"}},
	}); err != nil {
		t.Fatal(err)
	}

	// developer: after architect
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "developer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				incFired("developer")
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted, event.FeedbackGenerated}, AfterPersonas: []string{"architect"}},
	}); err != nil {
		t.Fatal(err)
	}

	// reviewer: after developer (parallel with qa + documenter)
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "reviewer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				incFired("reviewer")
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"developer"}},
	}); err != nil {
		t.Fatal(err)
	}

	// qa: after developer (parallel with reviewer + documenter)
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "qa",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				incFired("qa")
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"developer"}},
	}); err != nil {
		t.Fatal(err)
	}

	// documenter: after developer (parallel, NOT in Required — fire-and-forget)
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "documenter",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				incFired("documenter")
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"developer"}},
	}); err != nil {
		t.Fatal(err)
	}

	// committer: join gate on reviewer + qa
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "committer",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				incFired("committer")
				return nil, nil
			},
		},
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"reviewer", "qa"}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-full-rick")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-full-rick", "e2e-full-rick")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		events, _ := env.store.Load(ctx, "wf-full-rick")
		for _, e := range events {
			t.Logf("  event: %s (v%d, agg=%s)", e.Type, e.Version, e.AggregateID)
		}
		corr, _ := env.store.LoadByCorrelation(ctx, "wf-full-rick")
		t.Logf("  --- correlation events ---")
		for _, e := range corr {
			t.Logf("  event: %s (agg=%s)", e.Type, e.AggregateID)
		}
		t.Fatal("timeout: full Rick workflow never completed")
	}

	// Verify every persona fired exactly once
	for _, name := range []string{"researcher", "architect", "developer", "reviewer", "qa", "committer"} {
		if c := getFired(name); c != 1 {
			t.Errorf("%s: expected 1 fire, got %d", name, c)
		}
	}
	// documenter is not required but should have fired
	if c := getFired("documenter"); c != 1 {
		t.Errorf("documenter: expected 1 fire, got %d", c)
	}
}

// =============================================================================
// Scenario 5: ErrIncomplete — multi-cycle handler spanning event cycles
// =============================================================================

// TestErrIncomplete_MultiCycleHandler verifies that a handler returning
// ErrIncomplete on its first dispatch:
//   - persists result events but does NOT emit PersonaCompleted
//   - re-triggers when ChildWorkflowCompleted arrives
//   - completes normally on the second dispatch → PersonaCompleted
//   - WorkflowCompleted follows once all required handlers are done
func TestErrIncomplete_MultiCycleHandler(t *testing.T) {
	def := WorkflowDef{
		ID:            "e2e-incomplete",
		Required:      []string{"reader", "dispatcher"},
		MaxIterations: 3,
		Graph: map[string][]string{
			"reader":     {},
			"dispatcher": {"reader"},
		},
		// dispatcher also re-triggers on ChildWorkflowCompleted (second phase of multi-cycle handling).
		RetriggeredBy: map[string][]event.Type{"dispatcher": {event.ChildWorkflowCompleted}},
	}
	env := newE2EEnv(t, def)

	var dispatchCount atomic.Int32

	// reader: fires on start, completes normally.
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name:   "reader",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		},
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("e2e-incomplete")}},
	}); err != nil {
		t.Fatal(err)
	}

	// dispatcher: subscribes to PersonaCompleted (after reader) AND ChildWorkflowCompleted.
	// First dispatch: returns enrichment + ErrIncomplete.
	// Second dispatch (on ChildWorkflowCompleted): returns nil → PersonaCompleted.
	if err := env.reg.Register(&stubTriggeredHandler{
		stubHandler: stubHandler{
			name: "dispatcher",
			handle: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
				n := dispatchCount.Add(1)
				if n == 1 {
					// First call: dispatch children, stay incomplete.
					enrichment := event.New("context.enrichment", 1,
						event.MustMarshal(event.ContextEnrichmentPayload{
							Source:  "dispatcher",
							Kind:    "batch-status",
							Summary: "dispatched 3 child workflows",
						}))
					return []event.Envelope{enrichment}, handler.ErrIncomplete
				}
				// Second call: all children done, complete.
				return nil, nil
			},
		},
		trigger: handler.Trigger{
			Events:        []event.Type{event.PersonaCompleted, event.ChildWorkflowCompleted},
			AfterPersonas: []string{"reader"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	result := awaitWorkflowResult(t, env.bus, "wf-incomplete")
	env.start(ctx)
	env.fireWorkflow(ctx, t, "wf-incomplete", "e2e-incomplete")

	// After reader completes, dispatcher fires but returns ErrIncomplete.
	// Wait a bit to confirm no premature WorkflowCompleted.
	time.Sleep(500 * time.Millisecond)

	// Now inject ChildWorkflowCompleted — this should wake the dispatcher again.
	childEvt := event.New(event.ChildWorkflowCompleted, 1, event.MustMarshal(event.ChildWorkflowCompletedPayload{
		ParentCorrelation: "wf-incomplete",
		ChildCorrelation:  "child-task-1",
		ChildTicket:       "TASK-1",
		Status:            "completed",
	})).
		WithCorrelation("wf-incomplete").
		WithSource("test")

	// Publish the child event — no need to store, bus dispatch is sufficient.
	if err := env.bus.Publish(ctx, childEvt); err != nil {
		t.Fatalf("publish child event: %v", err)
	}

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Errorf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		events, _ := env.store.Load(ctx, "wf-incomplete")
		for _, e := range events {
			t.Logf("  event: %s (v%d, agg=%s)", e.Type, e.Version, e.AggregateID)
		}
		corr, _ := env.store.LoadByCorrelation(ctx, "wf-incomplete")
		t.Logf("  --- correlation events ---")
		for _, e := range corr {
			t.Logf("  event: %s (agg=%s, type=%s)", e.Type, e.AggregateID, e.Type)
		}
		t.Fatalf("timeout: multi-cycle handler never completed (dispatches=%d)", dispatchCount.Load())
	}

	if n := dispatchCount.Load(); n != 2 {
		t.Errorf("expected dispatcher called 2 times, got %d", n)
	}
}
