package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// ReviewConsolidatorName is the handler/persona name for the synchronization
// barrier that joins parallel reviewer + qa verdicts for the same developer
// iteration. Exported so workflow definitions and tests reference one
// canonical string.
const ReviewConsolidatorName = "review-consolidator"

// ReviewConsolidatorConfig configures the consolidator's join set, target
// persona, and event-store dependency.
type ReviewConsolidatorConfig struct {
	// Reviewers names the source personas whose VerdictRendered events the
	// consolidator joins. Must match the matching workflow def's
	// ConsolidatedReviewers AND the predecessors listed for this handler in
	// the Graph. Order is not significant.
	Reviewers []string
	// TargetPersona is the persona to re-trigger when any reviewer fails
	// (typically "developer"). Carried into the emitted FeedbackGenerated.
	TargetPersona string
	// Store is the event store used to load correlation history. Required —
	// the consolidator is stateless and reconstructs round state from events.
	Store eventstore.Store
	// Logger is used for diagnostic messages. May be nil; a default is used.
	Logger *slog.Logger
}

// ReviewConsolidator is the synchronization-barrier handler. It runs after
// the configured Reviewers have both rendered a VerdictRendered against the
// same developer iteration. When all verdicts pass it emits nothing and lets
// downstream handlers (quality-gate, committer) proceed via the DAG. When
// any verdict fails it emits a single merged FeedbackGenerated so the
// developer fires exactly once per round instead of once per reviewer.
//
// The handler is stateless on purpose: it derives round state by reading
// VerdictRendered events from the correlation log and grouping them by
// VerdictPayload.DevTriggerID (the developer PersonaCompleted ID that
// triggered the round). A server restart between reviewer completions and
// the consolidator dispatch is therefore non-destructive — on restart the
// DAG join re-fires when the late reviewer's PersonaCompleted replays.
type ReviewConsolidator struct {
	reviewers     []string
	targetPersona string
	store         eventstore.Store
	logger        *slog.Logger
}

// NewReviewConsolidator constructs the handler. Panics if Reviewers is empty
// or Store is nil — both are required for correct operation, and a misconfig
// at registration time is a programmer error caught immediately rather than
// a silent runtime degradation.
func NewReviewConsolidator(cfg ReviewConsolidatorConfig) *ReviewConsolidator {
	if len(cfg.Reviewers) == 0 {
		panic("review-consolidator: Reviewers must be non-empty")
	}
	if cfg.Store == nil {
		panic("review-consolidator: Store is required")
	}
	target := cfg.TargetPersona
	if target == "" {
		target = "developer"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	reviewers := append([]string(nil), cfg.Reviewers...)
	return &ReviewConsolidator{
		reviewers:     reviewers,
		targetPersona: target,
		store:         cfg.Store,
		logger:        logger,
	}
}

// Name implements handler.Handler.
func (h *ReviewConsolidator) Name() string { return ReviewConsolidatorName }

// Subscribes is informational only — PersonaRunner drives dispatch from
// WorkflowDef.Graph, not from this list. Returns nil so the handler does not
// fire outside of workflows that explicitly include it.
func (h *ReviewConsolidator) Subscribes() []event.Type { return nil }

// Handle is invoked after the configured reviewers complete (DAG join). It
// loads the correlation log, picks the most recent VerdictRendered for each
// reviewer that targets the same DevTriggerID, and emits a merged
// FeedbackGenerated if any are fail. When all reviewers pass it emits no
// events — quality-gate (whose predecessor is this handler) then fires on
// the consolidator's PersonaCompleted.
func (h *ReviewConsolidator) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	events, err := h.store.LoadByCorrelation(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("review-consolidator: load correlation %s: %w", env.CorrelationID, err)
	}

	verdicts, devTriggerID := h.pickRoundVerdicts(events)
	if len(verdicts) == 0 {
		// No matching verdicts on the chain. Shouldn't happen under the DAG
		// invariant (we only run after both reviewers completed) but guard
		// against replay/restart edge cases by no-opping rather than
		// fabricating feedback.
		h.logger.Warn("review-consolidator: no reviewer verdicts found",
			slog.String("correlation_id", env.CorrelationID),
			slog.String("trigger_id", string(env.ID)),
		)
		return nil, nil
	}

	failing := failingVerdicts(verdicts)
	if len(failing) == 0 {
		// All reviewers passed — emit no FeedbackGenerated. The consolidator's
		// PersonaCompleted (added by PersonaRunner) advances quality-gate.
		h.logger.Info("review-consolidator: round passed",
			slog.String("correlation_id", env.CorrelationID),
			slog.String("dev_trigger_id", devTriggerID),
			slog.Int("verdicts", len(verdicts)),
		)
		return nil, nil
	}

	iteration := countPriorFeedback(events, h.targetPersona) + 1
	fb := buildMergedFeedback(failing, h.targetPersona, iteration)
	h.logger.Info("review-consolidator: round failed — emitting merged feedback",
		slog.String("correlation_id", env.CorrelationID),
		slog.String("dev_trigger_id", devTriggerID),
		slog.Int("verdicts", len(verdicts)),
		slog.Int("failing", len(failing)),
		slog.Int("issues", len(fb.Issues)),
		slog.Int("iteration", iteration),
	)

	return []event.Envelope{
		event.New(event.FeedbackGenerated, 1, event.MustMarshal(fb)).
			WithSource("handler:" + ReviewConsolidatorName),
	}, nil
}

