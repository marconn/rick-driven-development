package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// TestPersonasToInvalidateFor_BarrierSiblingExpansion is the core regression
// for the silent wedge on workflow 3555adef-bd2b-4210-891e-3ec2a82dba10
// (pr-feedback, 2026-05-15). The operator retried from_phase=reviewer; the
// resulting InvalidatedPersonas did NOT include qa, so qa never re-rendered
// for the new dev_trigger_id, the review-consolidator's join was permanently
// unsatisfiable, and the workflow sat for 1h50m before manual cancellation.
//
// Under a sync-feedback barrier (SynchronousFeedback=true), retrying any
// member of ConsolidatedReviewers must invalidate ALL members so the
// consolidator's dev_trigger_id-keyed join can re-pair on the next round.
func TestPersonasToInvalidateFor_BarrierSiblingExpansion(t *testing.T) {
	def := WorkspaceDevWorkflowDef()

	t.Run("from_reviewer_includes_qa", func(t *testing.T) {
		got := def.PersonasToInvalidateFor("reviewer")
		if !slices.Contains(got, "qa") {
			t.Fatalf("PersonasToInvalidateFor(reviewer) = %v; MUST include qa — "+
				"this is the 3555adef-... wedge regression", got)
		}
		// Downstream consumers still get invalidated (existing contract).
		for _, want := range []string{"reviewer", "review-consolidator", "quality-gate", "committer"} {
			if !slices.Contains(got, want) {
				t.Errorf("PersonasToInvalidateFor(reviewer) = %v; missing %q (downstream contract)", got, want)
			}
		}
	})

	t.Run("from_qa_includes_reviewer", func(t *testing.T) {
		got := def.PersonasToInvalidateFor("qa")
		if !slices.Contains(got, "reviewer") {
			t.Fatalf("PersonasToInvalidateFor(qa) = %v; MUST include reviewer (symmetric barrier)", got)
		}
	})

	t.Run("from_developer_does_not_drag_barrier_siblings", func(t *testing.T) {
		// developer is NOT a member of ConsolidatedReviewers. Retrying from
		// developer must NOT invalidate reviewer or qa as "barrier peers" —
		// they ARE downstream of developer, so they'll be invalidated via
		// the normal DownstreamOf path, but the expansion logic should
		// emit them once, not twice, and should not over-invalidate any
		// non-DAG-downstream personas.
		got := def.PersonasToInvalidateFor("developer")
		seen := map[string]int{}
		for _, p := range got {
			seen[p]++
		}
		for p, count := range seen {
			if count > 1 {
				t.Errorf("PersonasToInvalidateFor(developer) duplicated %q (%d times); set must be deduped", p, count)
			}
		}
		// reviewer and qa ARE downstream of developer — they should still be present.
		for _, want := range []string{"reviewer", "qa"} {
			if !slices.Contains(got, want) {
				t.Errorf("PersonasToInvalidateFor(developer) missing %q (it IS downstream)", got)
			}
		}
	})

	t.Run("from_consolidator_matches_downstream_only", func(t *testing.T) {
		// review-consolidator is NOT a member of ConsolidatedReviewers; it
		// is the join CONSUMER, not a join input. Retrying from the
		// consolidator should behave exactly like DownstreamOf — no extra
		// sibling expansion.
		downstream := def.DownstreamOf("review-consolidator")
		got := def.PersonasToInvalidateFor("review-consolidator")
		if len(got) != len(downstream) {
			t.Errorf("PersonasToInvalidateFor(review-consolidator) = %v (len %d); want same length as DownstreamOf = %v (len %d)",
				got, len(got), downstream, len(downstream))
		}
		for _, p := range downstream {
			if !slices.Contains(got, p) {
				t.Errorf("PersonasToInvalidateFor(review-consolidator) missing downstream %q", p)
			}
		}
	})
}

// TestPersonasToInvalidateFor_NonBarrierWorkflowIsIdentity guards the
// no-change-for-non-barrier-workflows promise from the Design Brief. Any
// DAG without SynchronousFeedback must return exactly DownstreamOf(p) so
// existing callers (develop-only, plan-btu, etc.) see zero behavioral
// change.
func TestPersonasToInvalidateFor_NonBarrierWorkflowIsIdentity(t *testing.T) {
	defs := map[string]WorkflowDef{
		"develop-only": DevelopOnlyWorkflowDef(),
	}
	for name, def := range defs {
		t.Run(name, func(t *testing.T) {
			if def.SynchronousFeedback {
				t.Fatalf("%s: test premise broken — this DAG now has SynchronousFeedback=true, "+
					"move it to the barrier-expansion test or pick a different non-barrier DAG", name)
			}
			for handler := range def.Graph {
				downstream := def.DownstreamOf(handler)
				slices.Sort(downstream)
				expanded := def.PersonasToInvalidateFor(handler)
				slices.Sort(expanded)
				if !slices.Equal(downstream, expanded) {
					t.Errorf("%s: PersonasToInvalidateFor(%q) = %v; want = DownstreamOf = %v (non-barrier identity)",
						name, handler, expanded, downstream)
				}
			}
		})
	}
}

