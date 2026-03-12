package grpchandler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
)

// TestStreamDispatcher_DispatchHint_HappyPath verifies that DispatchHint sends
// a hint_only=true DispatchRequest, delivers the result, and returns the
// result events without error.
func TestStreamDispatcher_DispatchHint_HappyPath(t *testing.T) {
	d := NewStreamDispatcher(testLogger())
	sendCh := make(chan *pb.DispatchMessage, 1)
	token := d.Register("hint-handler", sendCh)
	defer d.Unregister("hint-handler", token)

	ctx := context.Background()
	env := event.New("workflow.started", 1, []byte(`{}`)).WithCorrelation("wf-hint-1")

	resultCh := make(chan struct {
		result *engine.DispatchResult
		err    error
	}, 1)

	go func() {
		res, err := d.DispatchHint(ctx, "hint-handler", env)
		resultCh <- struct {
			result *engine.DispatchResult
			err    error
		}{res, err}
	}()

	// Read the dispatch request; verify hint_only=true.
	var req *pb.DispatchMessage
	select {
	case req = <-sendCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for hint dispatch request")
	}

	dispatch := req.GetDispatch()
	if dispatch == nil {
		t.Fatal("expected DispatchRequest")
	}
	if !dispatch.HintOnly {
		t.Error("expected HintOnly=true on hint dispatch")
	}

	dispatchID := dispatch.GetDispatchId()

	// Deliver a result with one event.
	d.DeliverResult("hint-handler", &pb.HandlerResult{
		DispatchId: dispatchID,
		Events: []*pb.EventEnvelope{
			{Type: "hint.emitted", Payload: []byte(`{"confidence":0.9}`)},
		},
	})

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("expected nil error, got: %v", got.err)
		}
		if got.result == nil {
			t.Fatal("expected non-nil result")
		}
		if len(got.result.Events) != 1 {
			t.Errorf("expected 1 result event, got %d", len(got.result.Events))
		}
		if got.result.Events[0].Type != "hint.emitted" {
			t.Errorf("expected event type hint.emitted, got %s", got.result.Events[0].Type)
		}
		if got.result.Handler != "hint-handler" {
			t.Errorf("expected Handler=hint-handler, got %s", got.result.Handler)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for DispatchHint result")
	}
}

// TestStreamDispatcher_DispatchHint_HandlerNotFound verifies that DispatchHint
// returns ErrHandlerNotFound when the handler is not registered.
func TestStreamDispatcher_DispatchHint_HandlerNotFound(t *testing.T) {
	d := NewStreamDispatcher(testLogger())
	ctx := context.Background()
	env := event.New("workflow.started", 1, []byte(`{}`)).WithCorrelation("wf-1")

	_, err := d.DispatchHint(ctx, "no-such-handler", env)
	if err == nil {
		t.Fatal("expected error for unknown handler")
	}
	if !errors.Is(err, engine.ErrHandlerNotFound) {
		t.Errorf("expected ErrHandlerNotFound, got: %v", err)
	}
}

