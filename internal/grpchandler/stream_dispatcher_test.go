package grpchandler

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

func TestStreamDispatcher_IncompleteResult(t *testing.T) {
	d := NewStreamDispatcher(slog.Default())
	sendCh := make(chan *pb.DispatchMessage, 1)
	token := d.Register("incomplete-handler", sendCh)
	defer d.Unregister("incomplete-handler", token)

	ctx := context.Background()
	env := event.New("persona.completed", 1, []byte(`{}`)).
		WithCorrelation("wf-1")

	// Start Dispatch in background — it sends a request and blocks on result.
	resultCh := make(chan error, 1)
	var gotEvents int

	go func() {
		res, err := d.Dispatch(ctx, "incomplete-handler", env)
		if res != nil {
			gotEvents = len(res.Events)
		}
		resultCh <- err
	}()

	// Read the dispatch request to get the dispatch_id.
	var req *pb.DispatchMessage
	select {
	case req = <-sendCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for dispatch request")
	}
	dispatchID := req.GetDispatch().GetDispatchId()

	// Deliver a result with Incomplete=true and one event.
	d.DeliverResult("incomplete-handler", &pb.HandlerResult{
		DispatchId: dispatchID,
		Events: []*pb.EventEnvelope{
			{Type: "context.enrichment", Payload: []byte(`{"kind":"batch"}`)},
		},
		Incomplete: true,
	})

	// Dispatch should return handler.ErrIncomplete with result events.
	select {
	case err := <-resultCh:
		if !errors.Is(err, handler.ErrIncomplete) {
			t.Fatalf("expected handler.ErrIncomplete, got: %v", err)
		}
		if gotEvents != 1 {
			t.Errorf("expected 1 result event, got %d", gotEvents)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Dispatch return")
	}
}

func TestStreamDispatcher_NormalResult(t *testing.T) {
	d := NewStreamDispatcher(slog.Default())
	sendCh := make(chan *pb.DispatchMessage, 1)
	token := d.Register("normal-handler", sendCh)
	defer d.Unregister("normal-handler", token)

	ctx := context.Background()
	env := event.New("persona.completed", 1, []byte(`{}`)).
		WithCorrelation("wf-2")

	resultCh := make(chan error, 1)
	go func() {
		_, err := d.Dispatch(ctx, "normal-handler", env)
		resultCh <- err
	}()

	req := <-sendCh
	dispatchID := req.GetDispatch().GetDispatchId()

	// Deliver a normal result (Incomplete=false).
	d.DeliverResult("normal-handler", &pb.HandlerResult{
		DispatchId: dispatchID,
		Incomplete: false,
	})

	select {
	case err := <-resultCh:
		if err != nil {
			t.Fatalf("expected nil error for normal result, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}
