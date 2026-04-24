package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// WorkflowStatus represents the current state of a workflow.
type WorkflowStatus string

const (
	StatusRequested WorkflowStatus = "requested"
	StatusRunning   WorkflowStatus = "running"
	StatusCompleted WorkflowStatus = "completed"
	StatusFailed    WorkflowStatus = "failed"
	StatusCancelled WorkflowStatus = "cancelled"
	StatusPaused    WorkflowStatus = "paused"
)

// WorkflowAggregate is the domain aggregate for a workflow run.
// It is reconstituted from the event store via Apply() and produces
// new events via Decide().
type WorkflowAggregate struct {
	ID                string
	Version           int
	Status            WorkflowStatus
	WorkflowDef       *WorkflowDef    // lifecycle: which personas must complete
	CompletedPersonas map[string]bool   // set of persona names that have completed
	// SkippedPersonas records required personas whose failure was absorbed
	// under WorkflowDef.PartialReviewOnFailure instead of failing the
	// workflow. These are ALSO added to CompletedPersonas so the completion
	// check fires naturally; the separate set exists for observability and
	// downstream-consumer reporting (e.g., pr-consolidator listing which
	// reviewers were skipped in the posted PR review body).
	SkippedPersonas   map[string]bool
	FeedbackCount     map[string]int    // tracks feedback iterations per target persona
	FeedbackPending   map[string]string // persona → target that must re-complete before this persona can be re-tracked (stale event guard)
	// AutoRetries counts how many times the engine has emitted an automatic
	// WorkflowRetried (from decidePersonaFailed's transient-failure branch)
	// for each persona. Capped at MaxAutoRetriesPerPersona so a deterministic
	// failure can't loop forever. Operator-initiated retries via
	// rick_retry_workflow don't increment this counter.
	AutoRetries       map[string]int
	TokensUsed        int
	TokenBudget       int
	MaxIterations     int
	Prompt            string
	WorkflowID        string
	Source            string
	Ticket            string
	// LastVerdictFingerprint maps "<source_phase>|<target_persona>" to the
	// fingerprint (sorted hash of Issues[].Description) of the most recent
	// failing VerdictRendered for that pair. Used by decideVerdictRendered to
	// escalate a loop when two consecutive failures are byte-identical — a
	// strong signal that the developer can't fix the cited issues (test
	// flakes, env drift, pre-existing main-branch failures). Keyed per
	// source so a flaky quality-gate can't mask an independent reviewer
	// failure, and vice versa.
	LastVerdictFingerprint map[string]string
}

// MaxAutoRetriesPerPersona caps how many automatic retries the engine
// will emit for a single persona on a single workflow. One retry is
// enough to rescue transient failures (the feedback doc's core request)
// while stopping a deterministic crash from spinning indefinitely.
const MaxAutoRetriesPerPersona = 1

// NewWorkflowAggregate creates a new empty aggregate ready for event replay.
func NewWorkflowAggregate(id string) *WorkflowAggregate {
	return &WorkflowAggregate{
		ID:                     id,
		Status:                 StatusRequested,
		CompletedPersonas:      make(map[string]bool),
		SkippedPersonas:        make(map[string]bool),
		FeedbackCount:          make(map[string]int),
		FeedbackPending:        make(map[string]string),
		AutoRetries:            make(map[string]int),
		LastVerdictFingerprint: make(map[string]string),
		MaxIterations:          3,
	}
}

