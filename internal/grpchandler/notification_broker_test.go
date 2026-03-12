package grpchandler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
	"github.com/marconn/rick-event-driven-development/internal/projection"
)

func newBrokerTestEnv(t *testing.T) (*NotificationBroker, *eventbus.ChannelBus, *projection.WorkflowStatusProjection, *projection.TokenUsageProjection, *projection.PhaseTimelineProjection, *projection.VerdictProjection) {
	t.Helper()

	bus := eventbus.NewChannelBus()
	workflows := projection.NewWorkflowStatusProjection()
	tokens := projection.NewTokenUsageProjection()
	timelines := projection.NewPhaseTimelineProjection()
	verdicts := projection.NewVerdictProjection()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	broker := NewNotificationBroker(bus, workflows, tokens, timelines, verdicts, logger)
	broker.Start()

	t.Cleanup(func() {
		broker.Stop()
		_ = bus.Close()
	})

	return broker, bus, workflows, tokens, timelines, verdicts
}

// seedWorkflowProjections feeds events into the projections so they
// reflect a running or completed workflow.
func seedWorkflowProjections(
	t *testing.T,
	workflows *projection.WorkflowStatusProjection,
	tokens *projection.TokenUsageProjection,
	timelines *projection.PhaseTimelineProjection,
	correlationID string,
	terminal event.Envelope,
) {
	t.Helper()
	ctx := context.Background()

	// Seed WorkflowRequested + WorkflowStarted.
	requested := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt:     "test",
		WorkflowID: "test-wf",
	})).WithAggregate(correlationID, 1).WithCorrelation(correlationID)
	_ = workflows.Handle(ctx, requested)

	started := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "test-wf",
		Phases:     []string{"researcher"},
	})).WithAggregate(correlationID, 2).WithCorrelation(correlationID)
	_ = workflows.Handle(ctx, started)

	// Seed AIResponseReceived for token tracking.
	aiResp := event.New(event.AIResponseReceived, 1, event.MustMarshal(event.AIResponsePayload{
		Phase:      "researcher",
		Backend:    "claude",
		TokensUsed: 500,
	})).WithAggregate(correlationID+":persona:researcher", 1).WithCorrelation(correlationID)
	_ = tokens.Handle(ctx, aiResp)

	// Seed PersonaCompleted for timeline.
	pc := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona:    "researcher",
		DurationMS: 1200,
	})).WithAggregate(correlationID+":persona:researcher", 2).WithCorrelation(correlationID)
	_ = timelines.Handle(ctx, pc)

	// Seed terminal event.
	_ = workflows.Handle(ctx, terminal)
}

func TestBroker_LiveNotification(t *testing.T) {
	broker, bus, workflows, _, _, _ := newBrokerTestEnv(t)

	// Seed running workflow in projection.
	ctx := context.Background()
	requested := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "test-wf",
	})).WithAggregate("wf-1", 1).WithCorrelation("wf-1")
	_ = workflows.Handle(ctx, requested)

	started := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "test-wf", Phases: []string{"researcher"},
	})).WithAggregate("wf-1", 2).WithCorrelation("wf-1")
	_ = workflows.Handle(ctx, started)

	// Watch for wf-1.
	sendCh := make(chan *pb.DispatchMessage, 16)
	broker.Watch("test-handler", []string{"wf-1"}, sendCh)

	// Publish WorkflowCompleted on bus.
	completed := event.New(event.WorkflowCompleted, 1, event.MustMarshal(event.WorkflowCompletedPayload{
		Result: "all phases done",
	})).WithAggregate("wf-1", 3).WithCorrelation("wf-1")
	_ = workflows.Handle(ctx, completed)
	if err := bus.Publish(ctx, completed); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case msg := <-sendCh:
		notif := msg.GetNotification()
		if notif == nil {
			t.Fatal("expected WorkflowNotification")
		}
		if notif.CorrelationId != "wf-1" {
			t.Errorf("expected correlation wf-1, got %s", notif.CorrelationId)
		}
		if notif.Status != "completed" {
			t.Errorf("expected status completed, got %s", notif.Status)
		}
		if notif.Result != "all phases done" {
			t.Errorf("expected result 'all phases done', got %s", notif.Result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for notification")
	}
}

