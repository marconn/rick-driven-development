package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// handlerDeclaredEvents returns the event types a handler natively declares.
// Used as fallback for gRPC handlers not in any workflow Graph.
func handlerDeclaredEvents(h handler.Handler) []event.Type {
	if th, ok := h.(handler.TriggeredHandler); ok {
		return th.Trigger().Events
	}
	return h.Subscribes()
}

// workflowResolver manages workflow definitions, the correlationID→workflowID
// cache, and all DAG-based dispatch resolution logic. Thread-safe.
type workflowResolver struct {
	store  eventstore.Store
	logger *slog.Logger

	workflowsMu sync.RWMutex
	workflows   map[string]WorkflowDef // workflowID → def

	corrMapMu sync.RWMutex
	corrMap   map[string]string // correlationID → workflowID
}

func newWorkflowResolver(store eventstore.Store, logger *slog.Logger) *workflowResolver {
	return &workflowResolver{
		store:     store,
		logger:    logger,
		workflows: make(map[string]WorkflowDef),
		corrMap:   make(map[string]string),
	}
}

// registerWorkflow stores a workflow definition for DAG-based dispatch.
func (w *workflowResolver) registerWorkflow(def WorkflowDef) {
	w.workflowsMu.Lock()
	w.workflows[def.ID] = def
	w.workflowsMu.Unlock()
}

// getWorkflowDef returns the workflow definition for the given ID.
func (w *workflowResolver) getWorkflowDef(workflowID string) (WorkflowDef, bool) {
	w.workflowsMu.RLock()
	def, ok := w.workflows[workflowID]
	w.workflowsMu.RUnlock()
	return def, ok
}

// resolveWorkflowID returns the workflowID for a given correlationID from cache.
func (w *workflowResolver) resolveWorkflowID(correlationID string) (string, bool) {
	w.corrMapMu.RLock()
	wfID, ok := w.corrMap[correlationID]
	w.corrMapMu.RUnlock()
	return wfID, ok
}

// cacheWorkflowID stores a correlationID → workflowID mapping.
func (w *workflowResolver) cacheWorkflowID(correlationID, workflowID string) {
	w.corrMapMu.Lock()
	w.corrMap[correlationID] = workflowID
	w.corrMapMu.Unlock()
}

// evictCorrelation removes a correlationID from the cache on terminal events.
func (w *workflowResolver) evictCorrelation(correlationID string) {
	w.corrMapMu.Lock()
	delete(w.corrMap, correlationID)
	w.corrMapMu.Unlock()
}

// handlerInAnyGraph returns true if the handler name appears in any registered
// workflow's Graph.
func (w *workflowResolver) handlerInAnyGraph(name string) bool {
	w.workflowsMu.RLock()
	defer w.workflowsMu.RUnlock()
	for _, def := range w.workflows {
		if _, exists := def.Graph[name]; exists {
			return true
		}
	}
	return false
}

// handlerInAnyGraphAsNonRoot returns true if the handler appears in any
// workflow's Graph with non-empty predecessors.
func (w *workflowResolver) handlerInAnyGraphAsNonRoot(name string) bool {
	w.workflowsMu.RLock()
	defer w.workflowsMu.RUnlock()
	for _, def := range w.workflows {
		if deps, exists := def.Graph[name]; exists && len(deps) > 0 {
			return true
		}
	}
	return false
}

// resolveEventsFromDAG computes the event types a handler should subscribe to
// based on its presence across all registered workflow Graphs.
func (w *workflowResolver) resolveEventsFromDAG(h handler.Handler, hooks hookLookup) []event.Type {
	name := h.Name()
	var events []event.Type
	inAnyGraph := false

	w.workflowsMu.RLock()
	for _, def := range w.workflows {
		deps, exists := def.Graph[name]
		if !exists {
			continue
		}
		inAnyGraph = true
		if len(deps) == 0 {
			startEvt := event.WorkflowStartedFor(def.ID)
			if !slices.Contains(events, startEvt) {
				events = append(events, startEvt)
			}
		} else {
			if !slices.Contains(events, event.PersonaCompleted) {
				events = append(events, event.PersonaCompleted)
			}
		}
		for _, et := range def.RetriggeredBy[name] {
			if !slices.Contains(events, et) {
				events = append(events, et)
			}
		}
	}
	w.workflowsMu.RUnlock()

	if !inAnyGraph {
		events = handlerDeclaredEvents(h)
	}

	hasHooks := len(hooks.hooksFor(name)) > 0
	if hasHooks && !slices.Contains(events, event.PersonaCompleted) {
		events = append(events, event.PersonaCompleted)
	}

	return events
}