// Apply replays a single event to rebuild aggregate state.
// Apply must be side-effect-free — it only mutates in-memory state.
func (w *WorkflowAggregate) Apply(env event.Envelope) {
	w.Version = env.Version

	switch env.Type {
	case event.WorkflowRequested:
		var p event.WorkflowRequestedPayload
		_ = json.Unmarshal(env.Payload, &p)
		w.Prompt = p.Prompt
		w.WorkflowID = p.WorkflowID
		w.Source = p.Source
		w.Ticket = p.Ticket
		w.Status = StatusRequested

	case event.WorkflowCompleted:
		w.Status = StatusCompleted

	case event.WorkflowFailed:
		w.Status = StatusFailed

	case event.WorkflowCancelled:
		w.Status = StatusCancelled

	case event.WorkflowPaused:
		w.Status = StatusPaused

	case event.WorkflowResumed:
		w.Status = StatusRunning

	case event.WorkflowRetried:
		// Drops CompletedPersonas for every persona the emitter marked as
		// invalidated (FromPhase + DAG-downstream at emit time). Kept in the
		// payload so replay doesn't depend on the live WorkflowDef registry,
		// which isn't attached until after Apply runs.
		var p event.WorkflowRetriedPayload
		_ = json.Unmarshal(env.Payload, &p)
		for _, persona := range p.InvalidatedPersonas {
			delete(w.CompletedPersonas, persona)
			delete(w.FeedbackPending, persona)
			for src, target := range w.FeedbackPending {
				if target == persona {
					delete(w.FeedbackPending, src)
				}
			}
		}
		if p.Automatic && p.FromPhase != "" {
			if w.AutoRetries == nil {
				w.AutoRetries = make(map[string]int)
			}
			w.AutoRetries[p.FromPhase]++
		}
		w.Status = StatusRunning

	case event.PersonaCompleted, event.PersonaTracked:
		// PersonaTracked is the internal tracking copy stored by the engine on the
		// workflow aggregate; PersonaCompleted is the original from PersonaRunner.
		// Both carry PersonaCompletedPayload and must update the same state.
		var p event.PersonaCompletedPayload
		_ = json.Unmarshal(env.Payload, &p)
		w.CompletedPersonas[p.Persona] = true
		// Clear feedback gates whose target just re-completed.
		for persona, target := range w.FeedbackPending {
			if target == p.Persona {
				delete(w.FeedbackPending, persona)
			}
		}

	case event.PersonaFailedTracked:
		// Under PartialReviewOnFailure, a required-persona failure is absorbed
		// as a skip rather than failing the workflow. Record it in both
		// CompletedPersonas (so decidePersonaCompleted's completion check
		// fires naturally when the last reviewer finishes) and SkippedPersonas
		// (for observability + consolidator reporting). The mirror is only
		// emitted by engine.go for required personas on a running workflow,
		// so no further guards needed here beyond the workflow-def toggle.
		if w.WorkflowDef != nil && w.WorkflowDef.PartialReviewOnFailure {
			var p event.PersonaFailedPayload
			_ = json.Unmarshal(env.Payload, &p)
			if p.Persona != "" {
				w.CompletedPersonas[p.Persona] = true
				if w.SkippedPersonas == nil {
					w.SkippedPersonas = make(map[string]bool)
				}
				w.SkippedPersonas[p.Persona] = true
			}
		}

	case event.AIResponseReceived:
		var p event.AIResponsePayload
		_ = json.Unmarshal(env.Payload, &p)
		w.TokensUsed += p.TokensUsed

	case event.FeedbackGenerated:
		var p event.FeedbackGeneratedPayload
		_ = json.Unmarshal(env.Payload, &p)
		w.FeedbackCount[p.TargetPhase]++
		// Reset completed status — personas need to re-run after feedback.
		delete(w.CompletedPersonas, p.TargetPhase)
		if p.SourcePhase != "" {
			delete(w.CompletedPersonas, p.SourcePhase)
			// Gate: source persona can't be re-tracked until target re-completes.
			// This prevents stale PersonaCompleted events (already in the FIFO)
			// from prematurely re-tracking after feedback clears them.
			w.FeedbackPending[p.SourcePhase] = p.TargetPhase
		}

	case event.VerdictRendered:
		// Track fingerprint of failing verdicts so decideVerdictRendered can
		// detect byte-identical failures across iterations. Apply is
		// side-effect-free: we just record state so the next decision round
		// can compare. Pass verdicts clear the slot so a transient fail
		// followed by a pass doesn't trigger dedup on an unrelated later
		// regression.
		var vp event.VerdictPayload
		_ = json.Unmarshal(env.Payload, &vp)
		if w.WorkflowDef == nil {
			break
		}
		target := w.WorkflowDef.ResolvePhase(vp.Phase)
		source := w.WorkflowDef.ResolvePhase(vp.SourcePhase)
		if source == "" || target == "" {
			break
		}
		key := source + "|" + target
		if w.LastVerdictFingerprint == nil {
			w.LastVerdictFingerprint = make(map[string]string)
		}
		if vp.Outcome == event.VerdictFail {
			w.LastVerdictFingerprint[key] = verdictFingerprint(vp)
		} else {
			delete(w.LastVerdictFingerprint, key)
		}

	default:
		// Workflow-scoped start events: workflow.started.<id>
		if event.IsWorkflowStarted(env.Type) {
			w.Status = StatusRunning
		}
	}
	// Unrecognized event types (Phase*, Verdict, PersonaFailed, etc.) are no-ops.
	// Version is tracked by the w.Version = env.Version at top.
}

