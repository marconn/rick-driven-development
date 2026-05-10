package engine

import (
	"encoding/json"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// resumeRetargetTestDef is the minimal WorkflowDef needed to exercise
// decideWorkflowResumed's retarget branch. "developer" is required so
// isRequiredPersona returns true; "ghost" is intentionally absent so the
// non-required guard test can assert the skip path.
func resumeRetargetTestDef() WorkflowDef {
	return WorkflowDef{
		ID:                "wf-resume-retarget-test",
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

func newResumeRetargetAgg() *WorkflowAggregate {
	agg := NewWorkflowAggregate("wf-resume-retarget-test")
	def := resumeRetargetTestDef()
	agg.WorkflowDef = &def
	agg.WorkflowID = def.ID
	agg.Status = StatusPaused
	agg.MaxIterations = def.MaxIterations
	return agg
}

// TestDecideWorkflowResumed_RetargetSetEmitsFeedback covers the byte-identical
// pause case: PendingResumeRetarget is set, count < MaxIter; resume must
// re-emit FeedbackGenerated for the retarget pair using the cached
// diagnostics from LastFailingVerdict.
func TestDecideWorkflowResumed_RetargetSetEmitsFeedback(t *testing.T) {
	agg := newResumeRetargetAgg()
	agg.PendingResumeRetarget = "developer"
	agg.PendingResumeRetargetSource = "quality-gate"
	agg.FeedbackCount["developer"] = 1
	agg.LastFailingVerdict["developer"] = cachedVerdict{
		SourcePersona:  "quality-gate",
		RawDiagnostics: "make: *** No rule to make target 'test-backend'.  Stop.",
	}

	resume := event.Envelope{Type: event.WorkflowResumed, CorrelationID: "wf-resume-retarget-test"}
	out, err := agg.Decide(resume)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(out) != 1 || out[0].Type != event.FeedbackGenerated {
		t.Fatalf("expected one FeedbackGenerated, got %+v", out)
	}
	var fb event.FeedbackGeneratedPayload
	if err := json.Unmarshal(out[0].Payload, &fb); err != nil {
		t.Fatalf("unmarshal feedback: %v", err)
	}
	if fb.TargetPersona != "developer" {
		t.Errorf("TargetPersona: want developer, got %q", fb.TargetPersona)
	}
	if fb.SourcePersona != "quality-gate" {
		t.Errorf("SourcePersona: want quality-gate, got %q", fb.SourcePersona)
	}
	if fb.Iteration != 2 {
		t.Errorf("Iteration: want 2 (count+1), got %d", fb.Iteration)
	}
	if fb.RawDiagnostics == "" {
		t.Error("RawDiagnostics empty — cached diagnostics must rehydrate into FB")
	}
}

// TestDecideWorkflowResumed_AdvisoryNoRetargetReturnsNil locks in the
// advisory pause contract: empty retarget + count < MaxIter must NOT emit
// FeedbackGenerated. Advisory pauses advance via pauser.blocked replay,
// not via a re-triggered iteration.
func TestDecideWorkflowResumed_AdvisoryNoRetargetReturnsNil(t *testing.T) {
	agg := newResumeRetargetAgg()
	agg.PendingResumeRetarget = ""
	agg.FeedbackCount["developer"] = 1
	agg.LastFailingVerdict["developer"] = cachedVerdict{SourcePersona: "quality-gate"}

	resume := event.Envelope{Type: event.WorkflowResumed, CorrelationID: "wf-resume-retarget-test"}
	out, err := agg.Decide(resume)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if out != nil {
		t.Fatalf("expected no events for advisory resume, got %+v", out)
	}
}

// TestDecideWorkflowResumed_LegacyMaxIterFallback verifies that a
// pre-RetargetPersona-era WorkflowPaused (empty retarget) still drives
// resume correctly when the legacy max-iter condition holds. Without this
// fallback, in-flight workflows paused before the deploy would never resume.
func TestDecideWorkflowResumed_LegacyMaxIterFallback(t *testing.T) {
	agg := newResumeRetargetAgg()
	agg.PendingResumeRetarget = ""
	agg.FeedbackCount["developer"] = 3
	agg.MaxIterations = 3
	agg.LastFailingVerdict["developer"] = cachedVerdict{SourcePersona: "reviewer"}

	resume := event.Envelope{Type: event.WorkflowResumed, CorrelationID: "wf-resume-retarget-test"}
	out, err := agg.Decide(resume)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(out) != 1 || out[0].Type != event.FeedbackGenerated {
		t.Fatalf("expected legacy FB emission, got %+v", out)
	}
	if agg.MaxIterations != 4 {
		t.Errorf("legacy branch must bump MaxIterations to count+1; want 4 got %d", agg.MaxIterations)
	}
}

// TestDecideWorkflowResumed_NonRequiredPersonaSkipped guards against an
// operator-crafted WorkflowPaused with a bogus retarget corrupting
// CompletedPersonas via the FeedbackPending gate set in
// Apply(FeedbackGenerated).
func TestDecideWorkflowResumed_NonRequiredPersonaSkipped(t *testing.T) {
	agg := newResumeRetargetAgg()
	agg.PendingResumeRetarget = "ghost-persona"
	agg.PendingResumeRetargetSource = "quality-gate"
	agg.FeedbackCount["ghost-persona"] = 1

	resume := event.Envelope{Type: event.WorkflowResumed, CorrelationID: "wf-resume-retarget-test"}
	out, err := agg.Decide(resume)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if out != nil {
		t.Fatalf("non-required retarget must produce no events; got %+v", out)
	}
}

// TestDecideWorkflowResumed_ByteIdenticalPreservesMaxIter pins the
// MaxIterations bump policy: byte-identical at iter 1 with MaxIter 3 must
// NOT auto-grant unlimited retries. The bump only kicks in when the next
// iteration would otherwise exceed the budget.
func TestDecideWorkflowResumed_ByteIdenticalPreservesMaxIter(t *testing.T) {
	agg := newResumeRetargetAgg()
	agg.PendingResumeRetarget = "developer"
	agg.PendingResumeRetargetSource = "quality-gate"
	agg.FeedbackCount["developer"] = 1
	agg.MaxIterations = 3

	resume := event.Envelope{Type: event.WorkflowResumed, CorrelationID: "wf-resume-retarget-test"}
	if _, err := agg.Decide(resume); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if agg.MaxIterations != 3 {
		t.Errorf("MaxIterations must stay 3 when iteration<=MaxIter; got %d", agg.MaxIterations)
	}
}

// TestDecideWorkflowResumed_MaxIterRetargetBumps verifies that the retarget
// branch grants exactly one extra retry when resuming from a max-iter
// pause — same shape as the legacy fallback.
func TestDecideWorkflowResumed_MaxIterRetargetBumps(t *testing.T) {
	agg := newResumeRetargetAgg()
	agg.PendingResumeRetarget = "developer"
	agg.PendingResumeRetargetSource = "quality-gate"
	agg.FeedbackCount["developer"] = 3
	agg.MaxIterations = 3

	resume := event.Envelope{Type: event.WorkflowResumed, CorrelationID: "wf-resume-retarget-test"}
	if _, err := agg.Decide(resume); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if agg.MaxIterations != 4 {
		t.Errorf("MaxIterations must bump to iteration (4); got %d", agg.MaxIterations)
	}
}

// TestApplyWorkflowPausedOverwritesRetarget covers the Apply contract that
// every WorkflowPaused unconditionally overwrites the retarget pair (this
// is the canonical clearing path — Apply(WorkflowResumed) intentionally
// does NOT clear, because loadAggregate replays the new event before Decide
// runs and clearing would race the retarget read).
func TestApplyWorkflowPausedOverwritesRetarget(t *testing.T) {
	agg := newResumeRetargetAgg()

	pausedRetarget := event.Envelope{
		Type: event.WorkflowPaused,
		Payload: event.MustMarshal(event.WorkflowPausedPayload{
			RetargetPersona: "developer",
			RetargetSource:  "quality-gate",
		}),
	}
	agg.Apply(pausedRetarget)
	if agg.PendingResumeRetarget != "developer" || agg.PendingResumeRetargetSource != "quality-gate" {
		t.Fatalf("first pause must set retarget pair; got (%q,%q)",
			agg.PendingResumeRetarget, agg.PendingResumeRetargetSource)
	}

	resumed := event.Envelope{Type: event.WorkflowResumed}
	agg.Apply(resumed)
	if agg.Status != StatusRunning {
		t.Errorf("Apply(WorkflowResumed) must flip status to running; got %s", agg.Status)
	}
	if agg.PendingResumeRetarget != "developer" {
		t.Error("Apply(WorkflowResumed) must NOT clear retarget — Decide reads it after Apply")
	}

	// A subsequent pause with empty retarget (advisory / hint / operator)
	// must overwrite to empty.
	advisoryPause := event.Envelope{
		Type: event.WorkflowPaused,
		Payload: event.MustMarshal(event.WorkflowPausedPayload{
			Reason: "advisory",
		}),
	}
	agg.Apply(advisoryPause)
	if agg.PendingResumeRetarget != "" || agg.PendingResumeRetargetSource != "" {
		t.Errorf("subsequent advisory pause must overwrite retarget to empty; got (%q,%q)",
			agg.PendingResumeRetarget, agg.PendingResumeRetargetSource)
	}
}

// TestEscalateVerdictPayloadCarriesRetarget locks in the wire contract
// between escalateVerdict and decideWorkflowResumed: the byte-identical
// path emits WorkflowPaused with both retarget fields populated; the
// advisory path emits empty retarget.
func TestEscalateVerdictPayloadCarriesRetarget(t *testing.T) {
	agg := newResumeRetargetAgg()
	agg.Status = StatusRunning

	cases := []struct {
		name     string
		fn       func() []event.Envelope
		wantPers string
		wantSrc  string
	}{
		{
			name: "advisory_empty_retarget",
			fn: func() []event.Envelope {
				return agg.escalateVerdict(event.Envelope{}, "advisory failure", "", "")
			},
		},
		{
			name:     "byte_identical_carries_pair",
			wantPers: "developer",
			wantSrc:  "quality-gate",
			fn: func() []event.Envelope {
				return agg.escalateVerdict(event.Envelope{}, "byte-identical", "developer", "quality-gate")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.fn()
			if len(out) != 1 || out[0].Type != event.WorkflowPaused {
				t.Fatalf("expected one WorkflowPaused, got %+v", out)
			}
			var p event.WorkflowPausedPayload
			if err := json.Unmarshal(out[0].Payload, &p); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			if p.RetargetPersona != tc.wantPers {
				t.Errorf("RetargetPersona: want %q got %q", tc.wantPers, p.RetargetPersona)
			}
			if p.RetargetSource != tc.wantSrc {
				t.Errorf("RetargetSource: want %q got %q", tc.wantSrc, p.RetargetSource)
			}
			if p.Source != "engine:auto-escalation" {
				t.Errorf("Source: want engine:auto-escalation, got %q", p.Source)
			}
		})
	}
}
