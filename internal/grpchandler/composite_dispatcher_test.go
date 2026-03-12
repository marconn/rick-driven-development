package grpchandler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
)

// mockLocalDispatcher is a minimal Dispatcher for testing CompositeDispatcher
// routing logic without touching the real handler.Registry.
type mockLocalDispatcher struct {
	result *engine.DispatchResult
	err    error
}

func (m *mockLocalDispatcher) Dispatch(_ context.Context, name string, _ event.Envelope) (*engine.DispatchResult, error) {
	if m.result != nil {
		res := *m.result
		res.Handler = name
		return &res, m.err
	}
	return m.result, m.err
}

var errNonNotFound = errors.New("local handler: transient DB error")

// TestCompositeDispatcher_LocalSucceeds_StreamNotCalled verifies that when
// the local dispatcher succeeds, the stream dispatcher is NEVER called.
func TestCompositeDispatcher_LocalSucceeds_StreamNotCalled(t *testing.T) {
	localD := &mockLocalDispatcher{
		result: &engine.DispatchResult{},
		err:    nil,
	}

	// Use a StreamDispatcher with no registered handlers. If it were called
	// for "local-only-handler", Dispatch would return ErrHandlerNotFound.
	// But the CompositeDispatcher must not call it at all.
	streamD := NewStreamDispatcher(testLogger())

	composite := NewCompositeDispatcher(localD, streamD)

	ctx := context.Background()
	env := event.New("persona.completed", 1, []byte(`{}`)).WithCorrelation("wf-local")

	result, err := composite.Dispatch(ctx, "local-only-handler", env)
	if err != nil {
		t.Fatalf("expected success from local dispatcher, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Handler != "local-only-handler" {
		t.Errorf("expected handler=local-only-handler, got %q", result.Handler)
	}
}

// TestCompositeDispatcher_LocalNotFound_FallsBackToStream verifies that when
// the local dispatcher returns ErrHandlerNotFound, the call falls through to
// the stream dispatcher.
func TestCompositeDispatcher_LocalNotFound_FallsBackToStream(t *testing.T) {
	localD := &mockLocalDispatcher{
		result: nil,
		err:    engine.ErrHandlerNotFound,
	}

	streamD := NewStreamDispatcher(testLogger())
	sendCh := make(chan *pb.DispatchMessage, 2)
	token := streamD.Register("stream-handler", sendCh)
	defer streamD.Unregister("stream-handler", token)

	composite := NewCompositeDispatcher(localD, streamD)

	ctx := context.Background()
	env := event.New("persona.completed", 1, []byte(`{}`)).WithCorrelation("wf-fallback")

	resultCh := make(chan struct {
		res *engine.DispatchResult
		err error
	}, 1)
	go func() {
		res, err := composite.Dispatch(ctx, "stream-handler", env)
		resultCh <- struct {
			res *engine.DispatchResult
			err error
		}{res, err}
	}()

	// Wait for the stream dispatch request to arrive.
	var req *pb.DispatchMessage
	select {
	case req = <-sendCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: stream dispatcher was not called after ErrHandlerNotFound from local")
	}

	dispatch := req.GetDispatch()
	if dispatch == nil {
		t.Fatal("expected DispatchRequest")
	}

	// Deliver a successful result.
	streamD.DeliverResult("stream-handler", &pb.HandlerResult{
		DispatchId: dispatch.DispatchId,
		Events: []*pb.EventEnvelope{
			{Type: "context.enrichment"},
		},
	})

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("expected nil error from stream fallback, got: %v", got.err)
		}
		if got.res == nil {
			t.Fatal("expected non-nil result from stream")
		}
		if len(got.res.Events) != 1 {
			t.Errorf("expected 1 event, got %d", len(got.res.Events))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for composite dispatch result")
	}
}

// TestCompositeDispatcher_LocalNonNotFoundError_NoFallback verifies that when
// the local dispatcher returns an error that is NOT ErrHandlerNotFound, the
// stream dispatcher is NOT called — the error is returned directly.
func TestCompositeDispatcher_LocalNonNotFoundError_NoFallback(t *testing.T) {
	localD := &mockLocalDispatcher{
		result: nil,
		err:    errNonNotFound, // a non-ErrHandlerNotFound error
	}

	// If the stream were called, it would block forever (no registered handler).
	streamD := NewStreamDispatcher(testLogger())

	composite := NewCompositeDispatcher(localD, streamD)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	env := event.New("persona.completed", 1, []byte(`{}`)).WithCorrelation("wf-err")

	_, err := composite.Dispatch(ctx, "some-handler", env)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, errNonNotFound) {
		t.Errorf("expected errNonNotFound, got: %v", err)
	}
}
