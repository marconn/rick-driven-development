package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/persona"
)

// makeVerdict builds a VerdictRendered envelope wired to a correlation,
// tagged with the given DevTriggerID. Issues default to nil for PASS
// verdicts; callers pass explicit issues for FAIL.
func makeVerdict(corr, devTrigger, source string, outcome event.VerdictOutcome, issues []event.Issue, summary string) event.Envelope {
	return event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Persona:       "developer",
		SourcePersona: source,
		Outcome:       outcome,
		Issues:        issues,
		Summary:       summary,
		DevTriggerID:  devTrigger,
	})).WithCorrelation(corr).WithSource("handler:" + source)
}

// makeFeedback builds a FeedbackGenerated envelope used to seed iteration
// counters when testing iteration arithmetic.
func makeFeedback(corr, target string) event.Envelope {
	return event.New(event.FeedbackGenerated, 1, event.MustMarshal(event.FeedbackGeneratedPayload{
		TargetPersona: target,
		Iteration:     1,
	})).WithCorrelation(corr).WithSource("engine:aggregate")
}

func newConsolidator(store *mockStore) *ReviewConsolidator {
	return NewReviewConsolidator(ReviewConsolidatorConfig{
		Reviewers:     []string{"reviewer", "qa"},
		TargetPersona: "developer",
		Store:         store,
	})
}

// =============================================================================
// Both PASS — no FeedbackGenerated, consolidator just completes silently
// =============================================================================

func TestReviewConsolidator_BothPassEmitsNothing(t *testing.T) {
	store := newMockStore()
	corr := "corr-both-pass"
	dev := "dev-trigger-1"

	store.correlationEvents[corr] = []event.Envelope{
		makeVerdict(corr, dev, "reviewer", event.VerdictPass, nil, "passed review"),
		makeVerdict(corr, dev, "qa", event.VerdictPass, nil, "passed review"),
	}

	h := newConsolidator(store)
	trigger := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "qa",
	})).WithCorrelation(corr)

	results, err := h.Handle(context.Background(), trigger)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("want no events (both passed), got %d: %+v", len(results), results)
	}
}

// =============================================================================
// One FAIL one PASS — emits merged FeedbackGenerated with only the failing
// reviewer's issues
// =============================================================================

func TestReviewConsolidator_OneFailEmitsFeedback(t *testing.T) {
	store := newMockStore()
	corr := "corr-mixed"
	dev := "dev-trigger-2"

	store.correlationEvents[corr] = []event.Envelope{
		makeVerdict(corr, dev, "reviewer", event.VerdictPass, nil, "passed review"),
		makeVerdict(corr, dev, "qa", event.VerdictFail, []event.Issue{
			{Severity: "major", Category: "testing", Description: "missing edge case"},
		}, "qa says fail"),
	}

	h := newConsolidator(store)
	trigger := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "qa",
	})).WithCorrelation(corr)

	results, err := h.Handle(context.Background(), trigger)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 event (FeedbackGenerated), got %d", len(results))
	}
	if results[0].Type != event.FeedbackGenerated {
		t.Errorf("want FeedbackGenerated, got %s", results[0].Type)
	}

	var fb event.FeedbackGeneratedPayload
	if err := json.Unmarshal(results[0].Payload, &fb); err != nil {
		t.Fatalf("unmarshal feedback: %v", err)
	}
	if fb.TargetPersona != "developer" {
		t.Errorf("target = %q, want developer", fb.TargetPersona)
	}
	if fb.SourcePersona != ReviewConsolidatorName {
		t.Errorf("source = %q, want %q", fb.SourcePersona, ReviewConsolidatorName)
	}
	if len(fb.Issues) != 1 {
		t.Fatalf("want 1 issue (only the qa-failing one), got %d", len(fb.Issues))
	}
	if !strings.HasPrefix(fb.Issues[0].Description, "[qa]") {
		t.Errorf("issue description not tagged with source: %q", fb.Issues[0].Description)
	}
	if fb.Iteration != 1 {
		t.Errorf("iteration = %d, want 1 (no prior feedback)", fb.Iteration)
	}
}

// =============================================================================
// Both FAIL — emits one merged FeedbackGenerated with issues from BOTH,
// tagged by source persona so the developer can attribute each concern
// =============================================================================