// isStaleAfterFeedback returns true if the persona was cleared by feedback and
// the feedback target hasn't re-completed yet. This guards against stale
// PersonaCompleted events that were already in the Engine's FIFO channel when
// FeedbackGenerated was emitted.
func (w *WorkflowAggregate) isStaleAfterFeedback(persona string) bool {
	target, ok := w.FeedbackPending[persona]
	return ok && !w.CompletedPersonas[target]
}

// isRequiredPersona returns true if the persona is in the workflow's Required list.
func (w *WorkflowAggregate) isRequiredPersona(persona string) bool {
	if w.WorkflowDef == nil {
		return false
	}
	for _, req := range w.WorkflowDef.Required {
		if req == persona {
			return true
		}
	}
	return false
}

// Decide produces new events based on the current state and incoming event.
// This is the business logic core — it decides what happens next.
func (w *WorkflowAggregate) Decide(env event.Envelope) ([]event.Envelope, error) {
	switch env.Type {
	case event.WorkflowRequested:
		return w.decideWorkflowRequested(env)
	case event.PersonaCompleted:
		return w.decidePersonaCompleted(env)
	case event.PersonaFailed:
		return w.decidePersonaFailed(env)
	case event.VerdictRendered:
		return w.decideVerdictRendered(env)
	case event.TokenBudgetExceeded:
		return w.decideTokenBudgetExceeded(env)
	case event.WorkflowResumed:
		return w.decideWorkflowResumed(env)
	case event.WorkflowRetried:
		// State reset happens in Apply; PersonaRunner re-dispatches FromPhase
		// asynchronously via its own subscription. Nothing for Decide to emit.
		return nil, nil
	case event.HintEmitted:
		return w.decideHintEmitted(env)
	case event.HintRejected:
		return w.decideHintRejected(env)
	default:
		return nil, nil
	}
}

func (w *WorkflowAggregate) decideWorkflowRequested(env event.Envelope) ([]event.Envelope, error) {
	if w.WorkflowDef == nil {
		return nil, fmt.Errorf("engine: workflow def not set on aggregate %s", w.ID)
	}
	// Guard: if the workflow was cancelled before the Engine processed
	// WorkflowRequested, don't emit WorkflowStarted.
	if w.Status != StatusRequested {
		return nil, nil
	}
	payload := event.MustMarshal(event.WorkflowStartedPayload{
		WorkflowID: w.WorkflowID,
		Phases:     w.WorkflowDef.Required,
		Source:     w.Source,
		Ticket:     w.Ticket,
		Prompt:     w.Prompt,
	})
	return []event.Envelope{
		event.New(event.WorkflowStartedFor(w.WorkflowID), 1, payload).
			WithAggregate(w.ID, w.Version+1).
			WithCausation(env.ID).
			WithCorrelation(env.CorrelationID).
			WithSource("engine:aggregate"),
	}, nil
}

func (w *WorkflowAggregate) decidePersonaCompleted(env event.Envelope) ([]event.Envelope, error) {
	if evts := w.maybeEmitWorkflowCompleted(env); evts != nil {
		return evts, nil
	}
	return nil, nil
}

