package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// TestDecideVerdictRendered_PropagatesRawDiagnostics locks in the verdict→
// feedback wire that the 2026-04-29 PR-D fix introduced. RawDiagnostics is
// captured by quality-gate before filterDockerNoise strips the human-facing
// description; this test asserts the unfiltered tail survives the aggregate's
// VerdictRendered → FeedbackGenerated transformation. Removing the field copy
// at decideVerdictRendered (currently aggregate.go:531) must fail this test.
func TestDecideVerdictRendered_PropagatesRawDiagnostics(t *testing.T) {
	agg := NewWorkflowAggregate("wf-raw")
	def := WorkspaceDevWorkflowDef()
	agg.WorkflowDef = &def
	agg.WorkflowID = def.ID
	agg.Status = StatusRunning

	raw := strings.Join([]string{
		"FAIL\tgithub.com/hulilabs/go-sdk/pkg/grpc/context\t0.009s",
		"--- FAIL: TestUnableToExtract (0.00s)",
		"    context_test.go:42: dial tcp 127.0.0.1:6379: connect: connection refused",
	}, "\n")

	verdictEnv := event.Envelope{
		Type:          event.VerdictRendered,
		AggregateID:   "wf-raw",
		CorrelationID: "corr-raw",
		Payload: event.MustMarshal(event.VerdictPayload{
			Persona:        "developer",
			SourcePersona:    "quality-gate",
			Outcome:        event.VerdictFail,
			Summary:        "tests failing",
			Issues:         []event.Issue{{Severity: "major", Description: "TestUnableToExtract"}},
			RawDiagnostics: raw,
		}),
	}
	results, err := agg.Decide(verdictEnv)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(results) != 1 || results[0].Type != event.FeedbackGenerated {
		t.Fatalf("want 1 FeedbackGenerated, got %+v", results)
	}

	var fb event.FeedbackGeneratedPayload
	if err := json.Unmarshal(results[0].Payload, &fb); err != nil {
		t.Fatalf("unmarshal feedback payload: %v", err)
	}
	if fb.RawDiagnostics != raw {
		t.Errorf("RawDiagnostics not propagated:\n got=%q\nwant=%q", fb.RawDiagnostics, raw)
	}
	if !strings.Contains(fb.RawDiagnostics, "connection refused") {
		t.Errorf("RawDiagnostics missing infrastructure marker: %q", fb.RawDiagnostics)
	}
}

// TestDecideVerdictRendered_CleanVerdictHasNoFeedback documents the negative
// contract: a passing verdict produces zero events (no FeedbackGenerated, no
// stray RawDiagnostics).
func TestDecideVerdictRendered_CleanVerdictHasNoFeedback(t *testing.T) {
	agg := NewWorkflowAggregate("wf-clean")
	def := WorkspaceDevWorkflowDef()
	agg.WorkflowDef = &def
	agg.WorkflowID = def.ID
	agg.Status = StatusRunning

	verdictEnv := event.Envelope{
		Type: event.VerdictRendered,
		Payload: event.MustMarshal(event.VerdictPayload{
			Persona:     "developer",
			SourcePersona: "quality-gate",
			Outcome:     event.VerdictPass,
			Summary:     "ok",
		}),
	}
	results, err := agg.Decide(verdictEnv)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("pass verdict should produce zero events, got %+v", results)
	}
}

