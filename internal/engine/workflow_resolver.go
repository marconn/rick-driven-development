package engine

import (
	"context"
	"encoding/json"
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
func (w *workflowResolver) checkJoinCondition(ctx context.Context, requiredPersonas []string, correlationID string) (bool, string) {
	events, err := w.store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		w.logger.Error("persona runner: join check failed",
			slog.String("correlation", correlationID),
			slog.String("error", err.Error()),
		)
		return false, ""
	}

	var wfDef *WorkflowDef
	if wfID, ok := w.resolveWorkflowID(correlationID); ok {
		if def, ok := w.getWorkflowDef(wfID); ok {
			wfDef = &def
		}
	}

	type verdictTracker struct {
		active bool
		sealed bool
	}
	latestByPersona := make(map[string]string)
	verdicts := make(map[string]*verdictTracker)
	for _, e := range events {
		switch e.Type {
		case event.PersonaCompleted:
			var pc event.PersonaCompletedPayload
			if err := json.Unmarshal(e.Payload, &pc); err == nil {
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
			if err := json.Unmarshal(e.Payload, &v); err == nil {
				sourcePersona := v.SourcePhase
				if wfDef != nil {
					sourcePersona = wfDef.ResolvePhase(v.SourcePhase)
				}
				verdicts[sourcePersona] = &verdictTracker{
					active: v.Outcome == event.VerdictFail,
				}
			}
		case event.FeedbackGenerated:
			// Feedback invalidates all completions downstream of the
			// re-triggered persona. Without this, stale PersonaCompleted
			// events from a previous iteration satisfy join conditions
			// prematurely (e.g., quality-gate fires with old qa completion
			// before qa has re-run after the feedback loop).
			if wfDef != nil {
				var fb event.FeedbackGeneratedPayload
				if err := json.Unmarshal(e.Payload, &fb); err == nil {
					for _, stale := range wfDef.DownstreamOf(fb.TargetPhase) {
						delete(latestByPersona, stale)
						delete(verdicts, stale)
					}
				}
			}
		}
	}

	ids := make([]string, 0, len(requiredPersonas))
	for _, req := range requiredPersonas {
		id, ok := latestByPersona[req]
		if !ok {
			return false, ""
		}
		if vt := verdicts[req]; vt != nil && vt.active && wfDef != nil && len(wfDef.RetriggeredBy) > 0 {
			return false, ""
		}
		ids = append(ids, id)
	}

	sort.Strings(ids)
	return true, strings.Join(ids, "|")
}
