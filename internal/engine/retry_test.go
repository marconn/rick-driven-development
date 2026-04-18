package engine

import (
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// TestApply_WorkflowRetried_ClearsInvalidatedPersonas verifies that Apply
// drops the listed personas from CompletedPersonas, leaves upstream
// completions intact, and flips Status back to Running.
func TestApply_WorkflowRetried_ClearsInvalidatedPersonas(t *testing.T) {
	agg := NewWorkflowAggregate("wf-retry-1")
	agg.Status = StatusFailed
	agg.CompletedPersonas = map[string]bool{
		"researcher": true,
		"architect":  true,
		"developer":  true,
		"reviewer":   true,
	}

	retryEvt := event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
		FromPhase:           "developer",
		InvalidatedPersonas: []string{"developer", "reviewer"},
	})).WithAggregate(agg.ID, 1)

	agg.Apply(retryEvt)

	if agg.Status != StatusRunning {
		t.Errorf("Status = %q, want %q", agg.Status, StatusRunning)
	}
	if !agg.CompletedPersonas["researcher"] {
		t.Errorf("researcher should stay completed (upstream), got cleared")
	}
	if !agg.CompletedPersonas["architect"] {
		t.Errorf("architect should stay completed (upstream), got cleared")
	}
	if agg.CompletedPersonas["developer"] {
		t.Errorf("developer should be cleared (invalidated)")
	}
	if agg.CompletedPersonas["reviewer"] {
		t.Errorf("reviewer should be cleared (invalidated)")
	}
}

// TestApply_WorkflowRetried_ClearsFeedbackPending verifies that stale
// feedback gates pointing at invalidated personas don't survive the retry.
// Otherwise a re-dispatched source persona would be blocked forever waiting
// for a target whose completion was just cleared.
func TestApply_WorkflowRetried_ClearsFeedbackPending(t *testing.T) {
	agg := NewWorkflowAggregate("wf-retry-fb")
	agg.Status = StatusFailed
	agg.CompletedPersonas = map[string]bool{"reviewer": true, "developer": true}
	agg.FeedbackPending = map[string]string{
		"reviewer": "developer", // reviewer waiting for developer
	}

	retryEvt := event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
		FromPhase:           "developer",
		InvalidatedPersonas: []string{"developer", "reviewer"},
	})).WithAggregate(agg.ID, 1)

	agg.Apply(retryEvt)

	if _, stillPending := agg.FeedbackPending["reviewer"]; stillPending {
		t.Errorf("FeedbackPending[reviewer] should be cleared, got %v", agg.FeedbackPending)
	}
}

// TestApply_WorkflowRetried_EmptyInvalidatedList only flips Status; no
// CompletedPersonas entries are touched when the emitter passes no names.
// This guards against accidental global wipes if from_phase happens to be
// DAG-root or empty.
func TestApply_WorkflowRetried_EmptyInvalidatedList(t *testing.T) {
	agg := NewWorkflowAggregate("wf-retry-empty")
	agg.Status = StatusCancelled
	agg.CompletedPersonas = map[string]bool{"alpha": true, "beta": true}

	retryEvt := event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
		FromPhase:           "alpha",
		InvalidatedPersonas: nil,
	})).WithAggregate(agg.ID, 1)

	agg.Apply(retryEvt)

	if agg.Status != StatusRunning {
		t.Errorf("Status = %q, want Running", agg.Status)
	}
	if len(agg.CompletedPersonas) != 2 {
		t.Errorf("CompletedPersonas should be untouched, got %v", agg.CompletedPersonas)
	}
}

// TestDecide_WorkflowRetried_NoAdditionalEvents verifies Decide is a no-op
// for WorkflowRetried — all state changes happen via Apply and dispatch is
// handled by PersonaRunner's subscription.
func TestDecide_WorkflowRetried_NoAdditionalEvents(t *testing.T) {
	agg := NewWorkflowAggregate("wf-retry-decide")
	agg.Status = StatusFailed
	agg.WorkflowDef = &WorkflowDef{ID: "test", Required: []string{"developer"}}

	retryEvt := event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
		FromPhase:           "developer",
		InvalidatedPersonas: []string{"developer"},
	})).WithAggregate(agg.ID, 1)

	events, err := agg.Decide(retryEvt)
	if err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("Decide should emit no events, got %d", len(events))
	}
}
