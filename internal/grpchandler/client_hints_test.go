package grpchandler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// TestClient_HintHandler_CalledForHintOnlyDispatch verifies that when the
// Client receives a DispatchRequest with HintOnly=true and cfg.HintHandler is
// set, the HintHandler is called instead of Handler.
func TestClient_HintHandler_CalledForHintOnlyDispatch(t *testing.T) {
	conn, err := grpc.NewClient("localhost:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer conn.Close()

	var hintCalled atomic.Int64
	var handleCalled atomic.Int64

	c := NewClient(conn, ClientConfig{
		Name: "hint-router",
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			handleCalled.Add(1)
			return nil, nil
		},
		HintHandler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			hintCalled.Add(1)
			return []event.Envelope{
				event.New("hint.emitted", 1, []byte(`{"confidence":0.9}`)),
			}, nil
		},
	})

	// Dispatch with HintOnly=true — should route to HintHandler.
	hintDispatch := &pb.DispatchRequest{
		DispatchId: "hint-dispatch-1",
		HintOnly:   true,
		Event: &pb.EventEnvelope{
			Type:          "workflow.started",
			CorrelationId: "wf-hint",
			Payload:       []byte(`{}`),
			TimestampMs:   1000,
		},
	}
	msg := c.handleDispatch(context.Background(), hintDispatch)
	result := msg.GetResult()
	if result == nil {
		t.Fatal("expected HandlerResult")
	}
	if hintCalled.Load() != 1 {
		t.Errorf("expected HintHandler called once, got %d", hintCalled.Load())
	}
	if handleCalled.Load() != 0 {
		t.Errorf("expected Handler NOT called for hint dispatch, got %d", handleCalled.Load())
	}
	if len(result.Events) != 1 {
		t.Errorf("expected 1 hint event, got %d", len(result.Events))
	}

	// Dispatch with HintOnly=false — should route to Handler.
	normalDispatch := &pb.DispatchRequest{
		DispatchId: "normal-dispatch-1",
		HintOnly:   false,
		Event: &pb.EventEnvelope{
			Type:          "hint.approved",
			CorrelationId: "wf-hint",
			Payload:       []byte(`{}`),
			TimestampMs:   1000,
		},
	}
	msg2 := c.handleDispatch(context.Background(), normalDispatch)
	result2 := msg2.GetResult()
	if result2 == nil {
		t.Fatal("expected HandlerResult for normal dispatch")
	}
	if handleCalled.Load() != 1 {
		t.Errorf("expected Handler called once, got %d", handleCalled.Load())
	}
	if hintCalled.Load() != 1 {
		t.Errorf("expected HintHandler not called again, still %d", hintCalled.Load())
	}
}

// TestClient_HintHandler_FallsBackToHandler_WhenNil verifies that when
// cfg.HintHandler is nil and a hint_only dispatch arrives, cfg.Handler is
// used as fallback (backwards-compatible behavior).
func TestClient_HintHandler_FallsBackToHandler_WhenNil(t *testing.T) {
	conn, err := grpc.NewClient("localhost:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer conn.Close()

	var handlerCalled atomic.Bool
	c := NewClient(conn, ClientConfig{
		Name: "no-hint-handler",
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			handlerCalled.Store(true)
			return nil, nil
		},
		// HintHandler is intentionally nil — Handler must be the fallback.
	})

	dispatch := &pb.DispatchRequest{
		DispatchId: "hint-fallback-1",
		HintOnly:   true,
		Event: &pb.EventEnvelope{
			Type:          "workflow.started",
			CorrelationId: "wf-fallback",
			Payload:       []byte(`{}`),
			TimestampMs:   1000,
		},
	}

	msg := c.handleDispatch(context.Background(), dispatch)
	result := msg.GetResult()
	if result == nil {
		t.Fatal("expected HandlerResult")
	}
	if !handlerCalled.Load() {
		t.Error("expected cfg.Handler to be called when HintHandler is nil and dispatch is hint_only")
	}
	if result.Error != "" {
		t.Errorf("expected no error, got %q", result.Error)
	}
}

// TestClient_UnwatchWorkflow_HappyPath verifies that WatchWorkflow followed by
// UnwatchWorkflow removes the watcher so no notifications arrive for the
// unwatched correlation.
func TestClient_UnwatchWorkflow_HappyPath(t *testing.T) {
	def := engine.WorkflowDef{
		ID:            "client-unwatch-wf",
		Required:      []string{"researcher"},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	mustRegister(t, env, "researcher",
		handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("client-unwatch-wf")}},
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	env.start(ctx)

	conn := dialTestServer(t, env.addr)

	notifCh := make(chan *pb.WorkflowNotification, 4)
	client := NewClient(conn, ClientConfig{
		Name:    "unwatch-tester",
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		NotificationHandler: func(_ context.Context, notif *pb.WorkflowNotification) {
			notifCh <- notif
		},
		Logger: testLogger(),
	})

	go func() { _ = client.Run(ctx) }()
	time.Sleep(150 * time.Millisecond)

	// Watch, then immediately unwatch.
	if err := client.WatchWorkflow(ctx, "wf-unwatch-happy"); err != nil {
		t.Fatalf("WatchWorkflow: %v", err)
	}
	if err := client.UnwatchWorkflow(ctx, "wf-unwatch-happy"); err != nil {
		t.Fatalf("UnwatchWorkflow: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Fire the workflow — notification must NOT arrive for the unwatched correlation.
	env.fireWorkflow(ctx, t, "wf-unwatch-happy", "client-unwatch-wf")

	time.Sleep(1 * time.Second)

	select {
	case notif := <-notifCh:
		if notif.CorrelationId == "wf-unwatch-happy" {
			t.Errorf("expected no notification after unwatch, got status=%s for %s",
				notif.Status, notif.CorrelationId)
		}
		// Ignore notifications for other correlations.
	default:
		t.Log("no notification for unwatched workflow — correct behavior")
	}

	cancel()
}

// TestClient_UnwatchWorkflow_NoStream verifies that UnwatchWorkflow returns an
// error when no active stream exists.
func TestClient_UnwatchWorkflow_NoStream(t *testing.T) {
	conn, err := grpc.NewClient("localhost:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer conn.Close()

	client := NewClient(conn, ClientConfig{
		Name:    "no-stream-unwatch",
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	})

	err = client.UnwatchWorkflow(context.Background(), "wf-1")
	if err == nil {
		t.Fatal("expected error when no stream is active, got nil")
	}
}