// maybeEmitWorkflowCompleted returns a WorkflowCompleted envelope if every
// required persona is marked complete (including failures absorbed as skips
// under PartialReviewOnFailure), otherwise nil. Shared by the normal
// completion path (decidePersonaCompleted) and the partial-review failure
// path (decidePersonaFailed) so both check the same condition.
func (w *WorkflowAggregate) maybeEmitWorkflowCompleted(env event.Envelope) []event.Envelope {
	if w.WorkflowDef == nil || w.Status != StatusRunning {
		return nil
	}
	for _, req := range w.WorkflowDef.Required {
		if !w.CompletedPersonas[req] {
			return nil
		}
	}
	result := "all required personas completed"
	if len(w.SkippedPersonas) > 0 {
		result = fmt.Sprintf("completed with %d skipped persona(s)", len(w.SkippedPersonas))
	}
	payload := event.MustMarshal(event.WorkflowCompletedPayload{
		Result: result,
	})
	return []event.Envelope{
		event.New(event.WorkflowCompleted, 1, payload).
			WithAggregate(w.ID, w.Version+1).
			WithCausation(env.ID).
			WithCorrelation(env.CorrelationID).
			WithSource("engine:aggregate"),
	}
}

func (w *WorkflowAggregate) decidePersonaFailed(env event.Envelope) ([]event.Envelope, error) {
	if w.WorkflowDef == nil || w.Status != StatusRunning {
		return nil, nil
	}

	var p event.PersonaFailedPayload
	_ = json.Unmarshal(env.Payload, &p)

	// Only fail the workflow if the failed persona is required.
	// Non-required handlers (before-hooks, enrichers) failing shouldn't
	// kill the workflow — they're supplementary.
	if !w.isRequiredPersona(p.Persona) {
		return nil, nil
	}

	// Transient-failure auto-retry: when the backend subprocess went
	// silent (idle_timeout), a single retry often succeeds — the
	// feedback doc's top operator complaint. We emit WorkflowRetried
	// instead of WorkflowFailed so the existing retry machinery
	// (PersonaRunner re-dispatch, DAG-downstream invalidation) runs
	// unchanged. Capped per persona so a deterministic crash with the
	// same symptom shape can't loop forever.
	if retry, ok := w.maybeAutoRetry(env, p); ok {
		return []event.Envelope{retry}, nil
	}

	// Partial-review workflows (e.g. pr-review) absorb single-reviewer
	// failures as skips instead of failing the workflow. The Apply case
	// for PersonaFailedTracked already marked this persona as
	// completed+skipped, so if every required persona is now done, emit
	// WorkflowCompleted — otherwise wait for siblings (no event). This
	// eliminates the cascade-cancellation that killed pr-hygiene,
	// pr-concurrency, pr-idempotency when pr-data crashed on
	// hulilabs/huli#802 (correlation 154ce63a-42d3-41b0-b008-b8c083e538bc,
	// 2026-04-24).
	if w.WorkflowDef.PartialReviewOnFailure {
		return w.maybeEmitWorkflowCompleted(env), nil
	}

	// Surface FailureKind / Backend / Stderr from the PersonaFailed payload so
	// operators can tell idle_timeout from handler_error (and attribute to a
	// specific CLI) directly from rick_workflow_status / the WorkflowFailed
	// event — without replaying the persona-scoped aggregate. Prior to this,
	// WorkflowFailed carried only a reason string shaped like
	// "persona developer failed: handler developer: backend: claude: backend:
	// idle timeout exceeded (stall=2m0s) (after 2m0s)" — readable but not
	// machine-parsable, and the stderr tail was lost entirely.
	payload := event.MustMarshal(event.WorkflowFailedPayload{
		Reason:      fmt.Sprintf("persona %s failed: %s", p.Persona, p.Error),
		Phase:       p.Persona,
		FailureKind: p.FailureKind,
		Backend:     p.Backend,
		Stderr:      p.Stderr,
	})
	return []event.Envelope{
		event.New(event.WorkflowFailed, 1, payload).
			WithAggregate(w.ID, w.Version+1).
			WithCausation(env.ID).
			WithCorrelation(env.CorrelationID).
			WithSource("engine:aggregate"),
	}, nil
}