// isDAGRelevant checks whether this handler should fire for the given
// PersonaCompleted event using the workflow DAG.
func (w *workflowResolver) isDAGRelevant(h handler.Handler, completedPersona, correlationID string, hooks hookLookup) bool {
	wfID, ok := w.resolveWorkflowID(correlationID)
	if !ok {
		if w.handlerInAnyGraph(h.Name()) {
			return false
		}
		return w.isTriggerRelevant(h, completedPersona, hooks)
	}

	def, ok := w.getWorkflowDef(wfID)
	if !ok {
		if w.handlerInAnyGraph(h.Name()) {
			return false
		}
		return w.isTriggerRelevant(h, completedPersona, hooks)
	}

	deps, inGraph := def.Graph[h.Name()]
	if !inGraph {
		if w.handlerInAnyGraph(h.Name()) {
			return false
		}
		return w.isTriggerRelevant(h, completedPersona, hooks)
	}

	if slices.Contains(deps, completedPersona) {
		return true
	}

	return slices.Contains(hooks.hooksFor(h.Name()), completedPersona)
}

// isTriggerRelevant is the legacy relevance check for handlers with declared
// triggers (gRPC proxy handlers not in any Graph).
func (w *workflowResolver) isTriggerRelevant(h handler.Handler, completedPersona string, hooks hookLookup) bool {
	afterPersonas := w.legacyAfterPersonas(h, hooks)
	if len(afterPersonas) == 0 {
		return true
	}
	return slices.Contains(afterPersonas, completedPersona)
}

// legacyAfterPersonas returns handler-declared AfterPersonas merged with hooks.
func (w *workflowResolver) legacyAfterPersonas(h handler.Handler, hooks hookLookup) []string {
	var base []string
	if th, ok := h.(handler.TriggeredHandler); ok {
		base = th.Trigger().AfterPersonas
	}
	hks := hooks.hooksFor(h.Name())
	if len(hks) == 0 {
		return base
	}
	merged := make([]string, 0, len(base)+len(hks))
	merged = append(merged, base...)
	merged = append(merged, hks...)
	return merged
}

// isRetriggerable checks whether the handler is in RetriggeredBy for the
// workflow associated with the given correlationID.
func (w *workflowResolver) isRetriggerable(handlerName, correlationID string, handlerLookup func(string) (handler.Handler, bool)) bool {
	wfID, ok := w.resolveWorkflowID(correlationID)
	if !ok {
		h, exists := handlerLookup(handlerName)
		if !exists {
			return false
		}
		return slices.Contains(handlerDeclaredEvents(h), event.FeedbackGenerated)
	}

	def, ok := w.getWorkflowDef(wfID)
	if !ok {
		return false
	}

	return slices.Contains(def.RetriggeredBy[handlerName], event.FeedbackGenerated)
}

// effectiveAfterPersonas returns the full set of personas that must have
// completed before this handler can dispatch.
func (w *workflowResolver) effectiveAfterPersonas(h handler.Handler, correlationID string, hooks hookLookup) []string {
	var base []string

	wfID, ok := w.resolveWorkflowID(correlationID)
	if ok {
		def, defOk := w.getWorkflowDef(wfID)
		if defOk {
			if deps, inGraph := def.Graph[h.Name()]; inGraph {
				base = deps
			}
		}
	}

	if base == nil && !ok {
		if th, ok := h.(handler.TriggeredHandler); ok {
			base = th.Trigger().AfterPersonas
		}
	}

	hks := hooks.hooksFor(h.Name())
	if len(hks) == 0 {
		return base
	}
	merged := make([]string, 0, len(base)+len(hks))
	merged = append(merged, base...)
	merged = append(merged, hks...)
	return merged
}

