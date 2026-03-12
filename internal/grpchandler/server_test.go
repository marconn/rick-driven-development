package grpchandler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
	"github.com/marconn/rick-event-driven-development/internal/handler"
	"github.com/marconn/rick-event-driven-development/internal/projection"
)

// testEnv bundles all components for a gRPC integration test.
type testEnv struct {
	store      eventstore.Store
	bus        *eventbus.ChannelBus
	eng        *engine.Engine
	runner     *engine.PersonaRunner
	reg        *handler.Registry
	streamD    *StreamDispatcher
	injector   *EventInjector
	broker     *NotificationBroker
	projRunner *projection.Runner
	workflows  *projection.WorkflowStatusProjection
	tokens     *projection.TokenUsageProjection
	timelines  *projection.PhaseTimelineProjection
	srv        *Server
	grpcSrv    *grpc.Server
	addr       string
}

func newTestEnv(t *testing.T, def engine.WorkflowDef) *testEnv {
	t.Helper()

	store, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	bus := eventbus.NewChannelBus()
	reg := handler.NewRegistry()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	streamD := NewStreamDispatcher(logger)
	localD := engine.NewLocalDispatcher(reg)
	compositeD := NewCompositeDispatcher(localD, streamD)

	eng := engine.NewEngine(store, bus, logger)
	eng.RegisterWorkflow(def)

	runner := engine.NewPersonaRunner(store, bus, compositeD, logger,
		engine.WithMaxChainDepth(7),
	)
	// Mirror serve.go: register def with both Engine (lifecycle) and
	// PersonaRunner (DAG dispatch).
	runner.RegisterWorkflow(def)

	// Projections + notification broker.
	workflows := projection.NewWorkflowStatusProjection()
	tokens := projection.NewTokenUsageProjection()
	timelines := projection.NewPhaseTimelineProjection()
	verdicts := projection.NewVerdictProjection()

	projRunner := projection.NewRunner(store, bus, logger)
	projRunner.Register(workflows)
	projRunner.Register(tokens)
	projRunner.Register(timelines)
	projRunner.Register(verdicts)

	broker := NewNotificationBroker(bus, workflows, tokens, timelines, verdicts, logger)

	// Start gRPC server on random port.
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	injector := NewEventInjector(store, bus, logger)
	srv := NewServer(streamD, runner, injector, broker, eng, reg, logger)
	grpcSrv := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             15 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	pb.RegisterPersonaServiceServer(grpcSrv, srv)

	go func() { _ = grpcSrv.Serve(lis) }()

	t.Cleanup(func() {
		grpcSrv.GracefulStop()
		broker.Stop()
		projRunner.Stop()
		_ = runner.Close()
		eng.Stop()
		_ = bus.Close()
		_ = store.Close()
	})

	return &testEnv{
		store:     store,
		bus:       bus,
		eng:       eng,
		runner:    runner,
		reg:       reg,
		streamD:   streamD,
		injector:  injector,
		broker:     broker,
		projRunner: projRunner,
		workflows:  workflows,
		tokens:     tokens,
		timelines:  timelines,
		srv:       srv,
		grpcSrv:   grpcSrv,
		addr:      lis.Addr().String(),
	}
}

func (e *testEnv) start(ctx context.Context) {
	e.eng.Start()
	_ = e.projRunner.Start(ctx)
	e.broker.Start()
	e.runner.Start(ctx, e.reg)
}

