package engine

import (
	"encoding/json"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// syncFeedbackTestDef is the minimal WorkflowDef that exercises the
// SynchronousFeedback gating path: SynchronousFeedback=true,
// ConsolidatedReviewers={reviewer, qa}, with quality-gate as a non-consolidated
// verdict source (so we can prove the gate does NOT swallow its feedback).
func syncFeedbackTestDef() WorkflowDef {
	return WorkflowDef{
		ID:                    "wf-sync-feedback-test",
		Required:              []string{"developer", "reviewer", "qa", "review-consolidator", "quality-gate", "committer"},
		MaxIterations:         3,
		EscalateOnMaxIter:     true,
		SynchronousFeedback:   true,
		ConsolidatedReviewers: []string{"reviewer", "qa"},
		ReviewConsolidator:    "review-consolidator",
		Graph: map[string][]string{
			"developer":           {},
			"reviewer":            {"developer"},
			"qa":                  {"developer"},
			"review-consolidator": {"reviewer", "qa"},
			"quality-gate":        {"review-consolidator"},
			"committer":           {"quality-gate"},
		},
		RetriggeredBy: map[string][]event.Type{"developer": {event.FeedbackGenerated}},
	}
}

// legacyFeedbackTestDef is the matching def WITHOUT SynchronousFeedback. Used
// to lock in the regression: the legacy path must behave identically before
// and after this change.
func legacyFeedbackTestDef() WorkflowDef {
	return WorkflowDef{
		ID:                "wf-legacy-feedback-test",
		Required:          []string{"developer", "reviewer", "qa", "quality-gate", "committer"},
		MaxIterations:     3,
		EscalateOnMaxIter: true,
		Graph: map[string][]string{
			"developer":    {},
			"reviewer":     {"developer"},
			"qa":           {"developer"},
			"quality-gate": {"reviewer", "qa"},
			"committer":    {"quality-gate"},
		},
		RetriggeredBy: map[string][]event.Type{"developer": {event.FeedbackGenerated}},
	}
}

func newSyncFeedbackAgg(def WorkflowDef) *WorkflowAggregate {
	agg := NewWorkflowAggregate(def.ID)
	d := def
	agg.WorkflowDef = &d
	agg.WorkflowID = def.ID
	agg.Status = StatusRunning
	agg.MaxIterations = def.MaxIterations
	return agg
}

func makeFailVerdict(source string) event.Envelope {
	return event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Persona:       "developer",
		SourcePersona: source,
		Outcome:       event.VerdictFail,
		Summary:       source + " says fail",
		Issues: []event.Issue{
			{Severity: "major", Category: "correctness", Description: "issue from " + source},
		},
	})).WithCorrelation("corr-sync-test")
}

// =============================================================================
// Regression — without SynchronousFeedback, each fail emits FeedbackGenerated
// (legacy behavior preserved)
// =============================================================================

