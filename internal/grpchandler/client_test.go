package grpchandler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
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

// TestClientE2E_WorkflowCompletes verifies that a Client registers, receives a
// dispatch, and returns results that allow the workflow to complete.
func TestClientE2E_WorkflowCompletes(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "client-e2e",
		Required: []string{"architect", "frontend-enricher", "developer"},
		Graph: map[string][]string{
			"architect":         {},
			"frontend-enricher": {"architect"},
			"developer":         {"architect"},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	// Local handlers.
	mustRegister(t, env, "architect",
		handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("client-e2e")}},
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	)
	mustRegister(t, env, "developer",
		handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"architect"}},
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result := awaitResult(t, env.bus, "wf-client-e2e")
	env.start(ctx)

	conn := dialTestServer(t, env.addr)

	var dispatched atomic.Int64
	client := NewClient(conn, ClientConfig{
		Name:              "frontend-enricher",
		EventTypes:        []string{string(event.PersonaCompleted)},
		AfterPersonas:     []string{"architect"},
		BeforeHookTargets: []string{"developer"},
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			dispatched.Add(1)
			return nil, nil
		},
		Logger:     testLogger(),
		MaxRetries: 1,
	})

	clientDone := make(chan error, 1)
	go func() { clientDone <- client.Run(ctx) }()

	// Give the client time to register with the server.
	time.Sleep(100 * time.Millisecond)

	env.fireWorkflow(ctx, t, "wf-client-e2e", "client-e2e")

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: workflow never completed")
	}

	if dispatched.Load() == 0 {
		t.Error("expected handler to be dispatched at least once")
	}

	cancel() // unblock Run
	if err := <-clientDone; err != nil {
		t.Errorf("client.Run: %v", err)
	}
}

// TestClientDefaults verifies that zero-value BaseDelay and MaxDelay are
// replaced with sensible defaults.
func TestClientDefaults(t *testing.T) {
	conn, err := grpc.NewClient("localhost:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer conn.Close()

	c := NewClient(conn, ClientConfig{
		Name:    "test",
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	})
	if c.cfg.BaseDelay != defaultBaseDelay {
		t.Errorf("expected BaseDelay=%s, got %s", defaultBaseDelay, c.cfg.BaseDelay)
	}
	if c.cfg.MaxDelay != defaultMaxDelay {
		t.Errorf("expected MaxDelay=%s, got %s", defaultMaxDelay, c.cfg.MaxDelay)
	}
}

// TestClientMaxRetries verifies that Run returns an error once MaxRetries is
// exceeded. We point the client at a non-existent server so every attempt fails
// immediately.
func TestClientMaxRetries(t *testing.T) {
	conn, err := grpc.NewClient("localhost:1",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer conn.Close()

	c := NewClient(conn, ClientConfig{
		Name:       "test",
		Handler:    func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		Logger:     testLogger(),
		MaxRetries: 2,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   20 * time.Millisecond,
	})

	ctx := context.Background()
	err = c.Run(ctx)
	if err == nil {
		t.Fatal("expected error after max retries, got nil")
	}
}

// TestClientBackoff verifies the exponential backoff calculation.
func TestClientBackoff(t *testing.T) {
	c := &Client{
		cfg: ClientConfig{
			BaseDelay: 1 * time.Second,
			MaxDelay:  30 * time.Second,
		},
	}

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second}, // capped at MaxDelay
		{7, 30 * time.Second},
	}
	for _, tc := range cases {
		got := c.backoff(tc.attempt)
		if got != tc.want {
			t.Errorf("backoff(attempt=%d): got %s, want %s", tc.attempt, got, tc.want)
		}
	}
}