// pickRoundVerdicts walks correlation events and returns the per-reviewer
// VerdictRendered events for the round being consolidated. The round is
// identified by DevTriggerID: it picks the *latest* DevTriggerID that has at
// least one reviewer verdict, then collects the most-recent verdict per
// reviewer tagged with that same DevTriggerID.
//
// Verdicts from reviewers not in h.reviewers are ignored. Older verdicts
// from earlier dev iterations are ignored — only the current round counts.
// Returns the verdicts plus the picked DevTriggerID for logging.
func (h *ReviewConsolidator) pickRoundVerdicts(events []event.Envelope) ([]event.VerdictPayload, string) {
	// First pass: find the latest DevTriggerID that has a reviewer verdict.
	devTriggerID := ""
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type != event.VerdictRendered {
			continue
		}
		var v event.VerdictPayload
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			continue
		}
		if v.DevTriggerID == "" {
			continue
		}
		if !slices.Contains(h.reviewers, v.SourcePersona) {
			continue
		}
		devTriggerID = v.DevTriggerID
		break
	}
	if devTriggerID == "" {
		return nil, ""
	}

	// Second pass: collect the latest verdict per reviewer for that
	// DevTriggerID. Iterate forward; later events for the same persona
	// overwrite earlier ones so the final map holds the most recent per
	// reviewer (defensive — under the DAG invariant there is at most one
	// completion per persona per round).
	bySource := make(map[string]event.VerdictPayload, len(h.reviewers))
	for _, e := range events {
		if e.Type != event.VerdictRendered {
			continue
		}
		var v event.VerdictPayload
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			continue
		}
		if v.DevTriggerID != devTriggerID {
			continue
		}
		if !slices.Contains(h.reviewers, v.SourcePersona) {
			continue
		}
		bySource[v.SourcePersona] = v
	}

	verdicts := make([]event.VerdictPayload, 0, len(bySource))
	// Preserve the configured reviewer order so the merged feedback is
	// deterministic across runs (useful for test assertions + diff replay).
	for _, name := range h.reviewers {
		if v, ok := bySource[name]; ok {
			verdicts = append(verdicts, v)
		}
	}
	return verdicts, devTriggerID
}

// failingVerdicts filters for VerdictFail outcomes. Advisory verdicts are
// excluded — the aggregate has already emitted WorkflowPaused for those
// when the raw verdict landed (escapes the consolidator entirely).
func failingVerdicts(verdicts []event.VerdictPayload) []event.VerdictPayload {
	out := make([]event.VerdictPayload, 0, len(verdicts))
	for _, v := range verdicts {
		if v.Outcome != event.VerdictFail {
			continue
		}
		if v.Advisory {
			continue
		}
		out = append(out, v)
	}
	return out
}

// countPriorFeedback returns the number of FeedbackGenerated events that
// targeted the given persona in this correlation. Used to compute the next
// iteration number for the merged FeedbackGenerated, matching the aggregate's
// FeedbackCount semantics.
func countPriorFeedback(events []event.Envelope, target string) int {
	n := 0
	for _, e := range events {
		if e.Type != event.FeedbackGenerated {
			continue
		}
		var p event.FeedbackGeneratedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if p.TargetPersona == target {
			n++
		}
	}
	return n
}

// buildMergedFeedback combines failing verdicts into a single payload. Issues
// are concatenated in reviewer order; descriptions are prefixed with the
// source persona so the developer can see which reviewer raised which
// concern. Summary is a single line listing the source personas — the
// detailed per-source summaries surface in the bulleted issue list.
//
// RawDiagnostics is concatenated when present: quality-gate uses it for the
// raw failure tail, and even though reviewer/qa rarely populate it today,
// preserving the field keeps the door open for future review handlers that
// do.
func buildMergedFeedback(failing []event.VerdictPayload, target string, iteration int) event.FeedbackGeneratedPayload {
	issues := make([]event.Issue, 0)
	sources := make([]string, 0, len(failing))
	var rawDiag strings.Builder

	for _, v := range failing {
		sources = append(sources, v.SourcePersona)
		for _, iss := range v.Issues {
			tagged := iss
			tagged.Description = "[" + v.SourcePersona + "] " + iss.Description
			issues = append(issues, tagged)
		}
		// Some reviewers emit a summary but no structured issues. Surface the
		// summary as a synthetic issue so it does not get lost.
		if len(v.Issues) == 0 && strings.TrimSpace(v.Summary) != "" {
			issues = append(issues, event.Issue{
				Severity:    "minor",
				Category:    "correctness",
				Description: "[" + v.SourcePersona + "] " + strings.TrimSpace(v.Summary),
			})
		}
		if v.RawDiagnostics != "" {
			if rawDiag.Len() > 0 {
				rawDiag.WriteString("\n---\n")
			}
			rawDiag.WriteString("[" + v.SourcePersona + "]\n")
			rawDiag.WriteString(v.RawDiagnostics)
		}
	}

	summary := fmt.Sprintf("review-consolidator: merged feedback from %s", strings.Join(sources, ", "))

	return event.FeedbackGeneratedPayload{
		TargetPersona:  target,
		SourcePersona:  ReviewConsolidatorName,
		Iteration:      iteration,
		Issues:         issues,
		Summary:        summary,
		RawDiagnostics: rawDiag.String(),
	}
}
