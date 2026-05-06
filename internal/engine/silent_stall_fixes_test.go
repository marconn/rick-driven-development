package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// Regression suite for the 2026-04-20 docs-only silent-stall bug report
// (rick-bug-report-2026-04-20-docs-only-silent-stall.md). Three invariants
// are locked here so the fix does not drift:
//
//  1. WorkflowFailed propagates FailureKind + Backend + Stderr from the
//     originating PersonaFailed — operators see "idle_timeout on claude"
//     from rick_workflow_status without replaying the event chain.
//
//  2. When the aggregate decides WorkflowFailed (cap exceeded), the reason
//     string still reflects the underlying error AND the classifier fields
//     are machine-parsable. Previously rick_workflow_status returned
//     status:"failed" with no cause visible.
//
//  3. Apply ignores the PersonaFailedTracked storage-only mirror that the
//     Engine writes onto the workflow aggregate. Apply must not mis-count
//     it as a completion or a failure signal.

// TestWorkflowFailed_PropagatesFailureKindAndStderr locks the fact that the
// three diagnostic fields travel cleanly from PersonaFailed into
// WorkflowFailed when the cap is exceeded. This is what makes
// rick_workflow_status return actionable data instead of an opaque
// "failed".
func TestWorkflowFailed_PropagatesFailureKindAndStderr(t *testing.T) {
	agg := NewWorkflowAggregate("wf-propagate")
	agg.Status = StatusRunning
	def := WorkspaceDevWorkflowDef()
	agg.WorkflowDef = &def
	agg.AutoRetries["developer"] = MaxAutoRetriesPerPersona // cap exceeded → WorkflowFailed

	failEnv := event.Envelope{
		Type:          event.PersonaFailed,
		AggregateID:   "wf-propagate",
		CorrelationID: "corr-propagate",
		Payload: event.MustMarshal(event.PersonaFailedPayload{
			Persona:     "developer",
			Error:       "handler developer: backend: claude: backend: idle timeout exceeded (stall=2m0s) (after 5m0.025s)",
			FailureKind: event.FailureKindIdleTimeout,
			Backend:     "claude",
			Stderr:      "[claude] subprocess went silent at turn 3",
		}),
	}

	events, err := agg.Decide(failEnv)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(events) != 1 || events[0].Type != event.WorkflowFailed {
		t.Fatalf("want [WorkflowFailed] after cap, got %+v", events)
	}

	var wfp event.WorkflowFailedPayload
	if err := json.Unmarshal(events[0].Payload, &wfp); err != nil {
		t.Fatalf("unmarshal WorkflowFailedPayload: %v", err)
	}
	if wfp.FailureKind != event.FailureKindIdleTimeout {
		t.Errorf("FailureKind = %q; want idle_timeout (must match PersonaFailed classifier)",
			wfp.FailureKind)
	}
	if wfp.Backend != "claude" {
		t.Errorf("Backend = %q; want claude (must attribute failure to the driver that stalled)",
			wfp.Backend)
	}
	if wfp.Stderr != "[claude] subprocess went silent at turn 3" {
		t.Errorf("Stderr = %q; want the captured stderr tail", wfp.Stderr)
	}
	if wfp.Reason == "" {
		t.Errorf("Reason = empty; want a human-readable summary for log/UI fallback")
	}
	if wfp.Persona != "developer" {
		t.Errorf("Persona = %q; want developer (failure attribution)", wfp.Persona)
	}
}