// TestAggregate_AutoRetry_BarrierSiblingInvalidation pins the engine-side
// auto-retry path (idle_timeout on a barrier member) so it picks up the
// sibling expansion. Without this, an idle_timeout on reviewer would
// auto-retry only reviewer, leaving qa's verdict stale and recreating the
// same wedge — but via the auto-retry path instead of the manual MCP path.
func TestAggregate_AutoRetry_BarrierSiblingInvalidation(t *testing.T) {
	agg := NewWorkflowAggregate("wf-barrier-auto")
	agg.Status = StatusRunning
	def := WorkspaceDevWorkflowDef()
	agg.WorkflowDef = &def

	failEnv := event.Envelope{
		Type:          event.PersonaFailed,
		AggregateID:   "wf-barrier-auto",
		CorrelationID: "corr-barrier-auto",
		Payload: event.MustMarshal(event.PersonaFailedPayload{
			Persona:     "reviewer",
			Error:       "backend: claude: idle timeout exceeded (stall=2m0s)",
			FailureKind: event.FailureKindIdleTimeout,
		}),
	}

	events, err := agg.Decide(failEnv)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(events) != 1 || events[0].Type != event.WorkflowRetried {
		t.Fatalf("want [WorkflowRetried], got %+v", events)
	}
	var p event.WorkflowRetriedPayload
	if err := json.Unmarshal(events[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !slices.Contains(p.InvalidatedPersonas, "qa") {
		t.Errorf("auto-retry on reviewer InvalidatedPersonas = %v; MUST include qa to avoid recreating "+
			"the 3555adef-... wedge via the auto-retry path", p.InvalidatedPersonas)
	}
}

// TestAggregate_RateLimit_PausesNotFails covers the Bug 2 happy path. A
// PersonaFailed with FailureKind=rate_limited must produce WorkflowPaused
// (not WorkflowFailed) with RetryFromPhase set so resume re-dispatches the
// persona via the barrier-aware retry path. Pause is non-terminal, so
// subscribeTerminalEvents will NOT cancel the per-correlation context —
// the parallel sibling under the sync-feedback barrier keeps running.
func TestAggregate_RateLimit_PausesNotFails(t *testing.T) {
	agg := NewWorkflowAggregate("wf-rl-pause")
	agg.Status = StatusRunning
	def := WorkspaceDevWorkflowDef()
	agg.WorkflowDef = &def

	failEnv := event.Envelope{
		Type:          event.PersonaFailed,
		AggregateID:   "wf-rl-pause",
		CorrelationID: "corr-rl-pause",
		Payload: event.MustMarshal(event.PersonaFailedPayload{
			Persona:     "reviewer",
			Error:       "backend: claude: exit status 1 (after 2.65s): You've hit your limit · resets 4:50pm (America/Costa_Rica)",
			FailureKind: event.FailureKindRateLimited,
			Backend:     "claude",
			Stderr:      "[no stderr; stdout tail]\nYou've hit your limit · resets 4:50pm (America/Costa_Rica)",
		}),
	}

	events, err := agg.Decide(failEnv)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(events) != 1 || events[0].Type != event.WorkflowPaused {
		t.Fatalf("rate-limited must pause, got %+v", events)
	}
	var p event.WorkflowPausedPayload
	if err := json.Unmarshal(events[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.RetryFromPhase != "reviewer" {
		t.Errorf("RetryFromPhase = %q; want reviewer (resume must know what to re-dispatch)", p.RetryFromPhase)
	}
	if p.RateLimitBackend != "claude" {
		t.Errorf("RateLimitBackend = %q; want claude", p.RateLimitBackend)
	}
	if p.Source != "auto:rate_limited" {
		t.Errorf("Source = %q; want auto:rate_limited (operators filter on this)", p.Source)
	}
	if !strings.Contains(p.RateLimitResetHint, "4:50pm") {
		t.Errorf("RateLimitResetHint = %q; want it to carry the reset window for the operator", p.RateLimitResetHint)
	}
}

// TestAggregate_RateLimit_KillSwitchFallsThroughToFailed covers the
// RICK_RATE_LIMIT_AUTOPAUSE=0 revert lever. Operators must be able to
// disable the pause behavior at runtime (e.g. if stderr-pattern matching
// produces a false positive against a real fatal crash) without
// redeploying. Restores the pre-fix WorkflowFailed semantics exactly.
func TestAggregate_RateLimit_KillSwitchFallsThroughToFailed(t *testing.T) {
	t.Setenv(rateLimitAutopauseEnvVar, "0")

	agg := NewWorkflowAggregate("wf-rl-killswitch")
	agg.Status = StatusRunning
	def := WorkspaceDevWorkflowDef()
	agg.WorkflowDef = &def

	failEnv := event.Envelope{
		Type:          event.PersonaFailed,
		AggregateID:   "wf-rl-killswitch",
		CorrelationID: "corr-rl-killswitch",
		Payload: event.MustMarshal(event.PersonaFailedPayload{
			Persona:     "reviewer",
			Error:       "backend: claude: hit your limit",
			FailureKind: event.FailureKindRateLimited,
			Backend:     "claude",
			Stderr:      "You've hit your limit · resets later",
		}),
	}

	events, err := agg.Decide(failEnv)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(events) != 1 || events[0].Type != event.WorkflowFailed {
		t.Fatalf("kill switch engaged: want [WorkflowFailed] (legacy behavior), got %+v", events)
	}
}

// TestAggregate_ResumeAfterRateLimitPause_EmitsBarrierAwareRetry is the
// resume-path counterpart to TestAggregate_RateLimit_PausesNotFails. When
// an operator (or future scheduled resumer) calls rick_resume on a
// rate-limit pause, the aggregate must emit WorkflowRetried with the
// barrier-aware InvalidatedPersonas — otherwise resume re-creates the
// silent wedge the manual-retry path used to produce.
func TestAggregate_ResumeAfterRateLimitPause_EmitsBarrierAwareRetry(t *testing.T) {
	agg := NewWorkflowAggregate("wf-rl-resume")
	def := WorkspaceDevWorkflowDef()
	agg.WorkflowDef = &def
	agg.WorkflowID = def.ID

	// Replay: Paused with RetryFromPhase=reviewer.
	pauseEnv := event.New(event.WorkflowPaused, 1, event.MustMarshal(event.WorkflowPausedPayload{
		Reason:           "rate_limited: reviewer on claude — resume after limit window",
		Source:           "auto:rate_limited",
		RetryFromPhase:   "reviewer",
		RateLimitBackend: "claude",
	})).WithAggregate("wf-rl-resume", 10)
	agg.Apply(pauseEnv)

	if agg.PendingResumeRetryFromPhase != "reviewer" {
		t.Fatalf("Apply did not fold RetryFromPhase into PendingResumeRetryFromPhase, got %q",
			agg.PendingResumeRetryFromPhase)
	}

	// Decide(WorkflowResumed) must emit WorkflowRetried with sibling
	// expansion.
	resumeEnv := event.New(event.WorkflowResumed, 1, event.MustMarshal(event.WorkflowResumedPayload{
		Reason: "operator resume after rate-limit window",
	})).WithAggregate("wf-rl-resume", 11).WithCorrelation("corr-rl-resume")
	// Resumed status must be set before Decide so the function sees the
	// transition (decideWorkflowResumed runs regardless of status, but
	// PersonaRunner won't re-dispatch on a non-Running aggregate).
	agg.Status = StatusRunning

	events, err := agg.Decide(resumeEnv)
	if err != nil {
		t.Fatalf("Decide(WorkflowResumed): %v", err)
	}
	if len(events) != 1 || events[0].Type != event.WorkflowRetried {
		t.Fatalf("resume from rate-limit pause: want [WorkflowRetried], got %+v", events)
	}
	var p event.WorkflowRetriedPayload
	if err := json.Unmarshal(events[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.FromPhase != "reviewer" {
		t.Errorf("FromPhase = %q; want reviewer (resume must dispatch the rate-limited persona)", p.FromPhase)
	}
	if !slices.Contains(p.InvalidatedPersonas, "qa") {
		t.Errorf("InvalidatedPersonas = %v; MUST include qa via barrier expansion — "+
			"this is the precise contract that makes resume work post-rate-limit", p.InvalidatedPersonas)
	}
	if p.Automatic {
		t.Errorf("Automatic = true; resume should be operator-gated (Automatic=false) so it does not "+
			"consume the auto-retry budget meant for transient idle_timeouts")
	}
}

// TestExtractRateLimitResetHint covers the parser used by maybeRateLimitPause
// to surface the reset window in WorkflowPausedPayload.RateLimitResetHint.
// Best-effort by design — empty result is acceptable for unrecognised shapes,
// but the canonical claude format MUST parse so operators see when to resume.
func TestExtractRateLimitResetHint(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "claude_canonical_with_timezone",
			in:   "[no stderr; stdout tail]\nYou've hit your limit · resets 4:50pm (America/Costa_Rica)",
			want: "4:50pm (America/Costa_Rica)",
		},
		{
			name: "claude_alt_at_utc",
			in:   "You've hit your limit · resets at 16:50 UTC",
			want: "at 16:50 UTC",
		},
		{
			name: "no_reset_marker",
			in:   "rate_limit_exceeded: try again later",
			want: "",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "trailing_newline_stripped",
			in:   "You've hit your limit · resets 4:50pm\nadditional log noise",
			want: "4:50pm",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractRateLimitResetHint(tc.in)
			if got != tc.want {
				t.Errorf("extractRateLimitResetHint(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