// checkJoinCondition returns true when all requiredPersonas have a
// PersonaCompleted event recorded under the given correlationID.
//
// Return contract:
//   - satisfied=true: all predecessors are complete; dispatch proceeds.
//     fingerprint is the sorted joined predecessor event IDs (for join-gate
//     dedup). missing and err are nil.
//   - satisfied=false, err=nil: legitimate "not ready yet" — one or more
//     predecessors haven't emitted PersonaCompleted. missing lists which
//     personas are absent from latestByPersona.
//   - satisfied=false, err!=nil: store failure (LoadByCorrelation). Caller
//     should retry once before giving up; the error case is distinguishable
//     from legit unsatisfaction so transient store hiccups don't look like
//     wedges in the drop-reason telemetry.
func (w *workflowResolver) checkJoinCondition(ctx context.Context, requiredPersonas []string, correlationID string) (bool, string, []string, error) {
	events, err := w.store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return false, "", nil, fmt.Errorf("load correlation chain: %w", err)
	}

	var wfDef *WorkflowDef
	if wfID, ok := w.resolveWorkflowID(correlationID); ok {
		if def, ok := w.getWorkflowDef(wfID); ok {
			wfDef = &def
		}
	}

	type verdictTracker struct {
		active   bool
		sealed   bool
		advisory bool
	}
	latestByPersona := make(map[string]string)
	verdicts := make(map[string]*verdictTracker)
	// pendingStale tracks personas that have been invalidated by a
	// feedback.generated event but whose retrigger target has not yet
	// re-completed. PersonaCompleted events for these personas are skipped
	// because they represent in-flight work from the prior iteration that
	// finished after feedback was generated (late-arriving stale completions).
	// Entries are cleared when the retriggerTarget persona (e.g., "developer")
	// emits its next PersonaCompleted — marking the start of a fresh iteration.
	//
	// Without this guard, an in-flight qa from iteration N whose PC lands in
	// the store AFTER the feedback event would be re-added to latestByPersona
	// on the next checkJoinCondition pass, satisfying a join alongside a
	// freshly-completed reviewer from iteration N+1 and triggering downstream
	// handlers (e.g., quality-gate) twice.
	pendingStale := make(map[string]bool)
	var retriggerTarget string
	for _, e := range events {
		switch e.Type {
		case event.PersonaFailed:
			// Under WorkflowDef.PartialReviewOnFailure, a required-persona
			// failure is absorbed as a skip: the join gate must treat it as
			// a satisfied predecessor so the downstream consolidator fires
			// and can report the skipped reviewer. Without this, pr-consolidator
			// would block forever waiting on a dep that will never emit
			// PersonaCompleted.
			if wfDef == nil || !wfDef.PartialReviewOnFailure {
				continue
			}
			var pf event.PersonaFailedPayload
			if err := json.Unmarshal(e.Payload, &pf); err == nil && pf.Persona != "" {
				latestByPersona[pf.Persona] = string(e.ID)
			}

		case event.PersonaCompleted:
			var pc event.PersonaCompletedPayload
			if err := json.Unmarshal(e.Payload, &pc); err == nil {
				// Retrigger target re-completing marks the start of a fresh
				// iteration: clear pending flags for its strict downstream so
				// their real completions below are accepted.
				if retriggerTarget != "" && pc.Persona == retriggerTarget && wfDef != nil {
					for _, downstream := range wfDef.DownstreamOf(retriggerTarget) {
						if downstream != retriggerTarget {
							delete(pendingStale, downstream)
						}
					}
					retriggerTarget = ""
				}
				// Skip stale completions: this PC was in-flight when
				// feedback fired; it represents prior-iteration work.
				if pendingStale[pc.Persona] {
					continue
				}
				latestByPersona[pc.Persona] = string(e.ID)
				if vt := verdicts[pc.Persona]; vt != nil {
					if vt.sealed {
						delete(verdicts, pc.Persona)
					} else {
						vt.sealed = true
					}
				}
			}
		case event.VerdictRendered:
			var v event.VerdictPayload
			if err := json.Unmarshal(e.Payload, &v); err == nil && v.SourcePersona != "" {
				verdicts[v.SourcePersona] = &verdictTracker{
					active:   v.Outcome == event.VerdictFail,
					advisory: v.Advisory,
				}
			}
		case event.FeedbackGenerated:
			// Feedback invalidates all completions downstream of the
			// re-triggered persona. Wiping alone is insufficient because
			// events that arrive in the store AFTER this feedback would be
			// re-added to latestByPersona when iterated in order; mark them
			// as pending until the retrigger target re-completes.
			if wfDef != nil {
				var fb event.FeedbackGeneratedPayload
				if err := json.Unmarshal(e.Payload, &fb); err == nil && fb.TargetPersona != "" {
					target := fb.TargetPersona
					for _, stale := range wfDef.DownstreamOf(target) {
						if stale == target {
							continue
						}
						delete(latestByPersona, stale)
						delete(verdicts, stale)
						pendingStale[stale] = true
					}
					retriggerTarget = target
				}
			}
		}
	}

	ids := make([]string, 0, len(requiredPersonas))
	var missing []string
	for _, req := range requiredPersonas {
		id, ok := latestByPersona[req]
		if !ok {
			missing = append(missing, req)
			continue
		}
		if vt := verdicts[req]; vt != nil && vt.active && !vt.advisory && wfDef != nil && len(wfDef.RetriggeredBy) > 0 {
			// An active (fail) verdict is pending feedback — predecessor is
			// effectively not complete from the join's perspective. Advisory
			// fails are excluded: the aggregate's escalateVerdict path emits
			// WorkflowPaused without a FeedbackGenerated, so there is no
			// pending feedback to await — the operator's resume is the
			// signal to advance, and the runner's pauser/blocked-replay path
			// drives downstream handlers correctly once the join itself
			// stops blocking.
			missing = append(missing, req+"(pending_feedback)")
			continue
		}
		ids = append(ids, id)
	}
	if len(missing) > 0 {
		return false, "", missing, nil
	}

	sort.Strings(ids)
	return true, strings.Join(ids, "|"), nil, nil
}