// maybeAutoRetry returns a WorkflowRetried envelope when the failed
// persona's FailureKind is a transient shape AND the per-persona
// auto-retry cap hasn't been hit yet. Returns (_, false) when the
// caller should fall through to WorkflowFailed.
//
// Transient kinds: FailureKindIdleTimeout. WallTimeout is deliberately
// excluded — a 20-minute wall-timeout is less likely to succeed on
// immediate retry than a 2-minute idle stall. Cancelled and
// HandlerError are skipped because they indicate operator intent or a
// code-level bug, neither of which retries help.
func (w *WorkflowAggregate) maybeAutoRetry(env event.Envelope, p event.PersonaFailedPayload) (event.Envelope, bool) {
	if p.FailureKind != event.FailureKindIdleTimeout {
		return event.Envelope{}, false
	}
	if w.AutoRetries[p.Persona] >= MaxAutoRetriesPerPersona {
		return event.Envelope{}, false
	}
	// Must be in the workflow's DAG — otherwise DownstreamOf returns an
	// empty set and the retry does nothing.
	if _, inGraph := w.WorkflowDef.Graph[p.Persona]; !inGraph {
		return event.Envelope{}, false
	}

	invalidated := w.WorkflowDef.DownstreamOf(p.Persona)
	reason := fmt.Sprintf("engine: auto-retry on transient %s (attempt %d/%d)",
		p.FailureKind, w.AutoRetries[p.Persona]+1, MaxAutoRetriesPerPersona)

	retry := event.New(event.WorkflowRetried, 1, event.MustMarshal(event.WorkflowRetriedPayload{
		FromPhase:           p.Persona,
		InvalidatedPersonas: invalidated,
		Reason:              reason,
		Automatic:           true,
	})).
		WithAggregate(w.ID, w.Version+1).
		WithCausation(env.ID).
		WithCorrelation(env.CorrelationID).
		WithSource("engine:aggregate")
	return retry, true
}

func (w *WorkflowAggregate) decideVerdictRendered(env event.Envelope) ([]event.Envelope, error) {
	if w.Status != StatusRunning {
		return nil, nil // no feedback while paused/cancelled/completed
	}

	var v event.VerdictPayload
	_ = json.Unmarshal(env.Payload, &v)

	if v.Outcome != event.VerdictFail {
		return nil, nil
	}

	// Resolve verdict phase name to persona name. Handlers use phase verbs
	// ("develop", "review") in verdicts while workflow defs use handler names
	// ("developer", "reviewer") in their Required list.
	targetPersona := v.Phase
	sourcePersona := v.SourcePhase
	if w.WorkflowDef != nil {
		targetPersona = w.WorkflowDef.ResolvePhase(v.Phase)
		sourcePersona = w.WorkflowDef.ResolvePhase(v.SourcePhase)
	}

	// Only generate feedback if the target phase is a required persona in
	// this workflow. Review-only workflows (pr-review) emit verdicts as
	// output, not as feedback gates — generating FeedbackGenerated for a
	// non-existent phase permanently corrupts CompletedPersonas because
	// the FeedbackPending gate can never be cleared.
	if !w.isRequiredPersona(targetPersona) {
		return nil, nil
	}

	// Advisory verdicts (e.g., quality-gate with GitHub CI green on same SHA)
	// signal that the source doesn't trust its own failure and wants operator
	// review rather than a retry. Escalate immediately — no developer burn on
	// what the gate itself flagged as likely-flake.
	if v.Advisory {
		return w.escalateVerdict(env, fmt.Sprintf(
			"%s emitted advisory failure — %s (pausing for operator review instead of re-triggering %s)",
			sourcePersona, v.Summary, targetPersona)), nil
	}

	// Identical-failure dedup: if this verdict's fingerprint matches the one
	// stored on the aggregate for this (source, target) pair, we're in a
	// non-convergent loop (pre-existing flake, env drift, regression the
	// developer can't fix). Escalate on the second identical failure instead
	// of re-triggering the developer — the common case burns ~3.5M tokens
	// per wasted iteration on docs-only PRs. Gated on iteration >= 2 so the
	// first retry is always granted: one legitimate retry after a transient
	// dip is cheaper than a false escalation.
	fpKey := sourcePersona + "|" + targetPersona
	if w.FeedbackCount[targetPersona] >= 1 && w.LastVerdictFingerprint[fpKey] != "" &&
		w.LastVerdictFingerprint[fpKey] == verdictFingerprint(v) {
		return w.escalateVerdict(env, fmt.Sprintf(
			"%s failed twice with byte-identical verdict — not converging (last summary: %q)",
			sourcePersona, v.Summary)), nil
	}

	iteration := w.FeedbackCount[targetPersona] + 1
	if iteration > w.MaxIterations {
		// Escalate to operator (pause) or hard fail depending on workflow config
		if w.WorkflowDef != nil && w.WorkflowDef.EscalateOnMaxIter {
			return w.escalateVerdict(env, fmt.Sprintf(
				"max iterations (%d) reached for %s — escalated to operator", w.MaxIterations, targetPersona)), nil
		}
		payload := event.MustMarshal(event.WorkflowFailedPayload{
			Reason: fmt.Sprintf("max iterations (%d) reached for %s", w.MaxIterations, targetPersona),
			Phase:  targetPersona,
		})
		return []event.Envelope{
			event.New(event.WorkflowFailed, 1, payload).
				WithAggregate(w.ID, w.Version+1).
				WithCausation(env.ID).
				WithCorrelation(env.CorrelationID).
				WithSource("engine:aggregate"),
		}, nil
	}

	fbPayload := event.MustMarshal(event.FeedbackGeneratedPayload{
		TargetPhase: targetPersona,
		SourcePhase: sourcePersona,
		Iteration:   iteration,
		Issues:      v.Issues,
		Summary:     v.Summary,
	})
	return []event.Envelope{
		event.New(event.FeedbackGenerated, 1, fbPayload).
			WithAggregate(w.ID, w.Version+1).
			WithCausation(env.ID).
			WithCorrelation(env.CorrelationID).
			WithSource("engine:aggregate"),
	}, nil
}

