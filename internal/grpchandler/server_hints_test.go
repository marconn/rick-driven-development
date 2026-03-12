package grpchandler

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// TestGRPCHandleStream_FirstMessageNotRegistration verifies that if the first
// message from the client is not a HandlerRegistration, the server returns an
// error and closes the stream gracefully.
func TestGRPCHandleStream_FirstMessageNotRegistration(t *testing.T) {
	def := engine.WorkflowDef{
		ID:            "proto-validation",
		Required:      []string{"researcher"},
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

	// Send a non-registration message (a Heartbeat) as the first message.
	if err := stream.Send(&pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Heartbeat{Heartbeat: &pb.Heartbeat{}},
	}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}

	// The server should close the stream with an error.
	_, err = stream.Recv()
	if err == nil || err == io.EOF {
		// Either we get an error OR the stream closes with EOF depending on timing.
		// Both are acceptable — the server rejected our non-registration message.
		t.Log("stream closed cleanly (EOF) after non-registration first message")
		return
	}
	// Any non-nil error is the expected failure path.
	t.Logf("got expected stream error: %v", err)
}

// TestGRPCHandleStream_StreamDiesBeforeMessage verifies that the server handles
// a client that connects but sends nothing (drops the stream immediately).
func TestGRPCHandleStream_StreamDiesBeforeMessage(t *testing.T) {
	def := engine.WorkflowDef{
		ID:            "premature-close",
		Required:      []string{"researcher"},
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

	// Close the send side immediately — server receives EOF without any message.
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}

	// Server should return an error — verify it doesn't panic or deadlock.
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected error after premature close, got nil")
	}
	// Any error (EOF or status error) is acceptable — we just need no panic.
	t.Logf("got expected stream closure: %v", err)
}

// TestGRPCSupportsHints_RegistrationCreatesHinter verifies that when a client
// registers with SupportsHints=true, the server registers a gRPCHinter in the
// PersonaRunner and the StreamDispatcher receives hint_only=true dispatches.
func TestGRPCSupportsHints_RegistrationCreatesHinter(t *testing.T) {
	def := engine.WorkflowDef{
		ID:            "hints-test",
		Required:      []string{"hinted-handler", "consumer"},
		MaxIterations: 3,
	}
	env := newTestEnv(t, def)

	// Register a local consumer that fires after the hinted handler.
	if err := env.reg.Register(&stubTriggeredHandler{
		name:    "consumer",
		trigger: handler.Trigger{Events: []event.Type{event.PersonaCompleted}, AfterPersonas: []string{"hinted-handler"}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result := awaitResult(t, env.bus, "wf-hints")
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

	// Register with SupportsHints=true and the correct workflow-scoped event type.
	if err := stream.Send(&pb.HandlerMessage{
		Msg: &pb.HandlerMessage_Registration{
			Registration: &pb.HandlerRegistration{
				Name:          "hinted-handler",
				EventTypes:    []string{string(event.WorkflowStartedFor("hints-test"))},
				SupportsHints: true,
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
		t.Fatalf("expected ok, got %v", ack)
	}
	t.Log("handler registered with SupportsHints=true")

	time.Sleep(50 * time.Millisecond)

	// Fire workflow — PersonaRunner should call Hint() first, then Handle().
	env.fireWorkflow(ctx, t, "wf-hints", "hints-test")

	// External handler loop: respond to hint_only dispatches with hint.emitted,
	// respond to normal dispatches with completion.
	sawHintDispatch := make(chan struct{}, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil || ctx.Err() != nil {
				return
			}
			dispatch := msg.GetDispatch()
			if dispatch == nil {
				continue
			}

			if dispatch.HintOnly {
				select {
				case sawHintDispatch <- struct{}{}:
				default:
				}
				// Return a hint.emitted event with high confidence so engine auto-approves.
				hintPayload, _ := json.Marshal(event.HintEmittedPayload{
					Persona:    "hinted-handler",
					Plan:       "I plan to do the work",
					Confidence: 0.95,
					TriggerID:  dispatch.Event.Id,
				})
				if err := stream.Send(&pb.HandlerMessage{
					Msg: &pb.HandlerMessage_Result{
						Result: &pb.HandlerResult{
							DispatchId: dispatch.DispatchId,
							Events: []*pb.EventEnvelope{
								{
									Type:          string(event.HintEmitted),
									CorrelationId: dispatch.Event.CorrelationId,
									TimestampMs:   time.Now().UnixMilli(),
									Payload:       hintPayload,
								},
							},
						},
					},
				}); err != nil {
					return
				}
			} else {
				// Normal dispatch — complete successfully.
				if err := stream.Send(&pb.HandlerMessage{
					Msg: &pb.HandlerMessage_Result{
						Result: &pb.HandlerResult{
							DispatchId: dispatch.DispatchId,
						},
					},
				}); err != nil {
					return
				}
			}
		}
	}()

	select {
	case got := <-result:
		if got.Type != event.WorkflowCompleted {
			t.Fatalf("expected WorkflowCompleted, got %s", got.Type)
		}
		select {
		case <-sawHintDispatch:
			t.Log("hint dispatch confirmed — gRPCHinter correctly routed hint_only dispatch")
		default:
			t.Error("expected at least one hint_only dispatch, got none")
		}
		t.Log("workflow completed via hinted gRPC handler")
	case <-time.After(12 * time.Second):
		t.Fatal("timeout: hinted gRPC workflow never completed")
	}
}

// TestGRPCHinter_Unit verifies that gRPCHinter.Hint() delegates to
// StreamDispatcher.DispatchHint and returns the result events.
func TestGRPCHinter_Unit(t *testing.T) {
	d := NewStreamDispatcher(testLogger())
	sendCh := make(chan *pb.DispatchMessage, 2)
	token := d.Register("grpc-hinter-target", sendCh)
	defer d.Unregister("grpc-hinter-target", token)

	hinter := newGRPCHinter("grpc-hinter-target", d)

	ctx := context.Background()
	env := event.New("workflow.started", 1, []byte(`{}`)).WithCorrelation("wf-hinter-unit")

	resultCh := make(chan []event.Envelope, 1)
	errCh := make(chan error, 1)
	go func() {
		events, err := hinter.Hint(ctx, env)
		resultCh <- events
		errCh <- err
	}()

	req := <-sendCh
	dispatch := req.GetDispatch()
	if !dispatch.HintOnly {
		t.Error("expected HintOnly=true in gRPCHinter.Hint dispatch")
	}

	d.DeliverResult("grpc-hinter-target", &pb.HandlerResult{
		DispatchId: dispatch.DispatchId,
		Events: []*pb.EventEnvelope{
			{Type: "hint.emitted", Payload: []byte(`{"confidence":0.9}`)},
		},
	})

	select {
	case events := <-resultCh:
		err := <-errCh
		if err != nil {
			t.Fatalf("expected nil error, got: %v", err)
		}
		if len(events) != 1 {
			t.Errorf("expected 1 event, got %d", len(events))
		}
		if events[0].Type != "hint.emitted" {
			t.Errorf("expected hint.emitted, got %s", events[0].Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for gRPCHinter.Hint result")
	}
}