// TestClientHandlerError verifies that a handler error is encoded in the result
// message rather than closing the stream.
func TestClientHandlerError(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "client-handler-err",
		Required: []string{"architect", "erroring-handler", "developer"},
		Graph: map[string][]string{
			"architect":       {},
			"erroring-handler": {"architect"},
			"developer":       {"architect"},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	mustRegister(t, env, "architect",
		handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("client-handler-err")}},
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	)
	mustRegister(t, env, "developer",
		handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"architect"}},
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	env.start(ctx)

	conn := dialTestServer(t, env.addr)

	var dispatchCount atomic.Int64
	client := NewClient(conn, ClientConfig{
		Name:          "erroring-handler",
		EventTypes:    []string{string(event.PersonaCompleted)},
		AfterPersonas: []string{"architect"},
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			dispatchCount.Add(1)
			return nil, errors.New("injected handler error")
		},
		Logger:     testLogger(),
		MaxRetries: 1,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   20 * time.Millisecond,
	})

	go func() { _ = client.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)

	env.fireWorkflow(ctx, t, "wf-err", "client-handler-err")

	// Wait briefly: PersonaRunner receives an error result and emits PersonaFailed,
	// which is valid — we just confirm the stream didn't crash.
	time.Sleep(500 * time.Millisecond)

	if dispatchCount.Load() == 0 {
		t.Error("expected handler to be called, got 0 dispatches")
	}

	cancel()
}