func TestAggregate_LegacyVerdictStillEmitsFeedback(t *testing.T) {
	agg := newSyncFeedbackAgg(legacyFeedbackTestDef())

	out, err := agg.Decide(makeFailVerdict("reviewer"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(out) != 1 || out[0].Type != event.FeedbackGenerated {
		t.Fatalf("legacy path must emit one FeedbackGenerated, got %d events: %+v", len(out), out)
	}
}

// =============================================================================
// SynchronousFeedback ON — verdicts from ConsolidatedReviewers do NOT emit
// FeedbackGenerated (the consolidator handler will)
// =============================================================================

func TestAggregate_SynchronousFeedbackSuppressesReviewerFailFeedback(t *testing.T) {
	agg := newSyncFeedbackAgg(syncFeedbackTestDef())

	out, err := agg.Decide(makeFailVerdict("reviewer"))
	if err != nil {
		t.Fatalf("Decide reviewer: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("reviewer verdict must be suppressed under SynchronousFeedback, got %d events: %+v", len(out), out)
	}

	out, err = agg.Decide(makeFailVerdict("qa"))
	if err != nil {
		t.Fatalf("Decide qa: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("qa verdict must be suppressed under SynchronousFeedback, got %d events: %+v", len(out), out)
	}
}

// =============================================================================
// SynchronousFeedback ON — verdicts from sources OUTSIDE ConsolidatedReviewers
// still emit FeedbackGenerated. Critical for quality-gate, which is downstream
// of the consolidator and has its own failure path that must reach developer.
// =============================================================================

func TestAggregate_SynchronousFeedbackPassesThroughQualityGate(t *testing.T) {
	agg := newSyncFeedbackAgg(syncFeedbackTestDef())

	out, err := agg.Decide(makeFailVerdict("quality-gate"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(out) != 1 || out[0].Type != event.FeedbackGenerated {
		t.Fatalf("quality-gate verdict must NOT be suppressed (it is not in ConsolidatedReviewers); want one FeedbackGenerated, got %d: %+v", len(out), out)
	}

	var fb event.FeedbackGeneratedPayload
	if err := json.Unmarshal(out[0].Payload, &fb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fb.SourcePersona != "quality-gate" {
		t.Errorf("SourcePersona = %q, want quality-gate", fb.SourcePersona)
	}
}

// =============================================================================
// Escape hatch — advisory verdicts still escalate to WorkflowPaused even when
// SynchronousFeedback is on. Operators get paged on local-flake quality-gate
// regardless of the synchronization layer below it.
// =============================================================================

func TestAggregate_SynchronousFeedback_AdvisoryStillEscalates(t *testing.T) {
	agg := newSyncFeedbackAgg(syncFeedbackTestDef())

	advisory := event.New(event.VerdictRendered, 1, event.MustMarshal(event.VerdictPayload{
		Persona:       "developer",
		SourcePersona: "reviewer",
		Outcome:       event.VerdictFail,
		Advisory:      true,
		Summary:       "reviewer says advisory fail",
	})).WithCorrelation("corr-sync-test")

	out, err := agg.Decide(advisory)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(out) != 1 || out[0].Type != event.WorkflowPaused {
		t.Fatalf("advisory must escalate to WorkflowPaused regardless of SynchronousFeedback, got %+v", out)
	}
}

// =============================================================================
// Escape hatch — max-iterations on a single raw verdict still pauses the
// workflow even under SynchronousFeedback. A non-convergent reviewer must
// not wait for its peer before bailing.
// =============================================================================

func TestAggregate_SynchronousFeedback_MaxIterStillEscalates(t *testing.T) {
	agg := newSyncFeedbackAgg(syncFeedbackTestDef())
	agg.FeedbackCount["developer"] = agg.MaxIterations // next iteration would overflow

	out, err := agg.Decide(makeFailVerdict("reviewer"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(out) != 1 || out[0].Type != event.WorkflowPaused {
		t.Fatalf("max-iter must escalate even under SynchronousFeedback, got %+v", out)
	}
}

// =============================================================================
// Escape hatch — byte-identical verdict on second iteration still pauses
// even when SynchronousFeedback is on. Operator-corrected guidance is the
// resume intent; we must not wait for the second reviewer to render the
// same identical fingerprint before bailing.
// =============================================================================

func TestAggregate_SynchronousFeedback_ByteIdenticalStillEscalates(t *testing.T) {
	agg := newSyncFeedbackAgg(syncFeedbackTestDef())
	agg.FeedbackCount["developer"] = 1 // gate is iter >= 2

	// Seed LastVerdictFingerprint with the fingerprint of the verdict we are
	// about to feed in — that simulates the "second time we see this exact
	// failure" condition the byte-identical guard catches.
	failingPayload := event.VerdictPayload{
		Persona:       "developer",
		SourcePersona: "reviewer",
		Outcome:       event.VerdictFail,
		Summary:       "reviewer says fail",
		Issues: []event.Issue{
			{Severity: "major", Category: "correctness", Description: "issue from reviewer"},
		},
	}
	agg.LastVerdictFingerprint["reviewer|developer"] = verdictFingerprint(failingPayload)

	out, err := agg.Decide(event.New(event.VerdictRendered, 1, event.MustMarshal(failingPayload)).
		WithCorrelation("corr-sync-test"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(out) != 1 || out[0].Type != event.WorkflowPaused {
		t.Fatalf("byte-identical must escalate even under SynchronousFeedback, got %+v", out)
	}
}

// =============================================================================
// Env kill-switch regression — RICK_SYNCHRONOUS_FEEDBACK=0 must force the
// flag back to false at registration time, reverting to legacy behavior.
// =============================================================================

func TestApplyEnvOverrides_SynchronousFeedbackKillSwitch(t *testing.T) {
	def := syncFeedbackTestDef()
	if !def.SynchronousFeedback {
		t.Fatal("test setup: def must enable SynchronousFeedback")
	}

	t.Setenv("RICK_SYNCHRONOUS_FEEDBACK", "0")
	got := applyEnvOverrides(def)
	if got.SynchronousFeedback {
		t.Error("kill switch ineffective: SynchronousFeedback still true after RICK_SYNCHRONOUS_FEEDBACK=0")
	}
}

func TestApplyEnvOverrides_SynchronousFeedbackUnsetIsNoOp(t *testing.T) {
	def := syncFeedbackTestDef()
	// Explicitly unset so test order doesn't matter.
	t.Setenv("RICK_SYNCHRONOUS_FEEDBACK", "")
	got := applyEnvOverrides(def)
	if !got.SynchronousFeedback {
		t.Error("unset env should preserve SynchronousFeedback=true from def")
	}
}

// =============================================================================
// IsConsolidatedReviewer matrix — guard against accidental matches on the
// other side of the flag boundary
// =============================================================================

func TestWorkflowDef_IsConsolidatedReviewer(t *testing.T) {
	sync := syncFeedbackTestDef()
	legacy := legacyFeedbackTestDef()

	tests := []struct {
		name   string
		def    *WorkflowDef
		source string
		want   bool
	}{
		{"sync + listed reviewer", &sync, "reviewer", true},
		{"sync + listed qa", &sync, "qa", true},
		{"sync + non-listed quality-gate", &sync, "quality-gate", false},
		{"sync + empty source", &sync, "", false},
		{"legacy reviewer", &legacy, "reviewer", false},
		{"nil def", nil, "reviewer", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.def.IsConsolidatedReviewer(tc.source); got != tc.want {
				t.Errorf("IsConsolidatedReviewer(%q) = %v, want %v", tc.source, got, tc.want)
			}
		})
	}
}

// =============================================================================
// Workflow-def shape — every workflow that pairs reviewer + qa must include
// the synchronization-barrier wiring. Locks in the contract so a future
// refactor cannot silently regress one DAG to the async fan-out.
// =============================================================================

func TestBuiltinWorkflows_HaveConsistentConsolidatorWiring(t *testing.T) {
	cases := []struct {
		name string
		def  func() WorkflowDef
	}{
		{"workspace-dev", WorkspaceDevWorkflowDef},
		{"jira-dev", JiraDevWorkflowDef},
		{"github-dev", GithubDevWorkflowDef},
		{"pr-feedback", PRFeedbackWorkflowDef},
		{"ci-fix", CIFixWorkflowDef},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := tc.def()
			if !def.SynchronousFeedback {
				t.Error("SynchronousFeedback must be true")
			}
			if def.ReviewConsolidator != "review-consolidator" {
				t.Errorf("ReviewConsolidator = %q, want %q", def.ReviewConsolidator, "review-consolidator")
			}
			wantReviewers := map[string]bool{"reviewer": true, "qa": true}
			for _, r := range def.ConsolidatedReviewers {
				if !wantReviewers[r] {
					t.Errorf("unexpected ConsolidatedReviewer: %q", r)
				}
				delete(wantReviewers, r)
			}
			if len(wantReviewers) > 0 {
				t.Errorf("missing ConsolidatedReviewers: %v", wantReviewers)
			}

			// Graph: review-consolidator must depend on both reviewers,
			// quality-gate (when present) must depend on the consolidator.
			conDeps := def.Graph["review-consolidator"]
			depSet := map[string]bool{}
			for _, d := range conDeps {
				depSet[d] = true
			}
			if !depSet["reviewer"] || !depSet["qa"] {
				t.Errorf("review-consolidator deps = %v, want {reviewer, qa}", conDeps)
			}

			if qgDeps, ok := def.Graph["quality-gate"]; ok {
				if len(qgDeps) != 1 || qgDeps[0] != "review-consolidator" {
					t.Errorf("quality-gate deps = %v, want [review-consolidator]", qgDeps)
				}
			}

			reqSet := map[string]bool{}
			for _, r := range def.Required {
				reqSet[r] = true
			}
			if !reqSet["review-consolidator"] {
				t.Error("Required must include review-consolidator")
			}
		})
	}
}