func (e *testEnv) fireWorkflow(ctx context.Context, t *testing.T, wfID, defID string) {
	t.Helper()
	reqEvt := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "grpc test",
		WorkflowID: defID,
	})).
		WithAggregate(wfID, 1).
		WithCorrelation(wfID).
		WithSource("grpc-test")

	if err := e.store.Append(ctx, wfID, 0, []event.Envelope{reqEvt}); err != nil {
		t.Fatalf("store append: %v", err)
	}
	if err := e.bus.Publish(ctx, reqEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// awaitResult subscribes for WorkflowCompleted or WorkflowFailed.
func awaitResult(t *testing.T, bus eventbus.Bus, wfID string) <-chan event.Envelope {
	t.Helper()
	ch := make(chan event.Envelope, 2)
	unsub1 := bus.Subscribe(event.WorkflowCompleted, func(_ context.Context, env event.Envelope) error {
		if env.AggregateID == wfID { ch <- env }
		return nil
	}, eventbus.WithName("test:completed"))
	unsub2 := bus.Subscribe(event.WorkflowFailed, func(_ context.Context, env event.Envelope) error {
		if env.AggregateID == wfID { ch <- env }
		return nil
	}, eventbus.WithName("test:failed"))
	t.Cleanup(func() { unsub1(); unsub2() })
	return ch
}

// =============================================================================
// Test: External handler registers via gRPC, enriches workflow, completes
// =============================================================================

func TestGRPCExternalHandlerE2E(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "grpc-test",
		Required: []string{"architect", "frontend-enricher", "developer"},
		Graph: map[string][]string{
			"architect":        {},
			"frontend-enricher": {"architect"},
			"developer":        {"architect"},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	// Register local handlers: architect + developer.
	if err := env.reg.Register(&stubTriggeredHandler{
		name:    "architect",
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("grpc-test")}},
		handleFn: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return []event.Envelope{
				event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
					Phase:   "architect",
					Backend: "claude",
					Output:  json.RawMessage(`"Build a dashboard with React"`),
				})),
			}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := env.reg.Register(&stubTriggeredHandler{
		name:    "developer",
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"architect"}},
		handleFn: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			return nil, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result := awaitResult(t, env.bus, "wf-grpc")
	env.start(ctx)

	// Connect external handler via gRPC stream.
	conn, err := grpc.NewClient(env.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewPersonaServiceClient(conn)
	stream, err := client.HandleStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	// Send registration.
	if err := stream.Send(&pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Registration{
			Registration: &pb.HandlerRegistration{
				Name:              "frontend-enricher",
				EventTypes:        []string{"persona.completed"},
				AfterPersonas:     []string{"architect"},
				BeforeHookTargets: []string{"developer"},
			},
		},
	}); err != nil {
		t.Fatalf("send registration: %v", err)
	}

	// Wait for ack.
	ackMsg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	ack := ackMsg.GetAck()
	if ack == nil || ack.Status != "ok" {
		t.Fatalf("expected ack ok, got %v", ack)
	}
	t.Logf("registration ack: %s status=%s", ack.Name, ack.Status)

	// Give PersonaRunner time to wire up the new handler.
	time.Sleep(50 * time.Millisecond)

	// Fire the workflow.
	env.fireWorkflow(ctx, t, "wf-grpc", "grpc-test")

	// External handler loop: receive dispatch requests, send results.
	go func() {
		for {
			msg, err := stream.Recv()
			if err == io.EOF || ctx.Err() != nil {
				return
			}
			if err != nil {
				t.Logf("external handler recv: %v", err)
				return
			}
			dispatch := msg.GetDispatch()
			if dispatch == nil {
				continue
			}
			t.Logf("external handler received dispatch: id=%s event=%s",
				dispatch.DispatchId, dispatch.Event.Type)

			// Simulate enrichment: emit a ContextEnrichment event.
			enrichPayload, _ := json.Marshal(event.ContextEnrichmentPayload{
				Source:  "frontend-enricher",
				Kind:    "libraries",
				Summary: "Use tanstack-table for data grids",
				Items: []event.EnrichmentItem{
					{Name: "tanstack-table", Version: "^8.0.0", Reason: "data grid support"},
				},
			})

			if err := stream.Send(&pb.HandlerMessage{
				Msg: &pb.HandlerMessage_Result{
					Result: &pb.HandlerResult{
						DispatchId: dispatch.DispatchId,
						Events: []*pb.EventEnvelope{
							{
								Id:      string(event.NewID()),
								Type:    string(event.ContextEnrichment),
								Payload: enrichPayload,
								Source:  "handler:frontend-enricher",
							},
						},
					},
				},
			}); err != nil {
				t.Logf("external handler send: %v", err)
				return
			}
		}
	}()

	// Wait for workflow completion.
	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
		t.Log("workflow completed via gRPC external handler")
	case <-time.After(10 * time.Second):
		events, _ := env.store.Load(ctx, "wf-grpc")
		for _, e := range events {
			t.Logf("  event: %s (v%d, agg=%s)", e.Type, e.Version, e.AggregateID)
		}
		corr, _ := env.store.LoadByCorrelation(ctx, "wf-grpc")
		t.Logf("  --- correlation events ---")
		for _, e := range corr {
			t.Logf("  event: %s (agg=%s, src=%s)", e.Type, e.AggregateID, e.Source)
		}
		t.Fatal("timeout: gRPC workflow never completed")
	}

	// Verify enrichment event is in the correlation chain.
	corr, _ := env.store.LoadByCorrelation(ctx, "wf-grpc")
	var sawEnrichment bool
	for _, e := range corr {
		if e.Type == event.ContextEnrichment {
			sawEnrichment = true
		}
	}
	if !sawEnrichment {
		t.Error("expected ContextEnrichment event in correlation chain")
	}
}

