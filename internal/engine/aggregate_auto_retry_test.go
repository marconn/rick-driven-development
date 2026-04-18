package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// TestAggregate_AutoRetry_OnIdleTimeout covers the transient-failure
// recovery path added for the developer-zero-iteration bug: the first
// PersonaFailed with FailureKind=idle_timeout must produce WorkflowRetried
// (Automatic=true) instead of WorkflowFailed, preserving upstream
// completions and re-dispatching only the failed persona + its
// DAG-downstream.
func TestAggregate_AutoRetry_OnIdleTimeout(t *testing.T) {
	agg := NewWorkflowAggregate("wf-auto-retry")
	agg.Status = StatusRunning
	def := WorkspaceDevWorkflowDef()
	agg.WorkflowDef = &def

	failEnv := event.Envelope{
		Type:          event.PersonaFailed,
		AggregateID:   "wf-auto-retry",
		CorrelationID: "corr-auto-retry",
		Payload: event.MustMarshal(event.PersonaFailedPayload{
			Persona:     "developer",
			Error:       "handler developer: backend: claude: idle timeout exceeded (stall=2m0s) (after 2m0s)",
			FailureKind: event.FailureKindIdleTimeout,
			Stderr:      "subprocess went silent",
		}),
	}

	events, err := agg.Decide(failEnv)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Type != event.WorkflowRetried {
		t.Fatalf("want WorkflowRetried (auto-retry on transient), got %s", events[0].Type)
	}

	var p event.WorkflowRetriedPayload
	if err := json.Unmarshal(events[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal retry payload: %v", err)
	}
	if !p.Automatic {
		t.Error("Automatic = false; want true so Apply counts this against the cap")
	}
	if p.FromPhase != "developer" {
		t.Errorf("FromPhase = %q; want developer", p.FromPhase)
	}
	// DAG-downstream must include the failed persona itself plus every
	// dependent so the retry re-runs the full affected slice.
	if !slices.Contains(p.InvalidatedPersonas, "developer") {
		t.Errorf("InvalidatedPersonas %v missing developer itself", p.InvalidatedPersonas)
	}
	if !strings.Contains(p.Reason, "idle_timeout") {
		t.Errorf("Reason = %q; want idle_timeout in text for operator trace", p.Reason)
	}
}

// TestAggregate_AutoRetry_CappedAtOne is the load-bearing guard against
// infinite retry loops on deterministic crashes. Once a persona has one
// automatic retry recorded, a second identical failure must fall through
// to WorkflowFailed so the operator sees the real failure.
func TestAggregate_AutoRetry_CappedAtOne(t *testing.T) {
	agg := NewWorkflowAggregate("wf-cap")
	agg.Status = StatusRunning
	def := WorkspaceDevWorkflowDef()
	agg.WorkflowDef = &def
	agg.AutoRetries["developer"] = 1 // simulate prior auto-retry already spent

	failEnv := event.Envelope{
		Type:          event.PersonaFailed,
		AggregateID:   "wf-cap",
		CorrelationID: "corr-cap",
		Payload: event.MustMarshal(event.PersonaFailedPayload{
			Persona:     "developer",
			Error:       "second idle timeout",
			FailureKind: event.FailureKindIdleTimeout,
		}),
	}

	events, err := agg.Decide(failEnv)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].Type != event.WorkflowFailed {
		t.Fatalf("want WorkflowFailed (cap exceeded), got %s — this is the infinite-loop regression guard",
			events[0].Type)
	}
}

// TestAggregate_AutoRetry_SkipsDeterministicFailureKinds enumerates every
// non-transient FailureKind and confirms we don't burn an auto-retry on
// them. HandlerError, BackendError, and Cancelled all indicate a retry
// won't help; WallTimeout is excluded pragmatically (20min timeouts are
// unlikely to succeed in another 20min).
func TestAggregate_AutoRetry_SkipsDeterministicFailureKinds(t *testing.T) {
	cases := []event.FailureKind{
		event.FailureKindHandlerError,
		event.FailureKindBackendError,
		event.FailureKindCancelled,
		event.FailureKindWallTimeout,
		event.FailureKindUnspecified,
	}
	for _, kind := range cases {
		t.Run(string(kind), func(t *testing.T) {
			agg := NewWorkflowAggregate("wf-skip")
			agg.Status = StatusRunning
			def := WorkspaceDevWorkflowDef()
			agg.WorkflowDef = &def

			events, err := agg.Decide(event.Envelope{
				Type:          event.PersonaFailed,
				AggregateID:   "wf-skip",
				CorrelationID: "corr-skip",
				Payload: event.MustMarshal(event.PersonaFailedPayload{
					Persona:     "developer",
					Error:       "not a transient shape",
					FailureKind: kind,
				}),
			})
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if len(events) != 1 || events[0].Type != event.WorkflowFailed {
				t.Fatalf("kind=%q: want WorkflowFailed, got %+v", kind, events)
			}
		})
	}
}

