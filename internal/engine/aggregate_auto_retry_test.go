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

// TestAggregate_AutoRetry_AcrossDeveloperDAGs locks the contract that
// idle_timeout auto-retry is workflow-agnostic: ANY built-in DAG with
// `developer` in its Graph must auto-retry, not just workspace-dev.
//
// Added after the 2026-04-18 pr-feedback idle_timeout report
// (rick-feedback-2026-04-18-pr-feedback-idle-timeout.md), where the
// operator observed a developer idle_timeout in pr-feedback and
// hypothesised that the workspace-dev fix had not been applied to the
// pr-feedback code path. The aggregate's maybeAutoRetry is in fact
// workflow-agnostic — it only checks that the failed persona is in the
// Graph — but the original test suite exercised only WorkspaceDevWorkflowDef,
// so nothing in CI proved the claim. This table-driven test pins every
// built-in DAG that ships a developer phase so that a future refactor
// splitting the retry path per-DAG (or a new DAG accidentally excluding
// developer from the auto-retry surface) fails loudly on a single line.
func TestAggregate_AutoRetry_AcrossDeveloperDAGs(t *testing.T) {
	defs := map[string]WorkflowDef{
		"workspace-dev": WorkspaceDevWorkflowDef(),
		"pr-feedback":   PRFeedbackWorkflowDef(),
		"jira-dev":      JiraDevWorkflowDef(),
		"github-dev":    GithubDevWorkflowDef(),
		"ci-fix":        CIFixWorkflowDef(),
		"develop-only":  DevelopOnlyWorkflowDef(),
	}
	for name, def := range defs {
		t.Run(name, func(t *testing.T) {
			if _, ok := def.Graph["developer"]; !ok {
				t.Fatalf("%s: developer missing from Graph — this test's premise is wrong, "+
					"update the defs map if this DAG legitimately no longer has a developer phase", name)
			}

			agg := NewWorkflowAggregate("wf-" + name)
			agg.Status = StatusRunning
			agg.WorkflowDef = &def

			failEnv := event.Envelope{
				Type:          event.PersonaFailed,
				AggregateID:   "wf-" + name,
				CorrelationID: "corr-" + name,
				Payload: event.MustMarshal(event.PersonaFailedPayload{
					Persona:     "developer",
					Error:       "handler developer: backend: claude: idle timeout exceeded (stall=2m0s)",
					FailureKind: event.FailureKindIdleTimeout,
				}),
			}
			events, err := agg.Decide(failEnv)
			if err != nil {
				t.Fatalf("%s: Decide: %v", name, err)
			}
			if len(events) != 1 || events[0].Type != event.WorkflowRetried {
				t.Fatalf("%s: want [WorkflowRetried] (auto-retry must cover this DAG), got %+v",
					name, events)
			}

			var p event.WorkflowRetriedPayload
			if err := json.Unmarshal(events[0].Payload, &p); err != nil {
				t.Fatalf("%s: unmarshal retry payload: %v", name, err)
			}
			if !p.Automatic {
				t.Errorf("%s: Automatic=false; want true so the cap is enforced", name)
			}
			if p.FromPhase != "developer" {
				t.Errorf("%s: FromPhase=%q; want developer", name, p.FromPhase)
			}
			// Every DAG-downstream persona must be invalidated so stale
			// completions don't satisfy the join on re-dispatch.
			for _, d := range def.DownstreamOf("developer") {
				if !slices.Contains(p.InvalidatedPersonas, d) {
					t.Errorf("%s: InvalidatedPersonas missing %q (DAG-downstream of developer)", name, d)
				}
			}
		})
	}
}