// TestDecideWorkflowResumed_RehydratesRawDiagnostics covers the operator-
// resume gap that PR-D missed: when a max-iter pause is resumed, the
// re-emitted FeedbackGenerated must carry the most recent failing verdict's
// RawDiagnostics + SourcePhase so the developer's next iteration prompt is
// not stripped of the failure tail. Pre-fix `decideWorkflowResumed`
// constructed the payload with only TargetPhase/Iteration/Summary — this
// test fails red until LastFailingVerdict is wired through Apply + Decide.
func TestDecideWorkflowResumed_RehydratesRawDiagnostics(t *testing.T) {
	agg := NewWorkflowAggregate("wf-resume")
	def := WorkspaceDevWorkflowDef()
	agg.WorkflowDef = &def
	agg.WorkflowID = def.ID
	agg.Status = StatusRunning
	agg.MaxIterations = 1 // any failing iteration will hit the cap

	raw := "stderr tail from quality-gate\nconnection refused"

	// Apply a failing verdict tracker so the cache populates, and a matching
	// FeedbackGenerated so FeedbackCount[developer] reaches MaxIterations.
	// VerdictRendered itself lives on the persona-scoped aggregate; only
	// VerdictTracked lands on the workflow aggregate, so that's the event
	// Apply consumes for fingerprint/cache state.
	failVerdict := event.New(event.VerdictTracked, 1, event.MustMarshal(event.VerdictPayload{
		Persona:        "developer",
		SourcePersona:    "quality-gate",
		Outcome:        event.VerdictFail,
		Summary:        "x",
		RawDiagnostics: raw,
	})).WithAggregate("wf-resume", 1)
	agg.Apply(failVerdict)

	feedback := event.New(event.FeedbackGenerated, 1, event.MustMarshal(event.FeedbackGeneratedPayload{
		TargetPersona:    "developer",
		SourcePersona:    "quality-gate",
		Iteration:      1,
		RawDiagnostics: raw,
	})).WithAggregate("wf-resume", 2)
	agg.Apply(feedback)

	// Operator pause then resume.
	agg.Status = StatusPaused
	resumeEnv := event.Envelope{
		Type:          event.WorkflowResumed,
		AggregateID:   "wf-resume",
		CorrelationID: "corr-resume",
		Payload:       event.MustMarshal(event.WorkflowResumedPayload{}),
	}
	results, err := agg.Decide(resumeEnv)
	if err != nil {
		t.Fatalf("Decide(WorkflowResumed): %v", err)
	}
	if len(results) != 1 || results[0].Type != event.FeedbackGenerated {
		t.Fatalf("want 1 FeedbackGenerated, got %+v", results)
	}

	var fb event.FeedbackGeneratedPayload
	if err := json.Unmarshal(results[0].Payload, &fb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fb.RawDiagnostics != raw {
		t.Errorf("RawDiagnostics not rehydrated on resume:\n got=%q\nwant=%q", fb.RawDiagnostics, raw)
	}
	if fb.SourcePersona != "quality-gate" {
		t.Errorf("SourcePhase not rehydrated on resume: got=%q want=quality-gate", fb.SourcePersona)
	}
	if fb.TargetPersona != "developer" {
		t.Errorf("TargetPhase = %q; want developer", fb.TargetPersona)
	}
}

// TestDecideWorkflowResumed_NoPriorFailureLeavesEmpty covers the safe edge
// case: an operator-pause without any prior failing verdict (e.g. paused
// from clean state via hint review) must not panic on a missing cache
// entry and must leave RawDiagnostics empty.
func TestDecideWorkflowResumed_NoPriorFailureLeavesEmpty(t *testing.T) {
	agg := NewWorkflowAggregate("wf-resume-clean")
	def := WorkspaceDevWorkflowDef()
	agg.WorkflowDef = &def
	agg.WorkflowID = def.ID
	agg.Status = StatusRunning
	agg.MaxIterations = 1

	// Drive FeedbackCount via a FeedbackGenerated with empty RawDiagnostics,
	// no preceding VerdictRendered to populate the cache.
	feedback := event.New(event.FeedbackGenerated, 1, event.MustMarshal(event.FeedbackGeneratedPayload{
		TargetPersona: "developer",
		SourcePersona: "quality-gate",
		Iteration:   1,
	})).WithAggregate("wf-resume-clean", 1)
	agg.Apply(feedback)

	agg.Status = StatusPaused
	resumeEnv := event.Envelope{
		Type:    event.WorkflowResumed,
		Payload: event.MustMarshal(event.WorkflowResumedPayload{}),
	}
	results, err := agg.Decide(resumeEnv)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 event, got %+v", results)
	}
	var fb event.FeedbackGeneratedPayload
	_ = json.Unmarshal(results[0].Payload, &fb)
	if fb.RawDiagnostics != "" {
		t.Errorf("expected empty RawDiagnostics with no prior failing verdict, got %q", fb.RawDiagnostics)
	}
}