// TestWorkflowFailed_EmptyFailureKindStaysEmpty guards against the
// aggregate fabricating diagnostic fields from thin air on non-persona
// failure paths (token budget, hint rejection). Those call sites must keep
// the new fields zero-valued so operators don't get a misleading
// "failure_kind: idle_timeout" on a budget overrun.
func TestWorkflowFailed_EmptyFailureKindStaysEmpty(t *testing.T) {
	agg := NewWorkflowAggregate("wf-empty")
	agg.Status = StatusRunning
	def := WorkspaceDevWorkflowDef()
	agg.WorkflowDef = &def

	// Token-budget path — no PersonaFailed upstream.
	events, err := agg.Decide(event.Envelope{
		Type:          event.TokenBudgetExceeded,
		AggregateID:   "wf-empty",
		CorrelationID: "corr-empty",
		Payload: event.MustMarshal(event.TokenBudgetExceededPayload{
			TotalUsed: 100,
			Budget:    50,
			Persona:   "developer",
		}),
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(events) != 1 || events[0].Type != event.WorkflowFailed {
		t.Fatalf("want [WorkflowFailed] on TokenBudgetExceeded, got %+v", events)
	}
	var wfp event.WorkflowFailedPayload
	if err := json.Unmarshal(events[0].Payload, &wfp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wfp.FailureKind != "" {
		t.Errorf("FailureKind = %q; want empty on non-persona failure (budget overrun)",
			wfp.FailureKind)
	}
	if wfp.Backend != "" || wfp.Stderr != "" {
		t.Errorf("Backend=%q Stderr=%q; both must be empty on token-budget path",
			wfp.Backend, wfp.Stderr)
	}
}

// TestApply_IgnoresPersonaFailedTracked pins the contract that the
// storage-only mirror emitted by Engine.tryProcessDecision does NOT update
// aggregate state. The mirror exists so `rick events <workflow_agg>` shows
// the failure breadcrumb — it must not count as a completion, nor re-enter
// the PersonaFailed decision path on replay.
func TestApply_IgnoresPersonaFailedTracked(t *testing.T) {
	agg := NewWorkflowAggregate("wf-mirror")
	agg.Status = StatusRunning
	def := WorkspaceDevWorkflowDef()
	agg.WorkflowDef = &def

	// Pretend Engine mirrored a PersonaFailed onto the workflow aggregate.
	mirror := event.New(event.PersonaFailedTracked, 1, event.MustMarshal(event.PersonaFailedPayload{
		Persona:     "developer",
		Error:       "idle timeout",
		FailureKind: event.FailureKindIdleTimeout,
	})).WithAggregate("wf-mirror", 5)

	beforeCompleted := len(agg.CompletedPersonas)
	agg.Apply(mirror)

	if len(agg.CompletedPersonas) != beforeCompleted {
		t.Errorf("PersonaFailedTracked bumped CompletedPersonas (%d → %d); "+
			"the mirror is storage-only and must not mutate completion state",
			beforeCompleted, len(agg.CompletedPersonas))
	}
	if agg.Status != StatusRunning {
		t.Errorf("Status = %q after PersonaFailedTracked Apply; want Running (mirror is not a terminal signal)",
			agg.Status)
	}
	// Apply still updates Version from the envelope — that's intentional, it
	// keeps the aggregate's version pointer monotonic. Verify.
	if agg.Version != 5 {
		t.Errorf("Version = %d; want 5 (Apply must still track envelope version)", agg.Version)
	}
}

// TestEngineMirrorsPersonaFailedOntoWorkflowAggregate locks the wire between
// PersonaRunner's persona-scoped PersonaFailed and the workflow-scoped
// PersonaFailedTracked mirror that Engine.tryProcessDecision now emits.
// Without this mirror, `rick events <workflow_agg>` shows only
// PersonaTracked successes and then a bare WorkflowRetried / WorkflowFailed
// — the failure payload lives on `{corr}:persona:{name}` and is invisible
// to the workflow-scoped view. The 2026-04-20 report's exact complaint.
func TestEngineMirrorsPersonaFailedOntoWorkflowAggregate(t *testing.T) {
	eng, store, _ := newTestEngine(t)
	ctx := context.Background()

	def := WorkflowDef{
		ID: "mirror-wf", Required: []string{"developer"}, MaxIterations: 3,
		Graph: map[string][]string{"developer": nil},
	}
	eng.RegisterWorkflow(def)

	wfID := "wf-mirror-e2e"
	reqEvt := event.New(event.WorkflowRequested, 1,
		event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "build", WorkflowID: "mirror-wf"})).
		WithAggregate(wfID, 1).WithCorrelation(wfID)
	startEvt := event.New(event.WorkflowStartedFor("mirror-wf"), 1,
		event.MustMarshal(event.WorkflowStartedPayload{WorkflowID: "mirror-wf"})).
		WithAggregate(wfID, 2).WithCorrelation(wfID)
	if err := store.Append(ctx, wfID, 0, []event.Envelope{reqEvt, startEvt}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Simulate the PersonaRunner appending PersonaFailed to its
	// persona-scoped aggregate — with an idle_timeout shape that will
	// trigger auto-retry (only mirror + retry should land on the workflow
	// aggregate; no WorkflowFailed because the cap is still 0/1).
	devFailed := event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
		Persona:     "developer",
		Error:       "handler developer: backend: claude: idle timeout exceeded (stall=2m0s)",
		FailureKind: event.FailureKindIdleTimeout,
		Backend:     "claude",
		Stderr:      "YOLO mode is enabled.",
	})).
		WithAggregate(wfID+":persona:developer", 1).
		WithCorrelation(wfID)

	if err := eng.processDecision(ctx, devFailed); err != nil {
		t.Fatalf("processDecision: %v", err)
	}

	events, err := store.Load(ctx, wfID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var mirror *event.Envelope
	for i := range events {
		if events[i].Type == event.PersonaFailedTracked {
			mirror = &events[i]
			break
		}
	}
	if mirror == nil {
		t.Fatal("PersonaFailedTracked mirror missing on workflow aggregate — operators can't see the failure via rick_events")
	}
	if mirror.AggregateID != wfID {
		t.Errorf("mirror aggregate = %q; want workflow aggregate %q", mirror.AggregateID, wfID)
	}
	var p event.PersonaFailedPayload
	if err := json.Unmarshal(mirror.Payload, &p); err != nil {
		t.Fatalf("mirror payload: %v", err)
	}
	if p.Persona != "developer" {
		t.Errorf("mirror Persona = %q; want developer", p.Persona)
	}
	if p.FailureKind != event.FailureKindIdleTimeout {
		t.Errorf("mirror FailureKind = %q; want idle_timeout (must preserve classifier)", p.FailureKind)
	}
	if p.Backend != "claude" {
		t.Errorf("mirror Backend = %q; want claude", p.Backend)
	}
	if p.Stderr == "" {
		t.Error("mirror Stderr empty — the stderr tail should travel to the workflow-scoped view")
	}

	// The auto-retry decision should ALSO land on the workflow aggregate,
	// positioned after the mirror so replay orders correctly.
	var retry *event.Envelope
	for i := range events {
		if events[i].Type == event.WorkflowRetried {
			retry = &events[i]
			break
		}
	}
	if retry == nil {
		t.Fatal("WorkflowRetried missing — auto-retry must still fire alongside the mirror")
	}
	if retry.Version <= mirror.Version {
		t.Errorf("retry version %d should exceed mirror version %d (aggregate version must be monotonic)",
			retry.Version, mirror.Version)
	}
}