// TestGRPCDisconnectCleansUpHooksAndSubscriptions verifies that when a gRPC
// handler disconnects, its before-hooks are removed and bus subscriptions are
// unsubscribed. Without cleanup, the target persona would wait forever for a
// dead handler.
func TestGRPCDisconnectCleansUpHooksAndSubscriptions(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "grpc-cleanup",
		Required: []string{"architect", "developer"},
		Graph: map[string][]string{
			"architect": {},
			"developer": {"architect"},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	// Register local handlers.
	if err := env.reg.Register(&stubTriggeredHandler{
		name:    "architect",
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("grpc-cleanup")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(&stubTriggeredHandler{
		name:    "developer",
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"architect"}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	env.start(ctx)

	// Connect external handler via gRPC.
	conn, err := grpc.NewClient(env.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewPersonaServiceClient(conn)
	stream, err := client.HandleStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	// Register with a before-hook on developer.
	if err := stream.Send(&pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Registration{
			Registration: &pb.HandlerRegistration{
				Name:              "ephemeral-enricher",
				EventTypes:        []string{"persona.completed"},
				AfterPersonas:     []string{"architect"},
				BeforeHookTargets: []string{"developer"},
			},
		},
	}); err != nil {
		t.Fatalf("send registration: %v", err)
	}

	ack, _ := stream.Recv()
	if ack.GetAck().Status != "ok" {
		t.Fatalf("registration failed: %v", ack)
	}

	time.Sleep(50 * time.Millisecond)

	// Close the stream — simulates handler crash / shutdown.
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}

	// Give the server time to process the disconnect and clean up.
	time.Sleep(200 * time.Millisecond)

	// Now fire a workflow. Developer should complete WITHOUT waiting for
	// ephemeral-enricher — the hook was cleaned up on disconnect.
	result := awaitResult(t, env.bus, "wf-cleanup")
	env.fireWorkflow(ctx, t, "wf-cleanup", "grpc-cleanup")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
		t.Log("workflow completed after handler disconnect — hooks cleaned up correctly")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: developer is still gated by dead hook — UnregisterHook not working")
	}
}

// =============================================================================
// Test: Inject event via gRPC stream — happy path
// =============================================================================

func TestGRPCInjectEvent_HappyPath(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "inject-test",
		Required: []string{"researcher"},
		Graph: map[string][]string{
			"researcher": {},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	env.start(ctx)

	// Seed a running workflow.
	env.fireWorkflow(ctx, t, "wf-inject", "inject-test")
	time.Sleep(50 * time.Millisecond) // let engine process WorkflowRequested → WorkflowStarted

	// Connect external handler via gRPC (producer-only: subscribes to nothing).
	conn, err := grpc.NewClient(env.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewPersonaServiceClient(conn)
	stream, err := client.HandleStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	// Register with no subscriptions (producer-only).
	if err := stream.Send(&pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Registration{
			Registration: &pb.HandlerRegistration{
				Name: "ci-injector",
			},
		},
	}); err != nil {
		t.Fatalf("send registration: %v", err)
	}

	ack, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	if ack.GetAck().Status != "ok" {
		t.Fatalf("expected ack ok, got %v", ack)
	}

	// Inject OperatorGuidance.
	payload, _ := json.Marshal(map[string]string{"guidance": "use React for the frontend"})
	if err := stream.Send(&pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Inject{
			Inject: &pb.InjectEventRequest{
				RequestId: "req-1",
				Event: &pb.EventEnvelope{
					Type:          string(event.OperatorGuidance),
					CorrelationId: "wf-inject",
					Payload:       payload,
				},
			},
		},
	}); err != nil {
		t.Fatalf("send inject: %v", err)
	}

	// Wait for InjectEventResult.
	resultMsg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv inject result: %v", err)
	}
	ir := resultMsg.GetInjectResult()
	if ir == nil {
		t.Fatal("expected InjectEventResult")
	}
	if !ir.Success {
		t.Fatalf("inject failed: %s", ir.Error)
	}
	if ir.RequestId != "req-1" {
		t.Errorf("expected request_id req-1, got %s", ir.RequestId)
	}
	if ir.EventId == "" {
		t.Error("expected non-empty event_id")
	}

	// Verify persisted.
	events, _ := env.store.LoadByCorrelation(ctx, "wf-inject")
	var sawGuidance bool
	for _, e := range events {
		if e.Type == event.OperatorGuidance {
			sawGuidance = true
			if e.Source != "grpc:ci-injector" {
				t.Errorf("expected source grpc:ci-injector, got %s", e.Source)
			}
		}
	}
	if !sawGuidance {
		t.Error("expected OperatorGuidance in event stream")
	}
}

// =============================================================================
// Test: Inject blocked event type returns error
// =============================================================================

func TestGRPCInjectEvent_Blocked(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "inject-blocked",
		Required: []string{"researcher"},
		Graph: map[string][]string{
			"researcher": {},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	env.start(ctx)

	conn, err := grpc.NewClient(env.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewPersonaServiceClient(conn)
	stream, err := client.HandleStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	if err := stream.Send(&pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Registration{
			Registration: &pb.HandlerRegistration{Name: "malicious"},
		},
	}); err != nil {
		t.Fatalf("send registration: %v", err)
	}

	ack, _ := stream.Recv()
	if ack.GetAck().Status != "ok" {
		t.Fatalf("registration failed: %v", ack)
	}

	// Attempt to inject PersonaCompleted (blocked).
	if err := stream.Send(&pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Inject{
			Inject: &pb.InjectEventRequest{
				RequestId: "req-blocked",
				Event: &pb.EventEnvelope{
					Type:          string(event.PersonaCompleted),
					CorrelationId: "wf-any",
					Payload:       []byte(`{}`),
				},
			},
		},
	}); err != nil {
		t.Fatalf("send inject: %v", err)
	}

	resultMsg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv inject result: %v", err)
	}
	ir := resultMsg.GetInjectResult()
	if ir == nil {
		t.Fatal("expected InjectEventResult")
	}
	if ir.Success {
		t.Fatal("expected inject to fail for blocked type")
	}
	if ir.Error == "" {
		t.Error("expected non-empty error message")
	}
	t.Logf("blocked inject error: %s", ir.Error)
}