// TestClientContextCancel verifies that cancelling the context causes Run to
// return nil cleanly without waiting for MaxRetries.
func TestClientContextCancel(t *testing.T) {
	conn, err := grpc.NewClient("localhost:1",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())

	c := NewClient(conn, ClientConfig{
		Name:       "test",
		Handler:    func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		Logger:     testLogger(),
		MaxRetries: 0, // unlimited — so only ctx cancellation stops it
		BaseDelay:  50 * time.Millisecond,
		MaxDelay:   100 * time.Millisecond,
	})

	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Let at least one attempt fail, then cancel.
	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil on ctx cancel, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// --- helpers ---

func dialTestServer(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func mustRegister(t *testing.T, env *testEnv, name string, trigger handler.Trigger,
	fn func(context.Context, event.Envelope) ([]event.Envelope, error)) {
	t.Helper()
	if err := env.reg.Register(&stubTriggeredHandler{
		name:     name,
		trigger:  trigger,
		handleFn: fn,
	}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestClientInjectEvent_HappyPath verifies that Client.InjectEvent sends an
// inject request and receives the result through the bidirectional stream.
func TestClientInjectEvent_HappyPath(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "client-inject",
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
	env.fireWorkflow(ctx, t, "wf-client-inject", "client-inject")
	time.Sleep(50 * time.Millisecond)

	conn := dialTestServer(t, env.addr)

	client := NewClient(conn, ClientConfig{
		Name:    "inject-client",
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		Logger:  testLogger(),
	})

	go func() { _ = client.Run(ctx) }()

	// Wait for stream to be established.
	time.Sleep(200 * time.Millisecond)

	payload, _ := json.Marshal(map[string]string{"guidance": "use Vue"})
	eventID, err := client.InjectEvent(ctx, "wf-client-inject", event.OperatorGuidance, payload)
	if err != nil {
		t.Fatalf("InjectEvent: %v", err)
	}
	if eventID == "" {
		t.Error("expected non-empty event ID")
	}

	// Verify persisted.
	events, _ := env.store.LoadByCorrelation(ctx, "wf-client-inject")
	var found bool
	for _, e := range events {
		if e.Type == event.OperatorGuidance {
			found = true
		}
	}
	if !found {
		t.Error("OperatorGuidance not found in event stream")
	}
}

// TestClientInjectEvent_Error verifies that injection errors are returned.
func TestClientInjectEvent_Error(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "client-inject-err",
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
		Name:    "inject-err-client",
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		Logger:  testLogger(),
	})

	go func() { _ = client.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Inject blocked type.
	_, err := client.InjectEvent(ctx, "wf-any", event.PersonaCompleted, []byte(`{}`))
	if err == nil {
		t.Fatal("expected error for blocked event type")
	}
}

// TestClientInjectEvent_NoStream verifies error when no stream is active.
func TestClientInjectEvent_NoStream(t *testing.T) {
	conn, err := grpc.NewClient("localhost:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer conn.Close()

	client := NewClient(conn, ClientConfig{
		Name:    "no-stream",
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	})

	_, err = client.InjectEvent(context.Background(), "wf-1", event.OperatorGuidance, []byte(`{}`))
	if err == nil {
		t.Fatal("expected error when no stream is active")
	}
}

// TestClientInjectEvent_ConcurrentWithDispatch verifies that inject and dispatch
// can operate concurrently without deadlock. Uses a slow handler to ensure the
// workflow stays running while inject happens.
func TestClientInjectEvent_ConcurrentWithDispatch(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "client-concurrent",
		Required: []string{"architect", "concurrent-handler", "slow-dep"},
		Graph: map[string][]string{
			"architect":         {},
			"concurrent-handler": {"architect"},
			"slow-dep":          {"concurrent-handler"},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	mustRegister(t, env, "architect",
		handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("client-concurrent")}},
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	)
	// slow-dep never completes, keeping the workflow running.
	mustRegister(t, env, "slow-dep",
		handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"concurrent-handler"}},
		func(ctx context.Context, _ event.Envelope) ([]event.Envelope, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	env.start(ctx)

	conn := dialTestServer(t, env.addr)

	dispatched := make(chan struct{}, 1)
	client := NewClient(conn, ClientConfig{
		Name:          "concurrent-handler",
		EventTypes:    []string{string(event.PersonaCompleted)},
		AfterPersonas: []string{"architect"},
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			select {
			case dispatched <- struct{}{}:
			default:
			}
			return nil, nil
		},
		Logger: testLogger(),
	})

	go func() { _ = client.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Fire workflow to trigger dispatches.
	env.fireWorkflow(ctx, t, "wf-concurrent", "client-concurrent")

	// Wait for handler dispatch before injecting.
	select {
	case <-dispatched:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for dispatch")
	}

	// Inject while workflow is still running.
	payload, _ := json.Marshal(map[string]string{"data": "concurrent"})
	_, err := client.InjectEvent(ctx, "wf-concurrent", event.OperatorGuidance, payload)
	if err != nil {
		t.Fatalf("concurrent InjectEvent: %v", err)
	}

	// Verify both inject and dispatch worked.
	events, _ := env.store.LoadByCorrelation(ctx, "wf-concurrent")
	var sawGuidance bool
	for _, e := range events {
		if e.Type == event.OperatorGuidance {
			sawGuidance = true
		}
	}
	if !sawGuidance {
		t.Error("OperatorGuidance not found — inject during dispatch failed")
	}
}

// TestClient_NotificationHandler verifies that the NotificationHandler callback
// is invoked when a workflow notification is pushed by the server.
func TestClient_NotificationHandler(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "client-notif",
		Required: []string{"researcher"},
		Graph: map[string][]string{
			"researcher": {},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		name:    "researcher",
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("client-notif")}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	env.start(ctx)

	conn := dialTestServer(t, env.addr)

	notifCh := make(chan *pb.WorkflowNotification, 4)
	client := NewClient(conn, ClientConfig{
		Name:    "notif-handler",
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		NotificationHandler: func(_ context.Context, notif *pb.WorkflowNotification) {
			notifCh <- notif
		},
		WatchAll: true,
		Logger:   testLogger(),
	})

	go func() { _ = client.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	env.fireWorkflow(ctx, t, "wf-client-notif", "client-notif")

	select {
	case notif := <-notifCh:
		if notif.CorrelationId != "wf-client-notif" {
			t.Errorf("expected correlation wf-client-notif, got %s", notif.CorrelationId)
		}
		if notif.Status != "completed" {
			t.Errorf("expected status completed, got %s", notif.Status)
		}
		t.Logf("received notification via Client: status=%s", notif.Status)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for notification callback")
	}

	cancel()
}

// TestClient_WatchWorkflow_Dynamic verifies that WatchWorkflow() after connect
// causes notifications to arrive for the specified correlation.
func TestClient_WatchWorkflow_Dynamic(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "client-dyn-watch",
		Required: []string{"researcher"},
		Graph: map[string][]string{
			"researcher": {},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	if err := env.reg.Register(&stubTriggeredHandler{
		name:    "researcher",
		trigger: handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("client-dyn-watch")}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	env.start(ctx)

	conn := dialTestServer(t, env.addr)

	notifCh := make(chan *pb.WorkflowNotification, 4)
	client := NewClient(conn, ClientConfig{
		Name:    "dyn-watcher",
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		NotificationHandler: func(_ context.Context, notif *pb.WorkflowNotification) {
			notifCh <- notif
		},
		Logger: testLogger(),
	})

	go func() { _ = client.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Dynamic watch.
	if err := client.WatchWorkflow(ctx, "wf-dyn-watch"); err != nil {
		t.Fatalf("WatchWorkflow: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	env.fireWorkflow(ctx, t, "wf-dyn-watch", "client-dyn-watch")

	select {
	case notif := <-notifCh:
		if notif.CorrelationId != "wf-dyn-watch" {
			t.Errorf("expected correlation wf-dyn-watch, got %s", notif.CorrelationId)
		}
		if notif.Status != "completed" {
			t.Errorf("expected status completed, got %s", notif.Status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for dynamic watch notification")
	}

	cancel()
}

// TestClient_ErrIncomplete verifies that when the handler callback returns
// handler.ErrIncomplete, the client sets Incomplete=true on the proto result
// (not the Error field), and the workflow does NOT complete prematurely.
func TestClient_ErrIncomplete(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "client-incomplete",
		Required: []string{"reader", "incomplete-handler"},
		Graph: map[string][]string{
			// incomplete-handler subscribes to PersonaCompleted AND ChildWorkflowCompleted.
			// The ChildWorkflowCompleted re-trigger is not expressible as a DAG dep.
			// Omitting it from the Graph so it falls back to trigger-declared events,
			// keeping both subscriptions active (gRPC compat path).
			"reader": {},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	// Local reader — fires first, completes normally.
	mustRegister(t, env, "reader",
		handler.Trigger{Events: []event.Type{event.WorkflowStartedFor("client-incomplete")}},
		func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result := awaitResult(t, env.bus, "wf-client-incomplete")
	env.start(ctx)

	conn := dialTestServer(t, env.addr)

	var dispatchCount atomic.Int64
	client := NewClient(conn, ClientConfig{
		Name:          "incomplete-handler",
		EventTypes:    []string{string(event.PersonaCompleted), string(event.ChildWorkflowCompleted)},
		AfterPersonas: []string{"reader"},
		Handler: func(_ context.Context, dispatchEnv event.Envelope) ([]event.Envelope, error) {
			n := dispatchCount.Add(1)
			if n == 1 {
				// First call: return result events + ErrIncomplete.
				enrichment := event.New("context.enrichment", 1,
					event.MustMarshal(event.ContextEnrichmentPayload{
						Source: "incomplete-handler", Kind: "batch", Summary: "dispatched",
					}))
				return []event.Envelope{enrichment}, handler.ErrIncomplete
			}
			// Second call: complete normally.
			return nil, nil
		},
		Logger:     testLogger(),
		MaxRetries: 1,
	})

	clientDone := make(chan error, 1)
	go func() { clientDone <- client.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	env.fireWorkflow(ctx, t, "wf-client-incomplete", "client-incomplete")

	// After reader → incomplete-handler fires but returns ErrIncomplete.
	// Workflow should NOT complete yet.
	time.Sleep(500 * time.Millisecond)
	select {
	case got := <-result:
		t.Fatalf("workflow completed prematurely with %s", got.Type)
	default:
		// Expected: workflow still running.
	}

	// Inject ChildWorkflowCompleted to wake the handler again.
	time.Sleep(100 * time.Millisecond)
	payload := event.MustMarshal(event.ChildWorkflowCompletedPayload{
		ParentCorrelation: "wf-client-incomplete",
		ChildCorrelation:  "child-1",
		Status:            "completed",
	})
	_, err := client.InjectEvent(ctx, "wf-client-incomplete", event.ChildWorkflowCompleted, payload)
	if err != nil {
		t.Fatalf("InjectEvent: %v", err)
	}

	// Now the handler should fire again, return nil, emit PersonaCompleted,
	// and the workflow should complete.
	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timeout: workflow never completed (dispatches=%d)", dispatchCount.Load())
	}

	if n := dispatchCount.Load(); n != 2 {
		t.Errorf("expected 2 dispatches, got %d", n)
	}

	cancel()
	if err := <-clientDone; err != nil {
		t.Errorf("client.Run: %v", err)
	}
}

// TestClientHandleDispatch_ErrIncomplete_SetsIncompleteFlag verifies at the
// unit level that handleDispatch maps handler.ErrIncomplete to Incomplete=true
// on the proto result without setting the Error field.
func TestClientHandleDispatch_ErrIncomplete_SetsIncompleteFlag(t *testing.T) {
	conn, err := grpc.NewClient("localhost:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer conn.Close()

	c := NewClient(conn, ClientConfig{
		Name: "test-incomplete",
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
			enrichment := event.New("context.enrichment", 1, []byte(`{"kind":"test"}`))
			return []event.Envelope{enrichment}, handler.ErrIncomplete
		},
	})

	dispatch := &pb.DispatchRequest{
		DispatchId: "d-1",
		Event: &pb.EventEnvelope{
			Type:          "persona.completed",
			CorrelationId: "wf-1",
			Payload:       []byte(`{}`),
		},
	}

	msg := c.handleDispatch(context.Background(), dispatch)
	result := msg.GetResult()
	if result == nil {
		t.Fatal("expected HandlerResult in message")
	}
	if !result.Incomplete {
		t.Error("expected Incomplete=true")
	}
	if result.Error != "" {
		t.Errorf("expected empty Error field, got %q", result.Error)
	}
	if len(result.Events) != 1 {
		t.Errorf("expected 1 result event, got %d", len(result.Events))
	}
}

// TestClient_RegisterWorkflow verifies that RegisterWorkflow sends a request
// and receives a result with available/missing handler classification.
func TestClient_RegisterWorkflow(t *testing.T) {
	def := engine.WorkflowDef{
		ID:       "client-reg-wf",
		Required: []string{"researcher"},
		Graph: map[string][]string{
			"researcher": {},
		},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	mustRegister(t, env, "local-handler",
		handler.Trigger{Events: []event.Type{event.WorkflowStarted}},
		nil,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	env.start(ctx)

	conn := dialTestServer(t, env.addr)

	client := NewClient(conn, ClientConfig{
		Name:    "registrar-client",
		Handler: func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) { return nil, nil },
		Logger:  testLogger(),
	})

	go func() { _ = client.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	result, err := client.RegisterWorkflow(ctx, "dynamic-wf",
		[]string{"local-handler", "registrar-client", "not-here"},
		WithMaxIterations(2),
	)
	if err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success: %s", result.Error)
	}
	if len(result.MissingHandlers) != 1 || result.MissingHandlers[0] != "not-here" {
		t.Errorf("expected missing=[not-here], got %v", result.MissingHandlers)
	}
	if len(result.AvailableHandlers) != 2 {
		t.Errorf("expected 2 available, got %v", result.AvailableHandlers)
	}

	cancel()
}