// TestAggregate_AutoRetry_PRFeedback_AfterFeedbackAnalyzerVerdict replays
// the exact event sequence from the 2026-04-18 pr-feedback idle_timeout
// report:
//
//	github-pr-fetcher → workspace → feedback-analyzer (produces fail verdict)
//	  → FeedbackGenerated{target:developer, source:feedback-analyzer}
//	  → context-snapshot → developer FAILS with idle_timeout at iteration 0
//
// The pre-developer FeedbackGenerated is pr-feedback-specific: it bumps
// FeedbackCount[developer] to 1 and sets FeedbackPending[feedback-analyzer]
// to "developer" BEFORE the developer even runs. None of this should block
// the idle_timeout auto-retry. This test pins that behaviour so a future
// change to Apply / Decide that re-reads those fields from the feedback
// path can't silently break pr-feedback's retry surface.
func TestAggregate_AutoRetry_PRFeedback_AfterFeedbackAnalyzerVerdict(t *testing.T) {
	agg := NewWorkflowAggregate("wf-prfb-scenario")
	def := PRFeedbackWorkflowDef()
	agg.WorkflowDef = &def
	agg.WorkflowID = def.ID
	agg.MaxIterations = def.MaxIterations
	agg.Status = StatusRunning

	// Everything the aggregate sees from the workflow's perspective
	// before developer runs. Ordering matches what checkJoinCondition
	// would observe in the store.
	pre := []event.Envelope{
		personaCompletedEnv("github-pr-fetcher"),
		personaCompletedEnv("workspace"),
		personaCompletedEnv("feedback-analyzer"),
		feedbackGeneratedEnv("developer", "feedback-analyzer", 1),
		personaCompletedEnv("context-snapshot"),
	}
	for _, e := range pre {
		agg.Apply(e)
	}

	// Sanity-check the pre-developer state so a future change that breaks
	// this setup (e.g., FeedbackGenerated no longer bumping FeedbackCount)
	// surfaces as a clear precondition failure, not a misleading
	// auto-retry assertion.
	if agg.FeedbackCount["developer"] != 1 {
		t.Fatalf("precondition: FeedbackCount[developer]=%d; want 1 after feedback-analyzer verdict",
			agg.FeedbackCount["developer"])
	}
	if agg.FeedbackPending["feedback-analyzer"] != "developer" {
		t.Fatalf("precondition: FeedbackPending[feedback-analyzer]=%q; want developer",
			agg.FeedbackPending["feedback-analyzer"])
	}

	failEnv := event.Envelope{
		Type:          event.PersonaFailed,
		AggregateID:   "wf-prfb-scenario",
		CorrelationID: "corr-prfb-scenario",
		Payload: event.MustMarshal(event.PersonaFailedPayload{
			Persona:     "developer",
			Error:       "handler developer: backend: claude: backend: idle timeout exceeded (stall=2m0s) (after 5m0.025s)",
			FailureKind: event.FailureKindIdleTimeout,
		}),
	}
	events, err := agg.Decide(failEnv)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(events) != 1 || events[0].Type != event.WorkflowRetried {
		var wfp event.WorkflowFailedPayload
		if len(events) == 1 {
			_ = json.Unmarshal(events[0].Payload, &wfp)
		}
		t.Fatalf("want [WorkflowRetried] even after feedback-analyzer verdict state; got %+v (failed reason=%q)",
			events, wfp.Reason)
	}

	// Round-trip through Apply to prove the feedback-path state still
	// lines up cleanly after the retry: the stale FeedbackPending gate
	// for feedback-analyzer must be cleared (developer is in
	// InvalidatedPersonas → the reverse-lookup branch in Apply drops
	// FeedbackPending[feedback-analyzer]) or feedback-analyzer would
	// stay blocked from re-tracking forever.
	agg.Apply(events[0])
	if _, stuck := agg.FeedbackPending["feedback-analyzer"]; stuck {
		t.Error("FeedbackPending[feedback-analyzer] not cleared after auto-retry Apply — " +
			"feedback-analyzer will never be re-trackable, breaking the next feedback iteration")
	}
	if agg.AutoRetries["developer"] != 1 {
		t.Errorf("AutoRetries[developer]=%d; want 1 (retry must consume the budget)",
			agg.AutoRetries["developer"])
	}
	if agg.Status != StatusRunning {
		t.Errorf("Status=%q; want Running so PersonaRunner re-dispatch proceeds", agg.Status)
	}
}

// personaCompletedEnv builds a minimal PersonaCompleted envelope suitable
// for feeding through Apply — only fields the aggregate reads are set.
func personaCompletedEnv(persona string) event.Envelope {
	return event.Envelope{
		Type: event.PersonaCompleted,
		Payload: event.MustMarshal(event.PersonaCompletedPayload{
			Persona: persona,
		}),
	}
}

// feedbackGeneratedEnv builds the aggregate-emitted FeedbackGenerated
// shape (phase name already resolved to a persona — this matches what
// decideVerdictRendered produces after ResolvePhase).
func feedbackGeneratedEnv(target, source string, iteration int) event.Envelope {
	return event.Envelope{
		Type: event.FeedbackGenerated,
		Payload: event.MustMarshal(event.FeedbackGeneratedPayload{
			TargetPersona: target,
			SourcePersona: source,
			Iteration:   iteration,
		}),
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