func TestBroker_CatchUpOnWatch(t *testing.T) {
	broker, _, workflows, tokens, timelines, _ := newBrokerTestEnv(t)

	// Complete workflow BEFORE watching.
	completed := event.New(event.WorkflowCompleted, 1, event.MustMarshal(event.WorkflowCompletedPayload{
		Result: "pre-completed",
	})).WithAggregate("wf-catchup", 3).WithCorrelation("wf-catchup")
	seedWorkflowProjections(t, workflows, tokens, timelines, "wf-catchup", completed)

	// Now watch — should get immediate catch-up.
	sendCh := make(chan *pb.DispatchMessage, 16)
	broker.Watch("catcher", []string{"wf-catchup"}, sendCh)

	select {
	case msg := <-sendCh:
		notif := msg.GetNotification()
		if notif == nil {
			t.Fatal("expected catch-up WorkflowNotification")
		}
		if notif.Status != "completed" {
			t.Errorf("expected status completed, got %s", notif.Status)
		}
		if notif.TotalTokens != 500 {
			t.Errorf("expected 500 tokens, got %d", notif.TotalTokens)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for catch-up notification")
	}
}

func TestBroker_WatchAll(t *testing.T) {
	broker, bus, workflows, _, _, _ := newBrokerTestEnv(t)

	ctx := context.Background()

	// Watch all (empty correlation list).
	sendCh := make(chan *pb.DispatchMessage, 16)
	broker.Watch("wildcard-handler", nil, sendCh)

	// Complete two different workflows.
	for _, wfID := range []string{"wf-a", "wf-b"} {
		req := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "test", WorkflowID: "test-wf",
		})).WithAggregate(wfID, 1).WithCorrelation(wfID)
		_ = workflows.Handle(ctx, req)

		started := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
			WorkflowID: "test-wf",
		})).WithAggregate(wfID, 2).WithCorrelation(wfID)
		_ = workflows.Handle(ctx, started)

		completed := event.New(event.WorkflowCompleted, 1, event.MustMarshal(event.WorkflowCompletedPayload{
			Result: "done-" + wfID,
		})).WithAggregate(wfID, 3).WithCorrelation(wfID)
		_ = workflows.Handle(ctx, completed)
		if err := bus.Publish(ctx, completed); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	// Should receive notifications for both.
	received := make(map[string]bool)
	for i := 0; i < 2; i++ {
		select {
		case msg := <-sendCh:
			notif := msg.GetNotification()
			if notif == nil {
				t.Fatal("expected notification")
			}
			received[notif.CorrelationId] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for notification %d", i+1)
		}
	}
	if !received["wf-a"] || !received["wf-b"] {
		t.Errorf("expected both wf-a and wf-b, got %v", received)
	}
}

func TestBroker_Unwatch(t *testing.T) {
	broker, bus, workflows, _, _, _ := newBrokerTestEnv(t)

	ctx := context.Background()

	sendCh := make(chan *pb.DispatchMessage, 16)
	broker.Watch("unwatcher", []string{"wf-unwatch"}, sendCh)
	broker.Unwatch("unwatcher", []string{"wf-unwatch"})

	// Seed + publish completion.
	req := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "test-wf",
	})).WithAggregate("wf-unwatch", 1).WithCorrelation("wf-unwatch")
	_ = workflows.Handle(ctx, req)

	completed := event.New(event.WorkflowCompleted, 1, event.MustMarshal(event.WorkflowCompletedPayload{
		Result: "done",
	})).WithAggregate("wf-unwatch", 3).WithCorrelation("wf-unwatch")
	_ = workflows.Handle(ctx, completed)
	if err := bus.Publish(ctx, completed); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Give bus time to deliver.
	time.Sleep(100 * time.Millisecond)

	select {
	case msg := <-sendCh:
		t.Fatalf("expected no notification after unwatch, got %v", msg)
	default:
		// Expected: nothing.
	}
}