func TestReviewConsolidator_BothFailMergesIssues(t *testing.T) {
	store := newMockStore()
	corr := "corr-both-fail"
	dev := "dev-trigger-3"

	store.correlationEvents[corr] = []event.Envelope{
		makeVerdict(corr, dev, "reviewer", event.VerdictFail, []event.Issue{
			{Severity: "critical", Category: "security", Description: "SQL injection"},
			{Severity: "major", Category: "concurrency", Description: "race in writer"},
		}, "reviewer says fail"),
		makeVerdict(corr, dev, "qa", event.VerdictFail, []event.Issue{
			{Severity: "minor", Category: "testing", Description: "no nil-input test"},
		}, "qa says fail"),
	}

	h := newConsolidator(store)
	trigger := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "qa",
	})).WithCorrelation(corr)

	results, err := h.Handle(context.Background(), trigger)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 FeedbackGenerated, got %d", len(results))
	}

	var fb event.FeedbackGeneratedPayload
	if err := json.Unmarshal(results[0].Payload, &fb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fb.Issues) != 3 {
		t.Fatalf("want 3 issues (2 reviewer + 1 qa), got %d: %+v", len(fb.Issues), fb.Issues)
	}

	// Reviewer issues come first (configured reviewer order), followed by qa.
	if !strings.HasPrefix(fb.Issues[0].Description, "[reviewer]") {
		t.Errorf("issue[0] should be reviewer-sourced, got %q", fb.Issues[0].Description)
	}
	if !strings.HasPrefix(fb.Issues[1].Description, "[reviewer]") {
		t.Errorf("issue[1] should be reviewer-sourced, got %q", fb.Issues[1].Description)
	}
	if !strings.HasPrefix(fb.Issues[2].Description, "[qa]") {
		t.Errorf("issue[2] should be qa-sourced, got %q", fb.Issues[2].Description)
	}

	if !strings.Contains(fb.Summary, "reviewer") || !strings.Contains(fb.Summary, "qa") {
		t.Errorf("summary should mention both source personas, got %q", fb.Summary)
	}
}

// =============================================================================
// Stale verdicts — verdicts from a prior dev iteration must be excluded.
// Only the latest DevTriggerID's verdicts count.
// =============================================================================

func TestReviewConsolidator_IgnoresVerdictsFromPriorRounds(t *testing.T) {
	store := newMockStore()
	corr := "corr-stale"
	devOld := "dev-trigger-old"
	devNew := "dev-trigger-new"

	store.correlationEvents[corr] = []event.Envelope{
		// Round 1 — both failed.
		makeVerdict(corr, devOld, "reviewer", event.VerdictFail, []event.Issue{
			{Description: "ancient issue"},
		}, "ancient"),
		makeVerdict(corr, devOld, "qa", event.VerdictFail, []event.Issue{
			{Description: "ancient qa issue"},
		}, "ancient"),
		// FeedbackGenerated from round 1 (sets iteration count to 1).
		makeFeedback(corr, "developer"),
		// Round 2 — both passed.
		makeVerdict(corr, devNew, "reviewer", event.VerdictPass, nil, "passed review"),
		makeVerdict(corr, devNew, "qa", event.VerdictPass, nil, "passed review"),
	}

	h := newConsolidator(store)
	trigger := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "qa",
	})).WithCorrelation(corr)

	results, err := h.Handle(context.Background(), trigger)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("round 2 was all-pass — want no events, got %d: %+v", len(results), results)
	}
}

// =============================================================================
// Iteration arithmetic — N prior FeedbackGenerated events bump iteration to N+1
// =============================================================================

func TestReviewConsolidator_IterationCountsPriorFeedback(t *testing.T) {
	store := newMockStore()
	corr := "corr-iter"
	dev := "dev-trigger-iter"

	store.correlationEvents[corr] = []event.Envelope{
		makeFeedback(corr, "developer"),
		makeFeedback(corr, "developer"),
		makeFeedback(corr, "other-target"), // not counted — different target
		makeVerdict(corr, dev, "reviewer", event.VerdictPass, nil, "passed review"),
		makeVerdict(corr, dev, "qa", event.VerdictFail, []event.Issue{
			{Description: "still broken"},
		}, "qa says fail"),
	}

	h := newConsolidator(store)
	trigger := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "qa",
	})).WithCorrelation(corr)

	results, err := h.Handle(context.Background(), trigger)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 FeedbackGenerated, got %d", len(results))
	}
	var fb event.FeedbackGeneratedPayload
	if err := json.Unmarshal(results[0].Payload, &fb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 2 prior FeedbackGenerated for developer → next iteration is 3.
	if fb.Iteration != 3 {
		t.Errorf("iteration = %d, want 3", fb.Iteration)
	}
}

// =============================================================================
// Empty-history guard — no verdicts on the chain means no-op, not panic
// =============================================================================

func TestReviewConsolidator_EmptyChainNoOps(t *testing.T) {
	store := newMockStore()
	corr := "corr-empty"
	store.correlationEvents[corr] = []event.Envelope{}

	h := newConsolidator(store)
	trigger := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "qa",
	})).WithCorrelation(corr)

	results, err := h.Handle(context.Background(), trigger)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("want no events on empty chain, got %d", len(results))
	}
}

// =============================================================================
// Advisory fail — excluded from the failing-set so the consolidator does not
// emit feedback (the aggregate has already paused via escalateVerdict)
// =============================================================================