// TestStreamDispatcher_DispatchHint_ContextCancellation verifies that
// DispatchHint returns an error when the context is cancelled mid-wait.
func TestStreamDispatcher_DispatchHint_ContextCancellation(t *testing.T) {
	d := NewStreamDispatcher(testLogger())
	// Use a larger channel so the send succeeds before we cancel.
	sendCh := make(chan *pb.DispatchMessage, 1)
	token := d.Register("cancelable-hint-handler", sendCh)
	defer d.Unregister("cancelable-hint-handler", token)

	ctx, cancel := context.WithCancel(context.Background())

	env := event.New("workflow.started", 1, []byte(`{}`)).WithCorrelation("wf-cancel-hint")

	resultCh := make(chan error, 1)
	go func() {
		_, err := d.DispatchHint(ctx, "cancelable-hint-handler", env)
		resultCh <- err
	}()

	// Wait for the dispatch to be sent, then cancel — never deliver result.
	select {
	case <-sendCh:
		// Got the dispatch request; now cancel without delivering.
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for hint dispatch send")
	}

	cancel()

	select {
	case err := <-resultCh:
		if err == nil {
			t.Fatal("expected error from cancelled context, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for DispatchHint cancellation")
	}
}

// TestStreamDispatcher_DispatchHint_HandlerError verifies that an error
// string in the HandlerResult is returned as an error from DispatchHint.
func TestStreamDispatcher_DispatchHint_HandlerError(t *testing.T) {
	d := NewStreamDispatcher(testLogger())
	sendCh := make(chan *pb.DispatchMessage, 1)
	token := d.Register("error-hint-handler", sendCh)
	defer d.Unregister("error-hint-handler", token)

	ctx := context.Background()
	env := event.New("workflow.started", 1, []byte(`{}`)).WithCorrelation("wf-hint-err")

	resultCh := make(chan error, 1)
	go func() {
		_, err := d.DispatchHint(ctx, "error-hint-handler", env)
		resultCh <- err
	}()

	req := <-sendCh
	dispatchID := req.GetDispatch().GetDispatchId()

	d.DeliverResult("error-hint-handler", &pb.HandlerResult{
		DispatchId: dispatchID,
		Error:      "hint evaluation failed",
	})

	select {
	case err := <-resultCh:
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if err.Error() == "" {
			t.Error("expected non-empty error message")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for hint error result")
	}
}

// TestStreamDispatcher_Displacement_E2E verifies the full displacement flow:
// 1. Client A registers as "handler-x".
// 2. Client B registers as "handler-x" — displaces A.
// 3. Client A's pending dispatches are cancelled (result channels closed).
// 4. Client B receives subsequent dispatches.
// 5. Client A's deferred Unregister is a no-op (stale token guard).
func TestStreamDispatcher_Displacement_E2E(t *testing.T) {
	d := NewStreamDispatcher(testLogger())

	// Register Client A.
	sendChA := make(chan *pb.DispatchMessage, 8)
	tokenA := d.Register("handler-x", sendChA)

	env := event.New("persona.completed", 1, []byte(`{}`)).WithCorrelation("wf-displace")

	// Kick off a Dispatch to Client A — it will block waiting for a result.
	dispatchErrA := make(chan error, 1)
	go func() {
		_, err := d.Dispatch(context.Background(), "handler-x", env)
		dispatchErrA <- err
	}()

	// Wait for the dispatch to be sent to A's channel.
	select {
	case <-sendChA:
		// A received the dispatch; result channel is now registered.
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Client A's dispatch")
	}

	// Register Client B — displaces A.
	sendChB := make(chan *pb.DispatchMessage, 8)
	tokenB := d.Register("handler-x", sendChB)
	defer d.Unregister("handler-x", tokenB)

	// Client A's pending dispatch should be unblocked (channel closed → error).
	select {
	case err := <-dispatchErrA:
		if err == nil {
			t.Error("expected error for displaced dispatch, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: Client A's pending dispatch was not cancelled on displacement")
	}

	// Client A's deferred Unregister should be a no-op (stale token).
	d.Unregister("handler-x", tokenA) // must not delete Client B's registration

	// Client B should now receive subsequent dispatches.
	resultCh := make(chan error, 1)
	go func() {
		_, err := d.Dispatch(context.Background(), "handler-x", env)
		resultCh <- err
	}()

	select {
	case req := <-sendChB:
		dispatch := req.GetDispatch()
		if dispatch == nil {
			t.Fatal("expected DispatchRequest for Client B")
		}
		// Deliver result to unblock.
		d.DeliverResult("handler-x", &pb.HandlerResult{
			DispatchId: dispatch.DispatchId,
		})
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: Client B did not receive dispatch after displacement")
	}

	select {
	case err := <-resultCh:
		if err != nil {
			t.Errorf("expected nil error for Client B dispatch, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Client B dispatch result")
	}
}

// TestStreamDispatcher_ConcurrentDispatch verifies that multiple concurrent
// Dispatch calls each get their own result without cross-contamination.
// The key invariant: each dispatch_id maps to exactly one result, and the
// result delivered for a given dispatch_id is returned only to its caller.
func TestStreamDispatcher_ConcurrentDispatch(t *testing.T) {
	d := NewStreamDispatcher(testLogger())
	sendCh := make(chan *pb.DispatchMessage, 8)
	token := d.Register("concurrent-dispatch-handler", sendCh)
	defer d.Unregister("concurrent-dispatch-handler", token)

	const numDispatches = 4

	// resultByCorrID maps correlationID → received event type.
	resultByCorrID := sync.Map{}
	errByCorrID := sync.Map{}

	ctx := context.Background()

	// Launch concurrent dispatches, each with a unique correlationID.
	var launchWg sync.WaitGroup
	launchWg.Add(numDispatches)
	for i := range numDispatches {
		idx := i
		corrID := "wf-concurrent-" + string(rune('a'+idx))
		go func() {
			env := event.New(event.Type("persona.completed"), 1, []byte(`{}`)).
				WithCorrelation(corrID)
			res, err := d.Dispatch(ctx, "concurrent-dispatch-handler", env)
			if err != nil {
				errByCorrID.Store(corrID, err)
			} else if res != nil && len(res.Events) > 0 {
				resultByCorrID.Store(corrID, string(res.Events[0].Type))
			}
			launchWg.Done()
		}()
	}

	// Collect dispatch requests and build corrID → dispatchID mapping.
	corrToDispatch := make(map[string]string, numDispatches)
	for i := range numDispatches {
		select {
		case req := <-sendCh:
			dispatch := req.GetDispatch()
			if dispatch == nil {
				t.Fatal("expected DispatchRequest")
			}
			corrToDispatch[dispatch.Event.CorrelationId] = dispatch.DispatchId
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for dispatch %d", i)
		}
	}

	// Deliver each result with a unique event type keyed to the correlation.
	// The event type encodes the corrID so we can verify no cross-contamination.
	for idx := range numDispatches {
		corrID := "wf-concurrent-" + string(rune('a'+idx))
		dispatchID, ok := corrToDispatch[corrID]
		if !ok {
			t.Fatalf("no dispatch found for correlation %s", corrID)
		}
		d.DeliverResult("concurrent-dispatch-handler", &pb.HandlerResult{
			DispatchId: dispatchID,
			Events: []*pb.EventEnvelope{
				{Type: "result-for-" + corrID},
			},
		})
	}

	// Wait for all goroutines to receive their results.
	waitDone := make(chan struct{})
	go func() {
		launchWg.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: not all dispatches returned")
	}

	// Verify each corrID got exactly its own result.
	for idx := range numDispatches {
		corrID := "wf-concurrent-" + string(rune('a'+idx))
		want := "result-for-" + corrID

		if v, ok := errByCorrID.Load(corrID); ok {
			t.Errorf("corrID %s: unexpected error: %v", corrID, v)
			continue
		}
		v, ok := resultByCorrID.Load(corrID)
		if !ok {
			t.Errorf("corrID %s: no result received", corrID)
			continue
		}
		got := v.(string)
		if got != want {
			t.Errorf("corrID %s: expected event type %q, got %q (cross-contamination!)", corrID, want, got)
		}
	}
}