// =============================================================================
// Test: Producer-only handler starts a new workflow via injection
// =============================================================================

func TestGRPCInjectEvent_ProducerOnly_NewWorkflow(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "inject-new",
		Required: []string{"researcher"},
		Graph: map[string][]string{
			"researcher": {},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	env.start(ctx)

	conn, err := grpc.NewClient(env.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewPersonaServiceClient(conn)
	stream, err := client.HandleStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	// Register with empty event_types (producer-only).
	if err := stream.Send(&pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Registration{
			Registration: &pb.HandlerRegistration{Name: "workflow-starter"},
		},
	}); err != nil {
		t.Fatalf("send registration: %v", err)
	}

	ack, _ := stream.Recv()
	if ack.GetAck().Status != "ok" {
		t.Fatalf("registration failed: %v", ack)
	}

	// Inject WorkflowRequested to start a new workflow.
	payload := event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "build a dashboard",
		WorkflowID: "inject-new",
	})
	if err := stream.Send(&pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Inject{
			Inject: &pb.InjectEventRequest{
				RequestId: "req-start",
				Event: &pb.EventEnvelope{
					Type:          string(event.WorkflowRequested),
					CorrelationId: "wf-injected-new",
					Payload:       payload,
				},
			},
		},
	}); err != nil {
		t.Fatalf("send inject: %v", err)
	}

	resultMsg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv inject result: %v", err)
	}
	ir := resultMsg.GetInjectResult()
	if ir == nil {
		t.Fatal("expected InjectEventResult")
	}
	if !ir.Success {
		t.Fatalf("inject WorkflowRequested failed: %s", ir.Error)
	}

	// Verify the event is persisted.
	events, _ := env.store.Load(ctx, "wf-injected-new")
	if len(events) == 0 {
		t.Fatal("expected workflow events to be persisted")
	}
	if events[0].Type != event.WorkflowRequested {
		t.Errorf("expected WorkflowRequested, got %s", events[0].Type)
	}
	if events[0].Source != "grpc:workflow-starter" {
		t.Errorf("expected source grpc:workflow-starter, got %s", events[0].Source)
	}
}

// =============================================================================
// Test: Watch via gRPC stream — handler + watcher on same stream
// =============================================================================