func TestReviewConsolidator_AdvisoryFailDoesNotEmitFeedback(t *testing.T) {
	store := newMockStore()
	corr := "corr-advisory"
	dev := "dev-trigger-adv"

	advisoryVerdict := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Persona:       "developer",
		SourcePersona: "reviewer",
		Outcome:       event.VerdictFail,
		Advisory:      true,
		Summary:       "advisory only",
		DevTriggerID:  dev,
	})).WithCorrelation(corr).WithSource("handler:reviewer")

	store.correlationEvents[corr] = []event.Envelope{
		advisoryVerdict,
		makeVerdict(corr, dev, "qa", event.VerdictPass, nil, "passed review"),
	}

	h := newConsolidator(store)
	trigger := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "qa",
	})).WithCorrelation(corr)

	results, err := h.Handle(context.Background(), trigger)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("advisory fail should not produce feedback (aggregate already paused), got %d events", len(results))
	}
}

// =============================================================================
// Summary-only fail — when a reviewer emits a fail verdict with no structured
// issues, the summary is preserved as a synthetic issue so it is not silently
// dropped from the merged feedback
// =============================================================================

func TestReviewConsolidator_FailWithNoIssuesPromotesSummary(t *testing.T) {
	store := newMockStore()
	corr := "corr-summary-only"
	dev := "dev-trigger-summary"

	store.correlationEvents[corr] = []event.Envelope{
		makeVerdict(corr, dev, "reviewer", event.VerdictFail, nil, "code is just wrong, please rewrite"),
		makeVerdict(corr, dev, "qa", event.VerdictPass, nil, "passed review"),
	}

	h := newConsolidator(store)
	trigger := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "qa",
	})).WithCorrelation(corr)

	results, err := h.Handle(context.Background(), trigger)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 FeedbackGenerated, got %d", len(results))
	}
	var fb event.FeedbackGeneratedPayload
	if err := json.Unmarshal(results[0].Payload, &fb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fb.Issues) != 1 {
		t.Fatalf("want 1 synthesized issue from summary, got %d", len(fb.Issues))
	}
	if !strings.Contains(fb.Issues[0].Description, "code is just wrong") {
		t.Errorf("synthesized issue should carry the summary text, got %q", fb.Issues[0].Description)
	}
}

// =============================================================================
// Construction validation — Reviewers and Store are mandatory
// =============================================================================

func TestNewReviewConsolidator_PanicsOnEmptyReviewers(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty Reviewers")
		}
	}()
	NewReviewConsolidator(ReviewConsolidatorConfig{
		Reviewers: nil,
		Store:     newMockStore(),
	})
}

func TestNewReviewConsolidator_PanicsOnNilStore(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil Store")
		}
	}()
	NewReviewConsolidator(ReviewConsolidatorConfig{
		Reviewers: []string{"reviewer", "qa"},
		Store:     nil,
	})
}

// =============================================================================
// ReviewHandler regression — verdict carries DevTriggerID from envelope ID
// (required by the consolidator's round-pairing logic)
// =============================================================================

func TestReviewHandler_VerdictCarriesDevTriggerID(t *testing.T) {
	store := newMockStore()
	corrID := "corr-dev-trigger"
	store.correlationEvents[corrID] = []event.Envelope{
		event.New(event.WorkflowRequested, 1, event.MustMarshal(event.WorkflowRequestedPayload{
			Prompt: "build it",
		})).WithCorrelation(corrID),
	}

	mb := &mockBackend{
		name: "claude",
		response: &backend.Response{
			Output:   "looks ok\n\nVERDICT: PASS",
			Duration: 1 * time.Second,
		},
	}

	h := NewReviewHandler(ReviewHandlerConfig{
		AIConfig: AIHandlerConfig{
			Name:     "reviewer",
			Persona:  persona.Reviewer,
			Backend:  mb,
			Store:    store,
			Personas: persona.DefaultRegistry(),
			Builder:  persona.NewPromptBuilder(),
		},
		TargetPersona: "developer",
	})

	devID := event.ID("dev-evt-42")
	env := event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
		Persona: "developer",
	})).WithCorrelation(corrID)
	env.ID = devID

	results, err := h.Handle(context.Background(), env)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var verdictEvt *event.Envelope
	for i := range results {
		if results[i].Type == event.VerdictRendered {
			verdictEvt = &results[i]
			break
		}
	}
	if verdictEvt == nil {
		t.Fatal("no VerdictRendered emitted")
	}
	var v event.VerdictPayload
	if err := json.Unmarshal(verdictEvt.Payload, &v); err != nil {
		t.Fatalf("unmarshal verdict: %v", err)
	}
	if v.DevTriggerID != string(devID) {
		t.Errorf("DevTriggerID = %q, want %q", v.DevTriggerID, string(devID))
	}
}