func TestBroker_UnwatchAll(t *testing.T) {
	broker, bus, workflows, _, _, _ := newBrokerTestEnv(t)

	ctx := context.Background()

	sendCh := make(chan *pb.DispatchMessage, 16)
	broker.Watch("disconnector", []string{"wf-d1", "wf-d2"}, sendCh)
	broker.Watch("disconnector", nil, sendCh) // also wildcard
	broker.UnwatchAll("disconnector")

	// Complete a workflow.
	req := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "test-wf",
	})).WithAggregate("wf-d1", 1).WithCorrelation("wf-d1")
	_ = workflows.Handle(ctx, req)

	completed := event.New(event.WorkflowCompleted, 1, event.MustMarshal(event.WorkflowCompletedPayload{
		Result: "done",
	})).WithAggregate("wf-d1", 3).WithCorrelation("wf-d1")
	_ = workflows.Handle(ctx, completed)
	if err := bus.Publish(ctx, completed); err != nil {
		t.Fatalf("publish: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	select {
	case msg := <-sendCh:
		t.Fatalf("expected no notification after UnwatchAll, got %v", msg)
	default:
		// Expected: nothing.
	}
}

func TestBroker_NotificationSummaryData(t *testing.T) {
	broker, bus, workflows, tokens, timelines, _ := newBrokerTestEnv(t)

	ctx := context.Background()

	// Seed full workflow with projection data.
	completed := event.New(event.WorkflowCompleted, 1, event.MustMarshal(event.WorkflowCompletedPayload{
		Result: "full summary",
	})).WithAggregate("wf-summary", 3).WithCorrelation("wf-summary")
	seedWorkflowProjections(t, workflows, tokens, timelines, "wf-summary", completed)

	sendCh := make(chan *pb.DispatchMessage, 16)
	broker.Watch("summarizer", []string{"wf-summary"}, sendCh)

	if err := bus.Publish(ctx, completed); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case msg := <-sendCh:
		notif := msg.GetNotification()
		if notif == nil {
			t.Fatal("expected notification")
		}
		if notif.Status != "completed" {
			t.Errorf("expected completed, got %s", notif.Status)
		}
		if notif.TotalTokens != 500 {
			t.Errorf("expected 500 total tokens, got %d", notif.TotalTokens)
		}
		if notif.TokensByPhase["researcher"] != 500 {
			t.Errorf("expected 500 tokens for researcher, got %d", notif.TokensByPhase["researcher"])
		}
		if notif.TokensByBackend["claude"] != 500 {
			t.Errorf("expected 500 tokens for claude, got %d", notif.TokensByBackend["claude"])
		}
		if len(notif.Phases) == 0 {
			t.Error("expected phase timeline data")
		} else {
			if notif.Phases[0].Phase != "researcher" {
				t.Errorf("expected phase researcher, got %s", notif.Phases[0].Phase)
			}
		}
		if notif.StartedAtMs == 0 {
			t.Error("expected non-zero started_at_ms")
		}
		if notif.DurationMs < 0 {
			t.Error("expected non-negative duration")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for notification")
	}
}

func TestBroker_FailedWorkflowNotification(t *testing.T) {
	broker, bus, workflows, _, _, _ := newBrokerTestEnv(t)

	ctx := context.Background()

	// Seed running workflow.
	req := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "test-wf",
	})).WithAggregate("wf-fail", 1).WithCorrelation("wf-fail")
	_ = workflows.Handle(ctx, req)

	started := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "test-wf",
	})).WithAggregate("wf-fail", 2).WithCorrelation("wf-fail")
	_ = workflows.Handle(ctx, started)

	sendCh := make(chan *pb.DispatchMessage, 16)
	broker.Watch("fail-watcher", []string{"wf-fail"}, sendCh)

	failed := event.New(event.WorkflowFailed, 1, event.MustMarshal(event.WorkflowFailedPayload{
		Reason: "developer crashed",
		Phase:  "developer",
	})).WithAggregate("wf-fail", 3).WithCorrelation("wf-fail")
	_ = workflows.Handle(ctx, failed)

	if err := bus.Publish(ctx, failed); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case msg := <-sendCh:
		notif := msg.GetNotification()
		if notif == nil {
			t.Fatal("expected notification")
		}
		if notif.Status != "failed" {
			t.Errorf("expected status failed, got %s", notif.Status)
		}
		if notif.Result != "developer crashed" {
			t.Errorf("expected reason 'developer crashed', got %s", notif.Result)
		}
		if notif.FailedPhase != "developer" {
			t.Errorf("expected failed_phase 'developer', got %s", notif.FailedPhase)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for failed notification")
	}
}