func TestGRPCWatch_E2E(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "grpc-watch",
		Required: []string{"researcher"},
		Graph: map[string][]string{
			"researcher": {},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	// Register researcher as local handler (completes immediately).
	if err := env.reg.Register(&stubTriggeredHandler{
		name:    "researcher",
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("grpc-watch")}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	env.start(ctx)

	// Connect external handler (producer-only) + watch all workflows.
	conn, err := grpc.NewClient(env.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewPersonaServiceClient(conn)
	stream, err := client.HandleStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	// Register.
	if err := stream.Send(&pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Registration{
			Registration: &pb.HandlerRegistration{Name: "watcher-only"},
		},
	}); err != nil {
		t.Fatalf("send registration: %v", err)
	}
	ack, err := stream.Recv()
	if err != nil || ack.GetAck().Status != "ok" {
		t.Fatalf("ack: %v %v", ack, err)
	}

	// Watch all.
	if err := stream.Send(&pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Watch{Watch: &pb.WatchRequest{}},
	}); err != nil {
		t.Fatalf("send watch: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Fire workflow.
	env.fireWorkflow(ctx, t, "wf-watch-e2e", "grpc-watch")

	// Wait for notification on stream.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for workflow notification")
		default:
		}

		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}

		notif := msg.GetNotification()
		if notif == nil {
			continue // skip dispatch messages
		}

		if notif.CorrelationId != "wf-watch-e2e" {
			continue
		}

		if notif.Status != "completed" {
			t.Errorf("expected completed, got %s", notif.Status)
		}
		t.Logf("received notification: status=%s correlation=%s", notif.Status, notif.CorrelationId)
		return
	}
}