// TestEngineSkipsMirrorForNonRequiredPersona guards the gate added after
// reviewer feedback: writing a PersonaFailedTracked for a non-required
// persona (hook, enricher) would imply a required-persona failure that the
// engine actually ignored via decidePersonaFailed's isRequiredPersona
// early-return. The operator would see a misleading breadcrumb claiming a
// required phase failed. Mirror must be suppressed in that case.
func TestEngineSkipsMirrorForNonRequiredPersona(t *testing.T) {
	eng, store, _ := newTestEngine(t)
	ctx := context.Background()

	// DAG with ONLY "developer" as required — a PersonaFailed for any
	// other persona must not mirror.
	def := WorkflowDef{
		ID: "gate-wf", Required: []string{"developer"}, MaxIterations: 3,
		Graph: map[string][]string{"developer": nil},
	}
	eng.RegisterWorkflow(def)

	wfID := "wf-gate"
	reqEvt := event.New(event.WorkflowRequested, 1,
		event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "x", WorkflowID: "gate-wf"})).
		WithAggregate(wfID, 1).WithCorrelation(wfID)
	startEvt := event.New(event.WorkflowStartedFor("gate-wf"), 1,
		event.MustMarshal(event.WorkflowStartedPayload{WorkflowID: "gate-wf"})).
		WithAggregate(wfID, 2).WithCorrelation(wfID)
	if err := store.Append(ctx, wfID, 0, []event.Envelope{reqEvt, startEvt}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Non-required persona failure — e.g., a jira-context enricher hook.
	hookFailed := event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
		Persona:     "jira-context",
		Error:       "jira api 500",
		FailureKind: event.FailureKindBackendError,
	})).
		WithAggregate(wfID+":persona:jira-context", 1).
		WithCorrelation(wfID)

	if err := eng.processDecision(ctx, hookFailed); err != nil {
		t.Fatalf("processDecision: %v", err)
	}

	events, err := store.Load(ctx, wfID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, e := range events {
		if e.Type == event.PersonaFailedTracked {
			t.Errorf("PersonaFailedTracked mirror written for non-required persona — misleading operators about required-phase failures")
		}
		if e.Type == event.WorkflowFailed {
			t.Errorf("WorkflowFailed emitted for non-required persona failure — decidePersonaFailed contract violated")
		}
	}
}