func TestBroker_NotificationIncludesVerdicts(t *testing.T) {
	broker, bus, workflows, _, _, verdicts := newBrokerTestEnv(t)

	ctx := context.Background()

	// Seed running workflow.
	requested := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "test-wf",
	})).WithAggregate("wf-v", 1).WithCorrelation("wf-v")
	_ = workflows.Handle(ctx, requested)

	started := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "test-wf", Phases: []string{"developer", "reviewer", "qa"},
	})).WithAggregate("wf-v", 2).WithCorrelation("wf-v")
	_ = workflows.Handle(ctx, started)

	// Seed verdicts into the projection.
	reviewVerdict := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase:       "develop",
		SourcePhase: "reviewer",
		Outcome:     event.VerdictFail,
		Summary:     "Missing error handling",
		Issues: []event.Issue{
			{Severity: "major", Category: "correctness", Description: "unchecked error", File: "main.go", Line: 42},
		},
	})).WithAggregate("wf-v:persona:reviewer", 1).WithCorrelation("wf-v")
	_ = verdicts.Handle(ctx, reviewVerdict)

	qaVerdict := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase:       "develop",
		SourcePhase: "qa",
		Outcome:     event.VerdictPass,
		Summary:     "All tests pass",
	})).WithAggregate("wf-v:persona:qa", 1).WithCorrelation("wf-v")
	_ = verdicts.Handle(ctx, qaVerdict)

	// Watch and complete.
	sendCh := make(chan *pb.DispatchMessage, 16)
	broker.Watch("verdict-watcher", []string{"wf-v"}, sendCh)

	completed := event.New(event.WorkflowCompleted, 1, event.MustMarshal(event.WorkflowCompletedPayload{
		Result: "done with verdicts",
	})).WithAggregate("wf-v", 3).WithCorrelation("wf-v")
	_ = workflows.Handle(ctx, completed)
	if err := bus.Publish(ctx, completed); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case msg := <-sendCh:
		notif := msg.GetNotification()
		if notif == nil {
			t.Fatal("expected WorkflowNotification")
		}
		if len(notif.Verdicts) != 2 {
			t.Fatalf("expected 2 verdicts, got %d", len(notif.Verdicts))
		}

		// Verify reviewer verdict.
		rv := notif.Verdicts[0]
		if rv.SourcePhase != "reviewer" {
			t.Errorf("expected source_phase reviewer, got %s", rv.SourcePhase)
		}
		if rv.Outcome != "fail" {
			t.Errorf("expected outcome fail, got %s", rv.Outcome)
		}
		if rv.Summary != "Missing error handling" {
			t.Errorf("expected summary 'Missing error handling', got %s", rv.Summary)
		}
		if len(rv.Issues) != 1 {
			t.Fatalf("expected 1 issue, got %d", len(rv.Issues))
		}
		if rv.Issues[0].File != "main.go" || rv.Issues[0].Line != 42 {
			t.Errorf("expected issue at main.go:42, got %s:%d", rv.Issues[0].File, rv.Issues[0].Line)
		}
		if rv.Issues[0].Severity != "major" {
			t.Errorf("expected severity major, got %s", rv.Issues[0].Severity)
		}

		// Verify QA verdict.
		qv := notif.Verdicts[1]
		if qv.SourcePhase != "qa" {
			t.Errorf("expected source_phase qa, got %s", qv.SourcePhase)
		}
		if qv.Outcome != "pass" {
			t.Errorf("expected outcome pass, got %s", qv.Outcome)
		}
		if len(qv.Issues) != 0 {
			t.Errorf("expected no issues for QA pass, got %d", len(qv.Issues))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for notification")
	}
}

