package engine

import (
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// TestAggregate_PartialReview_AbsorbsRequiredFailure covers the fix for the
// cascade-cancellation incident on hulilabs/huli#802 (correlation
// 154ce63a-42d3-41b0-b008-b8c083e538bc, 2026-04-24). Under
// WorkflowDef.PartialReviewOnFailure, a required-persona failure must NOT
// emit WorkflowFailed — it must be absorbed as a skip so the workflow
// proceeds and siblings finish instead of being context-cancelled. The
// Apply path is responsible for marking the persona complete; Decide is
// responsible for deciding NOT to fail.
func TestAggregate_PartialReview_AbsorbsRequiredFailure(t *testing.T) {
	agg := NewWorkflowAggregate("wf-partial")
	agg.Status = StatusRunning
	def := WorkflowDef{
		ID:                     "test-partial",
		Required:               []string{"pr-security", "pr-data"},
		PartialReviewOnFailure: true,
	}
	agg.WorkflowDef = &def

	// Apply the PersonaFailedTracked mirror first (engine.go emits this
	// before Decide under the required-persona + running-workflow guard).
	trackedEnv := event.Envelope{
		Type:          event.PersonaFailedTracked,
		AggregateID:   "wf-partial",
		CorrelationID: "corr-partial",
		Payload: event.MustMarshal(event.PersonaFailedPayload{
			Persona:     "pr-data",
			Error:       "handler pr-data: backend: claude: exit status 1 (after 1m35s)",
			FailureKind: event.FailureKindBackendError,
			Backend:     "claude",
		}),
	}
	agg.Apply(trackedEnv)

	if !agg.CompletedPersonas["pr-data"] {
		t.Fatal("partial-review Apply: pr-data must be added to CompletedPersonas so completion check fires naturally")
	}
	if !agg.SkippedPersonas["pr-data"] {
		t.Fatal("partial-review Apply: pr-data must be added to SkippedPersonas for observability/consolidator reporting")
	}

	// Now Decide on the original PersonaFailed envelope.
	failEnv := event.Envelope{
		Type:          event.PersonaFailed,
		AggregateID:   "wf-partial",
		CorrelationID: "corr-partial",
		Payload:       trackedEnv.Payload,
	}
	events, err := agg.Decide(failEnv)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// pr-security hasn't completed yet → no WorkflowCompleted should fire;
	// the workflow should quietly wait for siblings.
	for _, e := range events {
		if e.Type == event.WorkflowFailed {
			t.Fatalf("partial-review Decide must NOT emit WorkflowFailed; got %+v", e)
		}
		if e.Type == event.WorkflowCompleted {
			t.Fatalf("WorkflowCompleted fired prematurely (pr-security still pending); got %+v", e)
		}
	}
}

// TestAggregate_PartialReview_EmitsCompletedWhenLastRequiredPersonaFinishes
// verifies the terminal path: when a partial-review workflow's LAST
// required persona fails (and all others have completed), the aggregate
// emits WorkflowCompleted rather than WorkflowFailed. Otherwise the
// workflow would wedge forever with no terminal event.
func TestAggregate_PartialReview_EmitsCompletedWhenLastRequiredPersonaFinishes(t *testing.T) {
	agg := NewWorkflowAggregate("wf-partial-last")
	agg.Status = StatusRunning
	def := WorkflowDef{
		ID:                     "test-partial-last",
		Required:               []string{"pr-security", "pr-data"},
		PartialReviewOnFailure: true,
	}
	agg.WorkflowDef = &def
	agg.CompletedPersonas["pr-security"] = true // the one real-pass sibling

	trackedEnv := event.Envelope{
		Type:          event.PersonaFailedTracked,
		AggregateID:   "wf-partial-last",
		CorrelationID: "corr-partial-last",
		Payload: event.MustMarshal(event.PersonaFailedPayload{
			Persona: "pr-data",
			Error:   "crashed",
		}),
	}
	agg.Apply(trackedEnv)

	failEnv := event.Envelope{
		Type:          event.PersonaFailed,
		AggregateID:   "wf-partial-last",
		CorrelationID: "corr-partial-last",
		Payload:       trackedEnv.Payload,
	}
	events, err := agg.Decide(failEnv)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	var completed *event.Envelope
	for i, e := range events {
		if e.Type == event.WorkflowCompleted {
			completed = &events[i]
		}
		if e.Type == event.WorkflowFailed {
			t.Fatalf("partial-review Decide must NOT emit WorkflowFailed on terminal skip; got %+v", e)
		}
	}
	if completed == nil {
		t.Fatal("expected WorkflowCompleted when all required personas are done (including skip)")
	}

	// Verify the Result string signals partial completion.
	if !containsBytes(completed.Payload, "skipped") {
		t.Errorf("WorkflowCompleted payload should mention 'skipped' when SkippedPersonas is non-empty; got %s", string(completed.Payload))
	}
}

// TestAggregate_PartialReview_NonRequiredFailureIgnored confirms that the
// existing "non-required personas don't fail the workflow" guard is
// preserved under PartialReviewOnFailure — a before-hook or enricher
// failure still returns nil events, same as the fail-fast default.
func TestAggregate_PartialReview_NonRequiredFailureIgnored(t *testing.T) {
	agg := NewWorkflowAggregate("wf-non-req")
	agg.Status = StatusRunning
	def := WorkflowDef{
		ID:                     "test-non-req",
		Required:               []string{"pr-security"},
		PartialReviewOnFailure: true,
	}
	agg.WorkflowDef = &def

	failEnv := event.Envelope{
		Type:          event.PersonaFailed,
		AggregateID:   "wf-non-req",
		CorrelationID: "corr-non-req",
		Payload: event.MustMarshal(event.PersonaFailedPayload{
			Persona: "some-enricher",
			Error:   "non-required failure",
		}),
	}
	events, err := agg.Decide(failEnv)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("non-required failure should yield zero events; got %+v", events)
	}
}

// TestAggregate_FailFast_StillFailsWithoutPartialFlag is the regression
// guard: turning PartialReviewOnFailure on for one workflow must not
// quietly leak the new semantic into fail-fast workflows. A required-
// persona failure on a workflow WITHOUT the flag must still emit
// WorkflowFailed (existing behavior).
func TestAggregate_FailFast_StillFailsWithoutPartialFlag(t *testing.T) {
	agg := NewWorkflowAggregate("wf-fail-fast")
	agg.Status = StatusRunning
	def := WorkflowDef{
		ID:       "test-fail-fast",
		Required: []string{"developer"},
		// PartialReviewOnFailure: false (default)
	}
	agg.WorkflowDef = &def

	failEnv := event.Envelope{
		Type:          event.PersonaFailed,
		AggregateID:   "wf-fail-fast",
		CorrelationID: "corr-fail-fast",
		Payload: event.MustMarshal(event.PersonaFailedPayload{
			Persona:     "developer",
			Error:       "backend error",
			FailureKind: event.FailureKindBackendError,
		}),
	}
	events, err := agg.Decide(failEnv)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	var failed bool
	for _, e := range events {
		if e.Type == event.WorkflowFailed {
			failed = true
		}
	}
	if !failed {
		t.Error("fail-fast workflow must still emit WorkflowFailed on required-persona failure")
	}
}

func containsBytes(b []byte, substr string) bool {
	for i := 0; i+len(substr) <= len(b); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			if b[i+j] != substr[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