// TestEngineCapExceeded_TerminalFailurePropagatesClassifiers drives the full
// auto-retry → cap exceeded → WorkflowFailed sequence through the engine.
// This closes the end-to-end gap the TestReviewer flagged: prior unit tests
// forced AutoRetries via a field poke, so a regression that drops
// FailureKind only on the cap-exceeded Decide branch would pass those but
// break operator visibility in production. This replay-based test pins the
// terminal WorkflowFailed payload.
func TestEngineCapExceeded_TerminalFailurePropagatesClassifiers(t *testing.T) {
	eng, store, _ := newTestEngine(t)
	ctx := context.Background()

	def := WorkflowDef{
		ID: "cap-wf", Required: []string{"developer"}, MaxIterations: 3,
		Graph: map[string][]string{"developer": nil},
	}
	eng.RegisterWorkflow(def)

	wfID := "wf-cap-e2e"
	reqEvt := event.New(event.WorkflowRequested, 1,
		event.MustMarshal(event.WorkflowRequestedPayload{Prompt: "x", WorkflowID: "cap-wf"})).
		WithAggregate(wfID, 1).WithCorrelation(wfID)
	startEvt := event.New(event.WorkflowStartedFor("cap-wf"), 1,
		event.MustMarshal(event.WorkflowStartedPayload{WorkflowID: "cap-wf"})).
		WithAggregate(wfID, 2).WithCorrelation(wfID)
	if err := store.Append(ctx, wfID, 0, []event.Envelope{reqEvt, startEvt}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	makeFailed := func(durationMS int64) event.Envelope {
		return event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
			Persona:     "developer",
			Error:       "handler developer: backend: claude: idle timeout exceeded (stall=2m0s)",
			FailureKind: event.FailureKindIdleTimeout,
			Backend:     "claude",
			Stderr:      "subprocess silent tail",
			DurationMS:  durationMS,
		})).
			WithAggregate(wfID+":persona:developer", 1).
			WithCorrelation(wfID)
	}

	// First failure → auto-retry (AutoRetries[developer] = 1 after Apply).
	if err := eng.processDecision(ctx, makeFailed(390_000)); err != nil {
		t.Fatalf("first processDecision: %v", err)
	}

	// Second failure → cap exceeded, WorkflowFailed emitted on the workflow
	// aggregate with propagated classifiers.
	if err := eng.processDecision(ctx, makeFailed(420_000)); err != nil {
		t.Fatalf("second processDecision: %v", err)
	}

	events, err := store.Load(ctx, wfID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var failed *event.Envelope
	for i := range events {
		if events[i].Type == event.WorkflowFailed {
			failed = &events[i]
		}
	}
	if failed == nil {
		t.Fatal("WorkflowFailed missing — cap exceeded must trigger terminal transition")
	}
	var wfp event.WorkflowFailedPayload
	if err := json.Unmarshal(failed.Payload, &wfp); err != nil {
		t.Fatalf("unmarshal WorkflowFailed: %v", err)
	}
	if wfp.FailureKind != event.FailureKindIdleTimeout {
		t.Errorf("terminal WorkflowFailed.FailureKind = %q; want idle_timeout — rick_workflow_status loses the classifier",
			wfp.FailureKind)
	}
	if wfp.Backend != "claude" {
		t.Errorf("terminal WorkflowFailed.Backend = %q; want claude", wfp.Backend)
	}
	if wfp.Stderr == "" {
		t.Error("terminal WorkflowFailed.Stderr empty — operators can't see subprocess tail without replaying persona aggregate")
	}
	if wfp.Persona != "developer" {
		t.Errorf("terminal WorkflowFailed.Persona = %q; want developer", wfp.Persona)
	}

	// Both PersonaFailed events should have been mirrored onto the workflow
	// aggregate (one before the retry, one before the terminal failure),
	// bracketing the retry in a readable order.
	var mirrors, retries int
	for _, e := range events {
		switch e.Type {
		case event.PersonaFailedTracked:
			mirrors++
		case event.WorkflowRetried:
			retries++
		}
	}
	if mirrors != 2 {
		t.Errorf("PersonaFailedTracked mirrors = %d; want 2 (each PersonaFailed must leave a breadcrumb)", mirrors)
	}
	if retries != 1 {
		t.Errorf("WorkflowRetried count = %d; want 1 (only the first failure retries)", retries)
	}
}