func TestGRPCWatch_DisconnectCleanup(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "grpc-watch-dc",
		Required: []string{"researcher"},
		Graph: map[string][]string{
			"researcher": {},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		name:    "researcher",
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("grpc-watch-dc")}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	env.start(ctx)

	conn, err := grpc.NewClient(env.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewPersonaServiceClient(conn)
	stream, err := client.HandleStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	if err := stream.Send(&pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Registration{
			Registration: &pb.HandlerRegistration{Name: "ephemeral-watcher"},
		},
	}); err != nil {
		t.Fatalf("send registration: %v", err)
	}

	ack, _ := stream.Recv()
	if ack.GetAck().Status != "ok" {
		t.Fatalf("ack: %v", ack)
	}

	// Watch all.
	if err := stream.Send(&pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Watch{Watch: &pb.WatchRequest{}},
	}); err != nil {
		t.Fatalf("send watch: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Disconnect.
	_ = stream.CloseSend()
	time.Sleep(200 * time.Millisecond)

	// Fire workflow — should NOT panic/leak from trying to write to closed channel.
	env.fireWorkflow(ctx, t, "wf-watch-dc", "grpc-watch-dc")

	// Wait for workflow to finish (verifies no panic).
	result := awaitResult(t, env.bus, "wf-watch-dc")
	select {
	case <-result:
		t.Log("workflow completed without panic after watcher disconnect")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: workflow never completed")
	}
}

// =============================================================================
// Test: Register workflow via gRPC stream
// =============================================================================

func TestGRPCRegisterWorkflow_E2E(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "base",
		Required: []string{"researcher"},
		Graph: map[string][]string{
			"researcher": {},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	// Register a local handler so it shows up as "available".
	if err := env.reg.Register(&stubTriggeredHandler{
		name:    "local-reviewer",
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	env.start(ctx)

	conn := dialTestServer(t, env.addr)

	client := NewClient(conn, ClientConfig{
		Name:       "workflow-registrar",
		EventTypes: []string{string(event.WorkflowStarted)},
		Handler:    func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		Logger:     testLogger(),
	})

	go func() { _ = client.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Register a custom workflow that references:
	// - "workflow-registrar" (the gRPC handler itself)
	// - "local-reviewer" (a local handler)
	// - "future-handler" (not yet connected)
	result, err := client.RegisterWorkflow(ctx, "custom-review",
		[]string{"workflow-registrar", "local-reviewer", "future-handler"},
		WithMaxIterations(1),
	)
	if err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got error: %s", result.Error)
	}

	// Verify available/missing classification.
	available := make(map[string]bool)
	for _, h := range result.AvailableHandlers {
		available[h] = true
	}
	missing := make(map[string]bool)
	for _, h := range result.MissingHandlers {
		missing[h] = true
	}

	if !available["workflow-registrar"] {
		t.Error("expected workflow-registrar to be available (connected gRPC handler)")
	}
	if !available["local-reviewer"] {
		t.Error("expected local-reviewer to be available (local handler)")
	}
	if !missing["future-handler"] {
		t.Error("expected future-handler to be missing")
	}

	t.Logf("registered workflow: available=%v missing=%v", result.AvailableHandlers, result.MissingHandlers)
	cancel()
}

// TestGRPCRegisterWorkflow_PreservesExistingGraph verifies that re-registering
// a workflow via gRPC preserves the existing Graph and RetriggeredBy fields.
// Regression test: gRPC registrations only provide Required/MaxIterations, so
// re-registering a built-in workflow would wipe its Graph, breaking DAG dispatch.
func TestGRPCRegisterWorkflow_PreservesExistingGraph(t *testing.T) {
	// Register a built-in workflow with a populated Graph.
	builtIn := engine.WorkflowDef{
		ID:       "plan-btu",
		Required: []string{"reader", "architect", "writer"},
		Graph: map[string][]string{
			"reader":    {},
			"architect": {"reader"},
			"writer":    {"architect"},
		},
		RetriggeredBy: map[string][]event.Type{
			"architect": {event.FeedbackGenerated},
		},
		HintThreshold: 0.5,
		MaxIterations: 3,
	}
	env := newTestEnv(t, builtIn)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	env.start(ctx)

	conn := dialTestServer(t, env.addr)
	client := NewClient(conn, ClientConfig{
		Name:       "registrar",
		EventTypes: []string{string(event.WorkflowStarted)},
		Handler:    func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		Logger:     testLogger(),
	})
	go func() { _ = client.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Re-register the same workflow via gRPC (no Graph in the request).
	result, err := client.RegisterWorkflow(ctx, "plan-btu",
		[]string{"reader", "architect", "writer"},
		WithMaxIterations(5),
	)
	if err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got error: %s", result.Error)
	}

	// Verify the Graph was preserved — not wiped by the gRPC registration.
	def, ok := env.eng.GetWorkflowDef("plan-btu")
	if !ok {
		t.Fatal("plan-btu workflow not found after re-registration")
	}
	if len(def.Graph) != 3 {
		t.Fatalf("expected Graph with 3 entries, got %d (Graph was wiped by gRPC registration)", len(def.Graph))
	}
	if _, exists := def.Graph["architect"]; !exists {
		t.Error("architect missing from Graph after re-registration")
	}
	if len(def.RetriggeredBy) == 0 {
		t.Error("RetriggeredBy was wiped by gRPC registration")
	}
	if def.HintThreshold != 0.5 {
		t.Errorf("HintThreshold changed: got %v, want 0.5", def.HintThreshold)
	}
	// MaxIterations should be updated from the gRPC request.
	if def.MaxIterations != 5 {
		t.Errorf("MaxIterations not updated: got %d, want 5", def.MaxIterations)
	}

	cancel()
}

func TestGRPCRegisterWorkflow_Validation(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "base",
		Required: []string{"researcher"},
		Graph: map[string][]string{
			"researcher": {},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	env.start(ctx)

	conn := dialTestServer(t, env.addr)

	client := NewClient(conn, ClientConfig{
		Name:    "validator",
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		Logger:  testLogger(),
	})

	go func() { _ = client.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Empty workflow_id.
	_, err := client.RegisterWorkflow(ctx, "", []string{"a"})
	if err == nil {
		t.Error("expected error for empty workflow_id")
	}

	// Empty required list.
	_, err = client.RegisterWorkflow(ctx, "empty-req", nil)
	if err == nil {
		t.Error("expected error for empty required list")
	}

	cancel()
}

// stubTriggeredHandler implements handler.Handler + handler.TriggeredHandler for tests.
type stubTriggeredHandler struct {
	name     string
	trigger  handler.Trigger
	handleFn func(context.Context, event.Envelope) ([]event.Envelope, error)
}

func (s *stubTriggeredHandler) Name() string             { return s.name }
func (s *stubTriggeredHandler) Subscribes() []event.Type { return s.trigger.Events }
func (s *stubTriggeredHandler) Trigger() handler.Trigger { return s.trigger }
func (s *stubTriggeredHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	if s.handleFn != nil {
		return s.handleFn(ctx, env)
	}
	return nil, nil
}
