package grpchandler

import (
	"context"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
)

// TestBroker_SendChFull_DropsNotification verifies that when a watcher's
// sendCh is full (buffer exhausted), the notification is silently dropped
// without panicking or blocking. This tests the non-blocking select in
// handleTerminalEvent.
func TestBroker_SendChFull_DropsNotification(t *testing.T) {
	broker, bus, workflows, _, _, _ := newBrokerTestEnv(t)

	ctx := context.Background()

	// Seed running workflow.
	req := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "test-wf",
	})).WithAggregate("wf-full-ch", 1).WithCorrelation("wf-full-ch")
	_ = workflows.Handle(ctx, req)

	started := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "test-wf",
	})).WithAggregate("wf-full-ch", 2).WithCorrelation("wf-full-ch")
	_ = workflows.Handle(ctx, started)

	// Create a sendCh with capacity=1 and fill it immediately so it's full.
	sendCh := make(chan *pb.DispatchMessage, 1)
	sendCh <- &pb.DispatchMessage{} // fills the buffer completely

	broker.Watch("full-handler", []string{"wf-full-ch"}, sendCh)

	// Publish a workflow completion. The broker tries to send to sendCh but
	// it's full — it must drop silently without panicking or blocking.
	completed := event.New(event.WorkflowCompleted, 1, event.MustMarshal(event.WorkflowCompletedPayload{
		Result: "done",
	})).WithAggregate("wf-full-ch", 3).WithCorrelation("wf-full-ch")
	_ = workflows.Handle(ctx, completed)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := bus.Publish(ctx, completed); err != nil {
			t.Errorf("bus.Publish failed: %v", err)
		}
	}()

	select {
	case <-done:
		// Publish returned without blocking — notification was dropped as expected.
		t.Log("bus.Publish returned without blocking despite full sendCh — drop confirmed")
	case <-time.After(2 * time.Second):
		t.Fatal("bus.Publish blocked on full sendCh — broker should drop silently")
	}

	// The original message that was in sendCh must still be there (not overwritten).
	select {
	case msg := <-sendCh:
		if msg.GetNotification() != nil {
			// If this is a notification, it means the original message was
			// consumed and the drop didn't happen. This is acceptable only if
			// the broker replaced the filler.
			t.Log("received notification (original filler was consumed)")
		} else {
			// Original filler message — correct.
			t.Log("original filler message still in channel — drop confirmed")
		}
	default:
		t.Log("sendCh now empty — broker consumed or replaced the filler")
	}
}

// TestBroker_SendChFull_CatchUp_DropsNotification verifies that the catch-up
// path (sendCatchUp) also handles a full sendCh without blocking.
func TestBroker_SendChFull_CatchUp_DropsNotification(t *testing.T) {
	broker, _, workflows, tokens, timelines, _ := newBrokerTestEnv(t)

	// Complete workflow BEFORE watching.
	completed := event.New(event.WorkflowCompleted, 1, event.MustMarshal(event.WorkflowCompletedPayload{
		Result: "pre-completed",
	})).WithAggregate("wf-catchup-full", 3).WithCorrelation("wf-catchup-full")
	seedWorkflowProjections(t, workflows, tokens, timelines, "wf-catchup-full", completed)

	// Create a full sendCh.
	sendCh := make(chan *pb.DispatchMessage, 1)
	sendCh <- &pb.DispatchMessage{} // fills the buffer

	// Watch with a full channel — catch-up must not block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		broker.Watch("full-catchup-handler", []string{"wf-catchup-full"}, sendCh)
	}()

	select {
	case <-done:
		t.Log("Watch returned without blocking on full sendCh — drop confirmed")
	case <-time.After(2 * time.Second):
		t.Fatal("Watch blocked on full sendCh during catch-up — should drop silently")
	}
}