// escalateVerdict is a small helper for emitting WorkflowPaused from the
// three decideVerdictRendered escape hatches (advisory / identical /
// max-iter). Keeps the payload construction in one place so operators see
// a consistent Source tag for every auto-escalation.
func (w *WorkflowAggregate) escalateVerdict(env event.Envelope, reason string) []event.Envelope {
	payload := event.MustMarshal(event.WorkflowPausedPayload{
		Reason: reason,
		Source: "engine:auto-escalation",
	})
	return []event.Envelope{
		event.New(event.WorkflowPaused, 1, payload).
			WithAggregate(w.ID, w.Version+1).
			WithCausation(env.ID).
			WithCorrelation(env.CorrelationID).
			WithSource("engine:aggregate"),
	}
}

// verdictFingerprint produces a stable hash of the failure-identifying
// fields of a VerdictPayload — summary + sorted issue descriptions. Issues
// are sorted so pass-order-dependent re-sequencing (rare but possible if
// the source persona streams them) doesn't invalidate dedup. Omits Severity,
// Category, File, Line deliberately: the description itself carries them
// for human-authored verdicts and including them would over-specify the
// fingerprint and miss near-identical retries.
func verdictFingerprint(v event.VerdictPayload) string {
	descs := make([]string, 0, len(v.Issues))
	for _, iss := range v.Issues {
		descs = append(descs, iss.Description)
	}
	sort.Strings(descs)
	hasher := sha256.New()
	hasher.Write([]byte(v.Summary))
	hasher.Write([]byte{0})
	for _, d := range descs {
		hasher.Write([]byte(d))
		hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func (w *WorkflowAggregate) decideTokenBudgetExceeded(env event.Envelope) ([]event.Envelope, error) {
	payload := event.MustMarshal(event.WorkflowFailedPayload{
		Reason: "token budget exceeded",
	})
	return []event.Envelope{
		event.New(event.WorkflowFailed, 1, payload).
			WithAggregate(w.ID, w.Version+1).
			WithCausation(env.ID).
			WithCorrelation(env.CorrelationID).
			WithSource("engine:aggregate"),
	}, nil
}

// decideWorkflowResumed handles resume after auto-escalation. When the
// workflow was paused because MaxIterations was reached, resuming means
// the operator has provided guidance and wants to continue. We re-emit
// FeedbackGenerated for the phase that hit the limit, allowing the
// developer to re-run with the new guidance context.
func (w *WorkflowAggregate) decideWorkflowResumed(env event.Envelope) ([]event.Envelope, error) {
	// Find the phase that hit the iteration limit — it needs re-triggering.
	for phase, count := range w.FeedbackCount {
		if count >= w.MaxIterations {
			// Grant one additional iteration so the re-triggered feedback
			// doesn't immediately hit the limit again.
			w.MaxIterations = count + 1

			fbPayload := event.MustMarshal(event.FeedbackGeneratedPayload{
				TargetPhase: phase,
				Iteration:   count + 1,
				Summary:     "re-triggered after operator guidance",
			})
			return []event.Envelope{
				event.New(event.FeedbackGenerated, 1, fbPayload).
					WithAggregate(w.ID, w.Version+1).
					WithCausation(env.ID).
					WithCorrelation(env.CorrelationID).
					WithSource("engine:aggregate"),
			}, nil
		}
	}
	return nil, nil
}

// decideHintEmitted auto-approves or pauses based on hint confidence and blockers.
func (w *WorkflowAggregate) decideHintEmitted(env event.Envelope) ([]event.Envelope, error) {
	if w.Status != StatusRunning {
		return nil, nil
	}
	var h event.HintEmittedPayload
	_ = json.Unmarshal(env.Payload, &h)

	threshold := 0.7
	if w.WorkflowDef != nil && w.WorkflowDef.HintThreshold > 0 {
		threshold = w.WorkflowDef.HintThreshold
	}

	if h.Confidence >= threshold && len(h.Blockers) == 0 {
		payload := event.MustMarshal(event.HintApprovedPayload{
			Persona:   h.Persona,
			TriggerID: h.TriggerID,
		})
		return []event.Envelope{
			event.New(event.HintApproved, 1, payload).
				WithAggregate(w.ID, w.Version+1).
				WithCausation(env.ID).
				WithCorrelation(env.CorrelationID).
				WithSource("engine:aggregate"),
		}, nil
	}

	// Low confidence or blockers → pause for operator review.
	reason := fmt.Sprintf("hint from %s: confidence=%.2f", h.Persona, h.Confidence)
	if len(h.Blockers) > 0 {
		reason += fmt.Sprintf(", blockers=%v", h.Blockers)
	}
	payload := event.MustMarshal(event.WorkflowPausedPayload{
		Reason: reason,
		Source: "engine:hint-review",
	})
	return []event.Envelope{
		event.New(event.WorkflowPaused, 1, payload).
			WithAggregate(w.ID, w.Version+1).
			WithCausation(env.ID).
			WithCorrelation(env.CorrelationID).
			WithSource("engine:aggregate"),
	}, nil
}

// decideHintRejected handles skip or fail based on the rejection action.
func (w *WorkflowAggregate) decideHintRejected(env event.Envelope) ([]event.Envelope, error) {
	if w.Status != StatusRunning && w.Status != StatusPaused {
		return nil, nil
	}
	var h event.HintRejectedPayload
	_ = json.Unmarshal(env.Payload, &h)

	switch h.Action {
	case "skip":
		// Mark persona as completed-skipped so the workflow can proceed.
		payload := event.MustMarshal(event.PersonaCompletedPayload{
			Persona:      h.Persona,
			TriggerEvent: string(event.HintRejected),
			TriggerID:    string(env.ID),
		})
		return []event.Envelope{
			event.New(event.PersonaCompleted, 1, payload).
				WithAggregate(w.ID, w.Version+1).
				WithCausation(env.ID).
				WithCorrelation(env.CorrelationID).
				WithSource("engine:hint-skip"),
		}, nil
	case "fail":
		payload := event.MustMarshal(event.WorkflowFailedPayload{
			Reason: fmt.Sprintf("hint rejected for %s: %s", h.Persona, h.Reason),
			Phase:  h.Persona,
		})
		return []event.Envelope{
			event.New(event.WorkflowFailed, 1, payload).
				WithAggregate(w.ID, w.Version+1).
				WithCausation(env.ID).
				WithCorrelation(env.CorrelationID).
				WithSource("engine:hint-fail"),
		}, nil
	default:
		return nil, nil
	}
}