// TestAggregate_Apply_CountsOnlyAutomaticRetries guards against operator
// retries silently consuming the auto-retry budget. A user who manually
// calls rick_retry_workflow N times must still get one auto-retry on any
// subsequent transient failure.
func TestAggregate_Apply_CountsOnlyAutomaticRetries(t *testing.T) {
	agg := NewWorkflowAggregate("wf-apply")

	// Operator-initiated retry: Automatic=false.
	manualRetry := event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
		FromPhase:           "developer",
		InvalidatedPersonas: []string{"developer"},
		Reason:              "operator retry via mcp",
		Automatic:           false,
	})).WithAggregate("wf-apply", 5)
	agg.Apply(manualRetry)

	if got := agg.AutoRetries["developer"]; got != 0 {
		t.Errorf("manual retry bumped AutoRetries to %d; want 0 (operator retries must not count against engine cap)", got)
	}

	// Engine-initiated retry: Automatic=true.
	autoRetry := event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
		FromPhase:           "developer",
		InvalidatedPersonas: []string{"developer"},
		Reason:              "engine: auto-retry on transient idle_timeout",
		Automatic:           true,
	})).WithAggregate("wf-apply", 6)
	agg.Apply(autoRetry)

	if got := agg.AutoRetries["developer"]; got != 1 {
		t.Errorf("automatic retry bumped AutoRetries to %d; want 1", got)
	}
}

// TestAggregate_AutoRetry_RoundTripThroughApply is the end-to-end
// invariant: Decide emits a retry, Apply consumes it, and the aggregate
// now refuses a second auto-retry for the same persona. This catches
// regressions where Decide and Apply disagree on what the payload's
// Automatic flag means.
func TestAggregate_AutoRetry_RoundTripThroughApply(t *testing.T) {
	agg := NewWorkflowAggregate("wf-round-trip")
	agg.Status = StatusRunning
	def := WorkspaceDevWorkflowDef()
	agg.WorkflowDef = &def

	firstFail := event.Envelope{
		Type:          event.PersonaFailed,
		AggregateID:   "wf-round-trip",
		CorrelationID: "corr-round-trip",
		Payload: event.MustMarshal(event.PersonaFailedPayload{
			Persona:     "developer",
			FailureKind: event.FailureKindIdleTimeout,
		}),
	}
	retryEvents, err := agg.Decide(firstFail)
	if err != nil || len(retryEvents) != 1 || retryEvents[0].Type != event.WorkflowRetried {
		t.Fatalf("first Decide: want [WorkflowRetried], got events=%+v err=%v", retryEvents, err)
	}
	agg.Apply(retryEvents[0])

	// After Apply, Status is back to Running (verified separately) and
	// AutoRetries["developer"] == 1. A second idle_timeout must NOT
	// retry.
	if agg.Status != StatusRunning {
		t.Errorf("Status after retry Apply = %q; want Running", agg.Status)
	}
	if agg.AutoRetries["developer"] != 1 {
		t.Errorf("AutoRetries[developer] = %d; want 1 after Apply", agg.AutoRetries["developer"])
	}

	secondFail := firstFail
	secondFail.Payload = event.MustMarshal(event.PersonaFailedPayload{
		Persona:     "developer",
		FailureKind: event.FailureKindIdleTimeout,
		Error:       "still idle",
	})
	finalEvents, err := agg.Decide(secondFail)
	if err != nil {
		t.Fatalf("second Decide: %v", err)
	}
	if len(finalEvents) != 1 || finalEvents[0].Type != event.WorkflowFailed {
		t.Fatalf("second Decide: want [WorkflowFailed] after cap, got %+v", finalEvents)
	}
}