func TestBroker_NotificationNoVerdicts(t *testing.T) {
	broker, bus, workflows, _, _, _ := newBrokerTestEnv(t)

	ctx := context.Background()

	// Seed running workflow with no verdicts.
	requested := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "test-wf",
	})).WithAggregate("wf-nv", 1).WithCorrelation("wf-nv")
	_ = workflows.Handle(ctx, requested)

	started := event.New(event.WorkflowStarted, 1, event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: "test-wf",
	})).WithAggregate("wf-nv", 2).WithCorrelation("wf-nv")
	_ = workflows.Handle(ctx, started)

	sendCh := make(chan *pb.DispatchMessage, 16)
	broker.Watch("no-verdict-watcher", []string{"wf-nv"}, sendCh)

	completed := event.New(event.WorkflowCompleted, 1, event.MustMarshal(event.WorkflowCompletedPayload{
		Result: "done",
	})).WithAggregate("wf-nv", 3).WithCorrelation("wf-nv")
	_ = workflows.Handle(ctx, completed)
	if err := bus.Publish(ctx, completed); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case msg := <-sendCh:
		notif := msg.GetNotification()
		if notif == nil {
			t.Fatal("expected WorkflowNotification")
		}
		if len(notif.Verdicts) != 0 {
			t.Errorf("expected 0 verdicts, got %d", len(notif.Verdicts))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for notification")
	}
}

func TestBroker_CatchUpIncludesVerdicts(t *testing.T) {
	broker, _, workflows, tokens, timelines, verdicts := newBrokerTestEnv(t)

	ctx := context.Background()

	// Complete workflow with verdicts BEFORE watching.
	completed := event.New(event.WorkflowCompleted, 1, event.MustMarshal(event.WorkflowCompletedPayload{
		Result: "pre-completed",
	})).WithAggregate("wf-catchup-v", 3).WithCorrelation("wf-catchup-v")
	seedWorkflowProjections(t, workflows, tokens, timelines, "wf-catchup-v", completed)

	// Seed a verdict.
	v := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Phase:       "develop",
		SourcePhase: "reviewer",
		Outcome:     event.VerdictPass,
		Summary:     "All good",
	})).WithAggregate("wf-catchup-v:persona:reviewer", 1).WithCorrelation("wf-catchup-v")
	_ = verdicts.Handle(ctx, v)

	// Watch — should get catch-up with verdict.
	sendCh := make(chan *pb.DispatchMessage, 16)
	broker.Watch("catchup-verdict", []string{"wf-catchup-v"}, sendCh)

	select {
	case msg := <-sendCh:
		notif := msg.GetNotification()
		if notif == nil {
			t.Fatal("expected catch-up WorkflowNotification")
		}
		if len(notif.Verdicts) != 1 {
			t.Fatalf("expected 1 verdict in catch-up, got %d", len(notif.Verdicts))
		}
		if notif.Verdicts[0].SourcePhase != "reviewer" {
			t.Errorf("expected reviewer, got %s", notif.Verdicts[0].SourcePhase)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for catch-up notification")
	}
}

// Verify JSON marshaling of WorkflowCancelledPayload is correct.
func TestBroker_CancelledWorkflowNotification(t *testing.T) {
	broker, bus, workflows, _, _, _ := newBrokerTestEnv(t)
	ctx := context.Background()

	req := event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
		Prompt: "test", WorkflowID: "test-wf",
	})).WithAggregate("wf-cancel", 1).WithCorrelation("wf-cancel")
	_ = workflows.Handle(ctx, req)

	sendCh := make(chan *pb.DispatchMessage, 16)
	broker.Watch("cancel-watcher", []string{"wf-cancel"}, sendCh)

	cancelled := event.New(event.WorkflowCancelled, 1, json.RawMessage(`{"reason":"user requested"}`)).
		WithAggregate("wf-cancel", 2).WithCorrelation("wf-cancel")
	_ = workflows.Handle(ctx, cancelled)
	if err := bus.Publish(ctx, cancelled); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case msg := <-sendCh:
		notif := msg.GetNotification()
		if notif == nil {
			t.Fatal("expected notification")
		}
		if notif.Status != "cancelled" {
			t.Errorf("expected status cancelled, got %s", notif.Status)
		}
		if notif.Result != "user requested" {
			t.Errorf("expected reason 'user requested', got %s", notif.Result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for cancelled notification")
	}
}
