package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

const (
	defaultDrainTimeout = 30 * time.Second
	defaultMaxChain     = 5
	defaultMaxActive    = 10
	defaultDedup        = 10000
)

// Event dispatch priorities. Lower value = higher priority.
// When multiple events are pending for the same handler+correlation,
// the highest-priority event is processed first.
const (
	PriorityOperatorGuidance  = 0
	PriorityFeedbackGenerated = 10
	PriorityPersonaCompleted  = 20
	PriorityDefault           = 30
)

// eventPriority maps an event type to its dispatch priority.
func eventPriority(t event.Type) int {
	switch t {
	case event.OperatorGuidance, event.HintApproved:
		return PriorityOperatorGuidance
	case event.FeedbackGenerated, event.ChildWorkflowCompleted:
		return PriorityFeedbackGenerated
	case event.PersonaCompleted, event.PersonaFailed:
		return PriorityPersonaCompleted
	default:
		return PriorityDefault
	}
}

// PersonaRunner is the sole dispatcher for ALL persona handlers. It uses
// DAG-based dispatch: workflow definitions declare execution topology via
// Graph, and handlers are dumb workers with no trigger declarations.
//
// On PersonaCompleted, the runner looks up the workflow's DAG, finds which
// handlers are now unlocked, and dispatches them. Handlers not in the
// workflow's Graph are never dispatched for that correlation.
//
// Events for the same (handler, correlation) are serialized through a
// per-key priority queue, ensuring a handler never runs concurrently on
// the same workflow. Different handlers and different workflows run in parallel.
type PersonaRunner struct {
	store      eventstore.Store
	bus        eventbus.Bus
	dispatcher Dispatcher
	logger     *slog.Logger

	// Lifecycle
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	drainTimeout time.Duration

	// Safety
	maxChain  atomic.Int32 // max reactive chain depth (adjusted dynamically)
	maxActive int32        // max concurrent reactive handlers
	active    atomic.Int32
	seen      *idempotencyCache

	// Pause support
	pausedMu sync.RWMutex
	paused   map[string]bool             // correlationID → paused
	blocked  map[string][]blockedDispatch // correlationID → pending dispatches

	// Before-hooks: persona name → additional personas that must complete first.
	// Injected at runtime via WithBeforeHook — no handler code changes needed.
	hooksMu sync.RWMutex
	hooks   map[string][]string

	// Per-(handler, correlation) dispatch queues for serial execution + priority.
	queuesMu sync.Mutex
	queues   map[string]*dispatchQueue

	// Hint support: track which handlers implement Hinter for two-phase dispatch.
	hinters map[string]handler.Hinter // handler name → Hinter impl

	// Handler references for dynamic subscription management.
	// Stored during Start() and RegisterHandler() so RegisterHook()
	// can add persona.completed subscriptions for gated handlers.
	handlersMu sync.RWMutex
	handlers   map[string]handler.Handler // handler name → Handler

	// Dynamic handler tracking: unsubscribes old subscriptions when a handler
	// re-registers (e.g., gRPC reconnect). Prevents duplicate bus subscriptions.
	dynamicMu  sync.Mutex
	dynamicGen map[string]uint64 // handler name → current generation

	unsubs []func()

	// DAG-based dispatch: workflow defs + correlation→workflowID cache.
	workflowsMu sync.RWMutex
	workflows   map[string]WorkflowDef // workflowID → def

	corrMapMu sync.RWMutex
	corrMap   map[string]string // correlationID → workflowID
}

// PersonaRunnerOption configures a PersonaRunner.
type PersonaRunnerOption func(*PersonaRunner)

// WithDrainTimeout sets the max wait for in-flight handlers on Close().
func WithDrainTimeout(d time.Duration) PersonaRunnerOption {
	return func(r *PersonaRunner) { r.drainTimeout = d }
}

// WithMaxChainDepth sets the max reactive chain depth (storm protection).
func WithMaxChainDepth(n int) PersonaRunnerOption {
	return func(r *PersonaRunner) { r.maxChain.Store(int32(n)) }
}

// AdjustChainDepth raises the max chain depth if the given workflow phase
// count (plus a margin for feedback loops) exceeds the current limit.
// Safe to call concurrently — used as a callback on workflow registration
// so the limit auto-scales with the longest registered workflow.
func (r *PersonaRunner) AdjustChainDepth(requiredCount int) {
	// Margin covers feedback loops where the developer→reviewer→qa chain
	// re-executes after a failed verdict.
	needed := int32(requiredCount + 5)
	for {
		old := r.maxChain.Load()
		if needed <= old {
			return
		}
		if r.maxChain.CompareAndSwap(old, needed) {
			r.logger.Info("persona runner: adjusted max chain depth",
				slog.Int("old", int(old)),
				slog.Int("new", int(needed)),
				slog.Int("workflow_phases", requiredCount),
			)
			return
		}
	}
}

// WithMaxActive sets the max concurrent reactive handlers.
func WithMaxActive(n int) PersonaRunnerOption {
	return func(r *PersonaRunner) { r.maxActive = int32(n) }
}

// WithBeforeHook injects additional join conditions for a persona without
// modifying handler code. The hook personas must emit PersonaCompleted before
// the target persona is dispatched. Multiple hooks for the same persona are
// merged additively.
func WithBeforeHook(persona string, hookPersonas ...string) PersonaRunnerOption {
	return func(r *PersonaRunner) {
		if r.hooks == nil {
			r.hooks = make(map[string][]string)
		}
		r.hooks[persona] = append(r.hooks[persona], hookPersonas...)
	}
}

// NewPersonaRunner creates a PersonaRunner that acts as the sole dispatcher
// for all persona handlers.
func NewPersonaRunner(store eventstore.Store, bus eventbus.Bus, dispatcher Dispatcher, logger *slog.Logger, opts ...PersonaRunnerOption) *PersonaRunner {
	r := &PersonaRunner{
		store:        store,
		bus:          bus,
		dispatcher:   dispatcher,
		logger:       logger,
		drainTimeout: defaultDrainTimeout,
		maxActive:    int32(defaultMaxActive),
		seen:         newIdempotencyCache(defaultDedup),
		paused:       make(map[string]bool),
		blocked:      make(map[string][]blockedDispatch),
		queues:       make(map[string]*dispatchQueue),
		hinters:      make(map[string]handler.Hinter),
		handlers:     make(map[string]handler.Handler),
		dynamicGen:   make(map[string]uint64),
		workflows:    make(map[string]WorkflowDef),
		corrMap:      make(map[string]string),
	}
	r.maxChain.Store(int32(defaultMaxChain))
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// RegisterWorkflow registers a workflow definition for DAG-based dispatch.
// Must be called before Start() for built-in workflows, or after for dynamic ones.
func (r *PersonaRunner) RegisterWorkflow(def WorkflowDef) {
	r.workflowsMu.Lock()
	r.workflows[def.ID] = def
	r.workflowsMu.Unlock()
}

// resolveWorkflowID returns the workflowID for a given correlationID from cache.
func (r *PersonaRunner) resolveWorkflowID(correlationID string) (string, bool) {
	r.corrMapMu.RLock()
	wfID, ok := r.corrMap[correlationID]
	r.corrMapMu.RUnlock()
	return wfID, ok
}

// cacheWorkflowID stores a correlationID → workflowID mapping.
func (r *PersonaRunner) cacheWorkflowID(correlationID, workflowID string) {
	r.corrMapMu.Lock()
	r.corrMap[correlationID] = workflowID
	r.corrMapMu.Unlock()
}

// evictCorrelation removes a correlationID from the cache on terminal events.
func (r *PersonaRunner) evictCorrelation(correlationID string) {
	r.corrMapMu.Lock()
	delete(r.corrMap, correlationID)
	r.corrMapMu.Unlock()
}

// getWorkflowDef returns the workflow definition for the given ID.
func (r *PersonaRunner) getWorkflowDef(workflowID string) (WorkflowDef, bool) {
	r.workflowsMu.RLock()
	def, ok := r.workflows[workflowID]
	r.workflowsMu.RUnlock()
	return def, ok
}

// Start subscribes all handlers to the bus using DAG-based event resolution.
// Handlers are subscribed based on their role across all registered workflows'
// Graphs, not based on handler-declared triggers.
func (r *PersonaRunner) Start(ctx context.Context, registry *handler.Registry) {
	r.ctx, r.cancel = context.WithCancel(ctx)
	r.subscribePauseResume()
	r.subscribeHintApproved()
	r.subscribeWorkflowStarted()
	r.subscribeTerminalEvents()
	for _, h := range registry.All() {
		r.handlers[h.Name()] = h
		r.registerHinter(h)
		events := r.resolveEventsFromDAG(h)
		if len(events) == 0 {
			continue
		}
		for _, et := range events {
			unsub := r.bus.Subscribe(et, r.wrap(h), eventbus.WithName("persona:"+h.Name()))
			r.unsubs = append(r.unsubs, unsub)
		}
		r.logger.Info("persona runner: subscribed handler",
			slog.String("handler", h.Name()),
			slog.Int("event_types", len(events)),
		)
	}
}

// RegisterHandler subscribes a handler to the bus after Start() has been called.
// Returns an unsubscribe function that removes all bus subscriptions for this handler.
// Used for dynamic registration of external handlers (gRPC, webhooks).
// Re-registering the same handler name bumps the generation counter so the old
// unsubscribe (from defer in gRPC HandleStream) is a no-op.
func (r *PersonaRunner) RegisterHandler(h handler.Handler) func() {
	r.handlersMu.Lock()
	r.handlers[h.Name()] = h
	r.handlersMu.Unlock()
	r.registerHinter(h)
	events := r.resolveEventsFromDAG(h)
	var handlerUnsubs []func()
	for _, et := range events {
		unsub := r.bus.Subscribe(et, r.wrap(h), eventbus.WithName("persona:"+h.Name()))
		r.unsubs = append(r.unsubs, unsub)
		handlerUnsubs = append(handlerUnsubs, unsub)
	}

	// Bump generation — old unsubscribe calls with stale generation are no-ops.
	r.dynamicMu.Lock()
	r.dynamicGen[h.Name()]++
	gen := r.dynamicGen[h.Name()]
	r.dynamicMu.Unlock()

	r.logger.Info("persona runner: dynamic handler registered",
		slog.String("handler", h.Name()),
		slog.Int("event_types", len(events)),
	)

	unsubFn := func() {
		r.dynamicMu.Lock()
		current := r.dynamicGen[h.Name()]
		r.dynamicMu.Unlock()

		// Only unsubscribe if we're still the active generation.
		// A newer RegisterHandler call supersedes us.
		if current != gen {
			return
		}

		for _, unsub := range handlerUnsubs {
			unsub()
		}
		r.logger.Info("persona runner: dynamic handler unregistered",
			slog.String("handler", h.Name()),
		)
	}

	return unsubFn
}

// RegisterHook adds a before-hook at runtime after Start() has been called.
// Used for dynamic registration of external handlers that need to gate a persona.
// If the gated handler is already subscribed but doesn't natively listen to
// persona.completed, an additional subscription is added so the handler gets
// re-evaluated when hook handlers complete.
func (r *PersonaRunner) RegisterHook(persona string, hookPersonas ...string) {
	r.hooksMu.Lock()
	if r.hooks == nil {
		r.hooks = make(map[string][]string)
	}
	r.hooks[persona] = append(r.hooks[persona], hookPersonas...)
	r.hooksMu.Unlock()

	// If the gated handler is already registered, ensure it's subscribed
	// to persona.completed so it gets re-evaluated when hook handlers complete.
	r.handlersMu.RLock()
	h, exists := r.handlers[persona]
	r.handlersMu.RUnlock()

	if exists {
		// Check if handler is already subscribed to PersonaCompleted via DAG
		// (i.e., it has non-empty predecessors in any workflow Graph).
		alreadySubscribed := r.handlerInAnyGraphAsNonRoot(persona)
		if !alreadySubscribed {
			unsub := r.bus.Subscribe(event.PersonaCompleted, r.wrap(h),
				eventbus.WithName("persona:"+h.Name()+":hook-trigger"))
			r.unsubs = append(r.unsubs, unsub)
			r.logger.Info("persona runner: added persona.completed subscription for hooked handler",
				slog.String("handler", persona),
			)
		}
	}

	r.logger.Info("persona runner: dynamic hook registered",
		slog.String("target", persona),
		slog.Any("hooks", hookPersonas),
	)
}

// handlerInAnyGraphAsNonRoot returns true if the handler appears in any
// workflow's Graph with non-empty predecessors (i.e., would be subscribed
// to PersonaCompleted via DAG resolution).
func (r *PersonaRunner) handlerInAnyGraphAsNonRoot(name string) bool {
	r.workflowsMu.RLock()
	defer r.workflowsMu.RUnlock()
	for _, def := range r.workflows {
		if deps, exists := def.Graph[name]; exists && len(deps) > 0 {
			return true
		}
	}
	return false
}

// RegisterExternalHinter registers a Hinter implementation for a handler that
// is NOT a local handler.Handler (e.g., a gRPC proxy). This enables two-phase
// hint/execute dispatch for externally-connected handlers.
func (r *PersonaRunner) RegisterExternalHinter(name string, hinter handler.Hinter) {
	r.hinters[name] = hinter
	r.logger.Info("persona runner: external hinter registered", slog.String("handler", name))
}

// UnregisterExternalHinter removes a registered external Hinter.
func (r *PersonaRunner) UnregisterExternalHinter(name string) {
	delete(r.hinters, name)
	r.logger.Info("persona runner: external hinter unregistered", slog.String("handler", name))
}

// UnregisterHook removes a hook handler from a persona's before-hook list.
func (r *PersonaRunner) UnregisterHook(persona string, hookName string) {
	r.hooksMu.Lock()
	defer r.hooksMu.Unlock()
	hooks := r.hooks[persona]
	for i, h := range hooks {
		if h == hookName {
			r.hooks[persona] = append(hooks[:i], hooks[i+1:]...)
			break
		}
	}
	if len(r.hooks[persona]) == 0 {
		delete(r.hooks, persona)
	}
	r.logger.Info("persona runner: dynamic hook unregistered",
		slog.String("target", persona),
		slog.String("hook", hookName),
	)
}

// resolveEventsFromDAG computes the event types a handler should subscribe to
// based on its presence across all registered workflow Graphs. This replaces
// the old trigger-based resolveEvents().
//
// Rules:
// - Root in any Graph (empty deps) → subscribe to WorkflowStartedFor(workflowID)
// - Non-root in any Graph (has deps) → subscribe to PersonaCompleted
// - In any RetriggeredBy → subscribe to those event types
// - Before-hooks → add PersonaCompleted if not already included
// - Not in any Graph → fall back to handler-declared triggers (gRPC compat)
func (r *PersonaRunner) resolveEventsFromDAG(h handler.Handler) []event.Type {
	name := h.Name()
	var events []event.Type
	inAnyGraph := false

	r.workflowsMu.RLock()
	for _, def := range r.workflows {
		deps, exists := def.Graph[name]
		if !exists {
			continue
		}
		inAnyGraph = true
		if len(deps) == 0 {
			// Root handler — subscribe to WorkflowStartedFor this workflow.
			startEvt := event.WorkflowStartedFor(def.ID)
			if !slices.Contains(events, startEvt) {
				events = append(events, startEvt)
			}
		} else {
			// Non-root handler — subscribe to PersonaCompleted.
			if !slices.Contains(events, event.PersonaCompleted) {
				events = append(events, event.PersonaCompleted)
			}
		}
		// RetriggeredBy events (e.g., FeedbackGenerated for developer).
		for _, et := range def.RetriggeredBy[name] {
			if !slices.Contains(events, et) {
				events = append(events, et)
			}
		}
	}
	r.workflowsMu.RUnlock()

	// Fallback: handler not in any Graph — use declared triggers (gRPC compat).
	if !inAnyGraph {
		events = handlerDeclaredEvents(h)
	}

	// Before-hooks: ensure PersonaCompleted subscription for the gated handler.
	r.hooksMu.RLock()
	hasHooks := len(r.hooks[name]) > 0
	r.hooksMu.RUnlock()
	if hasHooks && !slices.Contains(events, event.PersonaCompleted) {
		events = append(events, event.PersonaCompleted)
	}

	return events
}

// handlerDeclaredEvents returns the event types a handler natively declares.
// Used as fallback for gRPC handlers not in any workflow Graph.
func handlerDeclaredEvents(h handler.Handler) []event.Type {
	if th, ok := h.(handler.TriggeredHandler); ok {
		return th.Trigger().Events
	}
	return h.Subscribes()
}

// subscribeWorkflowStarted subscribes to all workflow.started.* events to
// populate the correlationID → workflowID cache.
func (r *PersonaRunner) subscribeWorkflowStarted() {
	unsub := r.bus.SubscribeAll(func(_ context.Context, env event.Envelope) error {
		if !strings.HasPrefix(string(env.Type), "workflow.started.") {
			return nil
		}
		// Extract workflowID from event type: "workflow.started.<id>"
		parts := strings.SplitN(string(env.Type), ".", 3)
		if len(parts) < 3 {
			return nil
		}
		workflowID := parts[2]
		corrID := env.CorrelationID
		if corrID == "" {
			corrID = env.AggregateID
		}
		if corrID != "" {
			r.cacheWorkflowID(corrID, workflowID)
		}
		return nil
	}, eventbus.WithName("persona-runner:workflow-cache"))
	r.unsubs = append(r.unsubs, unsub)
}

// subscribeTerminalEvents subscribes to terminal workflow events to evict
// the correlation cache.
func (r *PersonaRunner) subscribeTerminalEvents() {
	for _, et := range []event.Type{event.WorkflowCompleted, event.WorkflowFailed, event.WorkflowCancelled} {
		unsub := r.bus.Subscribe(et, func(_ context.Context, env event.Envelope) error {
			corrID := env.CorrelationID
			if corrID == "" {
				corrID = env.AggregateID
			}
			if corrID != "" {
				r.evictCorrelation(corrID)
			}
			return nil
		}, eventbus.WithName("persona-runner:evict:"+string(et)))
		r.unsubs = append(r.unsubs, unsub)
	}
}

// Close performs graceful shutdown: unsubscribe, cancel context, drain in-flight.
func (r *PersonaRunner) Close() error {
	for _, unsub := range r.unsubs {
		unsub()
	}
	r.unsubs = nil

	if r.cancel != nil {
		r.cancel()
	}

	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(r.drainTimeout):
		return fmt.Errorf("persona runner: drain timeout after %s with %d active handlers",
			r.drainTimeout, r.active.Load())
	}
}

// wrap creates an eventbus.HandlerFunc that admits events through safety checks
// and enqueues them into the per-(handler, correlation) priority queue.
//
// DAG-based dispatch: on PersonaCompleted, the handler must be in the completing
// persona's workflow Graph, and the completing persona must be a predecessor.
// On WorkflowStarted, only root handlers (empty deps) fire.
// On FeedbackGenerated, only handlers in RetriggeredBy fire.
func (r *PersonaRunner) wrap(h handler.Handler) eventbus.HandlerFunc {
	return func(_ context.Context, env event.Envelope) error {
		chainDepth := 0

		// 1. PersonaCompleted checks: self-trigger prevention, chain depth, DAG relevance
		if env.Type == event.PersonaCompleted {
			var pc event.PersonaCompletedPayload
			if err := json.Unmarshal(env.Payload, &pc); err == nil {
				if pc.Persona == h.Name() {
					return nil // prevent A→A loops
				}
				chainDepth = pc.ChainDepth + 1

				// DAG relevance check: is this handler in the workflow's Graph,
				// and is the completing persona one of its predecessors?
				if !r.isDAGRelevant(h, pc.Persona, env.CorrelationID) {
					return nil
				}
			}
		}
		if env.Type == event.PersonaFailed {
			var pf event.PersonaFailedPayload
			if err := json.Unmarshal(env.Payload, &pf); err == nil {
				if pf.Persona == h.Name() {
					return nil
				}
				chainDepth = pf.ChainDepth + 1
			}
		}

		// 2. FeedbackGenerated: only fire if handler is in RetriggeredBy for this workflow.
		if env.Type == event.FeedbackGenerated {
			if !r.isRetriggerable(h.Name(), env.CorrelationID) {
				return nil
			}
		}

		// 3. Chain depth check
		if chainDepth >= int(r.maxChain.Load()) {
			r.logger.Warn("persona runner: max chain depth reached",
				slog.String("handler", h.Name()),
				slog.Int("depth", chainDepth),
				slog.String("event_id", string(env.ID)),
			)
			return nil
		}

		// 4. Event dedup
		if !r.seen.Add(h.Name(), string(env.ID)) {
			return nil // already dispatched
		}

		// 5. Width limit
		if r.active.Load() >= r.maxActive {
			r.logger.Warn("persona runner: max active handlers reached",
				slog.String("handler", h.Name()),
				slog.Int("active", int(r.active.Load())),
			)
			return nil
		}

		// 6. Join condition check (DAG deps + hooks)
		afterPersonas := r.effectiveAfterPersonas(h, env.CorrelationID)
		if len(afterPersonas) > 0 && env.CorrelationID != "" {
			satisfied, fingerprint := r.checkJoinCondition(afterPersonas, env.CorrelationID)
			if !satisfied {
				return nil
			}
			// Join-gate dedup: when multiple PersonaCompleted events
			// satisfy the same join, dispatch only once per unique set.
			if len(afterPersonas) > 1 && env.Type == event.PersonaCompleted {
				if !r.seen.Add(h.Name()+":join", fingerprint) {
					return nil
				}
			}
		}

		// 7. Pause check
		if r.isPaused(env.CorrelationID) {
			r.addBlocked(env.CorrelationID, h, env)
			return nil
		}

		// 8. Check runner context
		if r.ctx.Err() != nil {
			return nil
		}

		// 9. Enqueue into the per-(handler, correlation) priority queue.
		r.enqueueAndDrain(h, env, chainDepth)
		return nil
	}
}

// isDAGRelevant checks whether this handler should fire for the given
// PersonaCompleted event. Uses the workflow DAG for scoping:
// - Resolves workflowID from corrMap cache
// - Checks if handler is in that workflow's Graph
// - Checks if the completing persona is one of the handler's predecessors (or a hook)
//
// Falls back to trigger-based relevance for handlers not in any Graph (gRPC compat).
func (r *PersonaRunner) isDAGRelevant(h handler.Handler, completedPersona, correlationID string) bool {
	wfID, ok := r.resolveWorkflowID(correlationID)
	if !ok {
		// No cached workflowID. Graph-managed handlers must not fire without
		// a known workflow — prevents cross-workflow dispatch on cache miss race.
		if r.handlerInAnyGraph(h.Name()) {
			return false
		}
		// Truly unmanaged handler (gRPC, no Graph) — fall back to triggers.
		return r.isTriggerRelevant(h, completedPersona)
	}

	def, ok := r.getWorkflowDef(wfID)
	if !ok {
		if r.handlerInAnyGraph(h.Name()) {
			return false
		}
		return r.isTriggerRelevant(h, completedPersona)
	}

	deps, inGraph := def.Graph[h.Name()]
	if !inGraph {
		// Handler not in this workflow's Graph. Check if it's a trigger-based
		// handler (gRPC fallback) — but only if it's not in ANY Graph, otherwise
		// it's scoped to other workflows and shouldn't fire here.
		if r.handlerInAnyGraph(h.Name()) {
			return false // handler belongs to other workflow(s), not this one
		}
		return r.isTriggerRelevant(h, completedPersona)
	}

	// Check if the completing persona is a predecessor in this DAG.
	if slices.Contains(deps, completedPersona) {
		return true
	}

	// Also check hooks — hook completion should trigger re-evaluation.
	r.hooksMu.RLock()
	hooks := r.hooks[h.Name()]
	r.hooksMu.RUnlock()
	return slices.Contains(hooks, completedPersona)
}

// handlerInAnyGraph returns true if the handler name appears in any registered
// workflow's Graph.
func (r *PersonaRunner) handlerInAnyGraph(name string) bool {
	r.workflowsMu.RLock()
	defer r.workflowsMu.RUnlock()
	for _, def := range r.workflows {
		if _, exists := def.Graph[name]; exists {
			return true
		}
	}
	return false
}

// isTriggerRelevant is the legacy relevance check for handlers with declared
// triggers (gRPC proxy handlers not in any Graph).
func (r *PersonaRunner) isTriggerRelevant(h handler.Handler, completedPersona string) bool {
	afterPersonas := r.legacyAfterPersonas(h)
	if len(afterPersonas) == 0 {
		return true // no filter — fires on any PersonaCompleted
	}
	return slices.Contains(afterPersonas, completedPersona)
}

// legacyAfterPersonas returns handler-declared AfterPersonas merged with hooks.
// Used only for gRPC fallback path.
func (r *PersonaRunner) legacyAfterPersonas(h handler.Handler) []string {
	var base []string
	if th, ok := h.(handler.TriggeredHandler); ok {
		base = th.Trigger().AfterPersonas
	}
	r.hooksMu.RLock()
	hooks := r.hooks[h.Name()]
	r.hooksMu.RUnlock()
	if len(hooks) == 0 {
		return base
	}
	merged := make([]string, 0, len(base)+len(hooks))
	merged = append(merged, base...)
	merged = append(merged, hooks...)
	return merged
}

// isRetriggerable checks whether the handler is in RetriggeredBy for the
// workflow associated with the given correlationID.
func (r *PersonaRunner) isRetriggerable(handlerName, correlationID string) bool {
	wfID, ok := r.resolveWorkflowID(correlationID)
	if !ok {
		// No cached workflowID — fall back: check if handler has FeedbackGenerated
		// in its declared triggers (gRPC compat).
		r.handlersMu.RLock()
		h, exists := r.handlers[handlerName]
		r.handlersMu.RUnlock()
		if !exists {
			return false
		}
		return slices.Contains(handlerDeclaredEvents(h), event.FeedbackGenerated)
	}

	def, ok := r.getWorkflowDef(wfID)
	if !ok {
		return false
	}

	retriggerEvents := def.RetriggeredBy[handlerName]
	return slices.Contains(retriggerEvents, event.FeedbackGenerated)
}

// effectiveAfterPersonas returns the full set of personas that must have
// completed before this handler can dispatch. Sources:
// 1. DAG predecessors from the workflow Graph (if correlation is cached)
// 2. Before-hooks
// Falls back to handler-declared AfterPersonas for gRPC compat.
func (r *PersonaRunner) effectiveAfterPersonas(h handler.Handler, correlationID string) []string {
	var base []string

	wfID, ok := r.resolveWorkflowID(correlationID)
	if ok {
		def, defOk := r.getWorkflowDef(wfID)
		if defOk {
			if deps, inGraph := def.Graph[h.Name()]; inGraph {
				base = deps
			}
		}
	}

	// If not resolved from DAG, fall back to handler-declared triggers.
	if base == nil && !ok {
		if th, ok := h.(handler.TriggeredHandler); ok {
			base = th.Trigger().AfterPersonas
		}
	}

	// Merge hooks.
	r.hooksMu.RLock()
	hooks := r.hooks[h.Name()]
	r.hooksMu.RUnlock()
	if len(hooks) == 0 {
		return base
	}
	merged := make([]string, 0, len(base)+len(hooks))
	merged = append(merged, base...)
	merged = append(merged, hooks...)
	return merged
}

// registerHinter checks if a handler implements Hinter and tracks it.
func (r *PersonaRunner) registerHinter(h handler.Handler) {
	if hinter, ok := h.(handler.Hinter); ok {
		r.hinters[h.Name()] = hinter
	}
}

// enqueueAndDrain adds an event to the handler's per-correlation priority queue
// and starts draining if no drain goroutine is active.
func (r *PersonaRunner) enqueueAndDrain(h handler.Handler, env event.Envelope, chainDepth int) {
	key := h.Name() + "|" + env.CorrelationID

	r.queuesMu.Lock()
	q, exists := r.queues[key]
	if !exists {
		q = &dispatchQueue{}
		r.queues[key] = q
	}
	r.queuesMu.Unlock()

	q.mu.Lock()
	q.push(dispatchItem{
		priority:   eventPriority(env.Type),
		env:        env,
		chainDepth: chainDepth,
	})
	if q.draining {
		q.mu.Unlock()
		return // another goroutine is already draining
	}
	q.draining = true
	q.mu.Unlock()

	// This goroutine becomes the drain worker.
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			q.mu.Lock()
			item, ok := q.pop()
			if !ok {
				q.draining = false
				q.mu.Unlock()
				return
			}
			q.mu.Unlock()

			r.executeDispatch(h, item.env, item.chainDepth)
		}
	}()
}

// executeDispatch runs the handler, emits PersonaCompleted/PersonaFailed,
// persists to the persona-scoped aggregate, and publishes resulting events.
func (r *PersonaRunner) executeDispatch(h handler.Handler, env event.Envelope, chainDepth int) {
	r.active.Add(1)
	defer r.active.Add(-1)

	// Two-phase hint: if handler implements Hinter and this isn't a
	// HintApproved replay, run the hint phase instead of full dispatch.
	if hinter, ok := r.hinters[h.Name()]; ok && env.Type != event.HintApproved {
		r.executeHint(hinter, h, env, chainDepth)
		return
	}

	start := time.Now()
	result, dispatchErr := r.dispatcher.Dispatch(r.ctx, h.Name(), env)
	durationMS := time.Since(start).Milliseconds()

	// Incomplete: handler ran successfully but has more work to do.
	if errors.Is(dispatchErr, handler.ErrIncomplete) {
		r.persistAndPublishResultOnly(h.Name(), env, result)
		r.logger.Info("persona runner: handler incomplete, awaiting future events",
			slog.String("handler", h.Name()),
			slog.String("correlation", env.CorrelationID),
			slog.Int64("duration_ms", durationMS),
		)
		return
	}

	// Build persona-scoped aggregate ID
	aggregateID := env.CorrelationID + ":persona:" + h.Name()

	var allEvents []event.Envelope

	// Collect handler result events
	if result != nil {
		for _, re := range result.Events {
			allEvents = append(allEvents, re.
				WithCorrelation(env.CorrelationID).
				WithCausation(env.ID))
		}
	}

	// Build PersonaCompleted or PersonaFailed
	if dispatchErr != nil {
		allEvents = append(allEvents, event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
			Persona:      h.Name(),
			TriggerEvent: string(env.Type),
			TriggerID:    string(env.ID),
			Reactive:     true,
			Error:        dispatchErr.Error(),
			DurationMS:   durationMS,
			ChainDepth:   chainDepth,
		})).
			WithCausation(env.ID).
			WithCorrelation(env.CorrelationID).
			WithSource("persona-runner:"+h.Name()))
	} else {
		outputRef := ""
		if result != nil {
			for _, re := range result.Events {
				if re.Type == event.AIResponseReceived {
					outputRef = string(re.ID)
				}
			}
		}
		allEvents = append(allEvents, event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
			Persona:      h.Name(),
			TriggerEvent: string(env.Type),
			TriggerID:    string(env.ID),
			Reactive:     true,
			OutputRef:    outputRef,
			DurationMS:   durationMS,
			ChainDepth:   chainDepth,
		})).
			WithCausation(env.ID).
			WithCorrelation(env.CorrelationID).
			WithSource("persona-runner:"+h.Name()))
	}

	// Persist to persona-scoped aggregate with retry.
	const maxPersistRetries = 3
	for attempt := range maxPersistRetries {
		currentVersion := 0
		if existing, loadErr := r.store.Load(r.ctx, aggregateID); loadErr == nil && len(existing) > 0 {
			currentVersion = existing[len(existing)-1].Version
		}
		for i := range allEvents {
			allEvents[i] = allEvents[i].WithAggregate(aggregateID, currentVersion+i+1)
		}
		persistErr := r.store.Append(r.ctx, aggregateID, currentVersion, allEvents)
		if persistErr == nil {
			break
		}
		if attempt == maxPersistRetries-1 {
			r.logger.Error("persona runner: persist failed after retries",
				slog.String("handler", h.Name()),
				slog.String("error", persistErr.Error()),
			)
		}
	}

	// Publish
	for _, ne := range allEvents {
		if pubErr := r.bus.Publish(r.ctx, ne); pubErr != nil {
			r.logger.Error("persona runner: publish failed",
				slog.String("event_type", string(ne.Type)),
				slog.String("error", pubErr.Error()),
			)
		}
	}
}

// checkJoinCondition returns true when all requiredPersonas have a
// PersonaCompleted event recorded under the given correlationID.
func (r *PersonaRunner) checkJoinCondition(requiredPersonas []string, correlationID string) (bool, string) {
	events, err := r.store.LoadByCorrelation(r.ctx, correlationID)
	if err != nil {
		r.logger.Error("persona runner: join check failed",
			slog.String("correlation", correlationID),
			slog.String("error", err.Error()),
		)
		return false, ""
	}

	// Resolve workflow def for phase-name → persona-name mapping.
	var wfDef *WorkflowDef
	if wfID, ok := r.resolveWorkflowID(correlationID); ok {
		if def, ok := r.getWorkflowDef(wfID); ok {
			wfDef = &def
		}
	}

	// Track the latest PersonaCompleted event ID per persona, and detect
	// active fail verdicts per source persona.
	//
	// Within a single handler execution, events are always ordered:
	//   ... → VerdictRendered → PersonaCompleted
	// A re-run (after feedback) produces a second PersonaCompleted.
	// Events from LoadByCorrelation are ordered by timestamp.
	//
	// State machine per source persona (verdictTracker):
	//   VerdictRendered{fail} → active=true, sealed=false
	//   VerdictRendered{pass} → active=false, sealed=false
	//   PersonaCompleted (1st after verdict) → seal the verdict (same execution)
	//   PersonaCompleted (2nd+) → re-run clears the verdict
	type verdictTracker struct {
		active bool // fail verdict from the current execution
		sealed bool // true after the first PC following the verdict
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
						// Second+ PC after the verdict — this is a re-run.
						// The old verdict is superseded.
						delete(verdicts, pc.Persona)
					} else {
						// First PC after the verdict — same execution, seal it.
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
		}
	}

	ids := make([]string, 0, len(requiredPersonas))
	for _, req := range requiredPersonas {
		id, ok := latestByPersona[req]
		if !ok {
			return false, ""
		}
		// A predecessor with an active fail verdict has triggered a feedback
		// loop — its PersonaCompleted is stale and must not satisfy downstream
		// joins until the loop completes and it re-runs with a passing
		// (or no) verdict.
		if vt := verdicts[req]; vt != nil && vt.active {
			return false, ""
		}
		ids = append(ids, id)
	}

	sort.Strings(ids)
	return true, strings.Join(ids, "|")
}

// =============================================================================
// Pause / Resume / Cancel
// =============================================================================

// isPaused returns true if the given correlation is paused.
func (r *PersonaRunner) isPaused(correlationID string) bool {
	r.pausedMu.RLock()
	defer r.pausedMu.RUnlock()
	return r.paused[correlationID]
}

// addBlocked records a blocked dispatch for replay on resume.
func (r *PersonaRunner) addBlocked(correlationID string, h handler.Handler, env event.Envelope) {
	r.pausedMu.Lock()
	defer r.pausedMu.Unlock()
	r.blocked[correlationID] = append(r.blocked[correlationID], blockedDispatch{handler: h, env: env})
}

// subscribePauseResume wires the PersonaRunner to pause/resume events.
func (r *PersonaRunner) subscribePauseResume() {
	unsub1 := r.bus.Subscribe(event.WorkflowPaused, func(_ context.Context, env event.Envelope) error {
		corrID := env.CorrelationID
		if corrID == "" {
			corrID = env.AggregateID
		}
		r.pausedMu.Lock()
		r.paused[corrID] = true
		r.pausedMu.Unlock()
		r.logger.Info("persona runner: workflow paused", slog.String("correlation", corrID))
		return nil
	}, eventbus.WithName("persona-runner:pause"))

	unsub2 := r.bus.Subscribe(event.WorkflowResumed, func(_ context.Context, env event.Envelope) error {
		corrID := env.CorrelationID
		if corrID == "" {
			corrID = env.AggregateID
		}
		r.pausedMu.Lock()
		delete(r.paused, corrID)
		replay := r.blocked[corrID]
		delete(r.blocked, corrID)
		r.pausedMu.Unlock()

		r.logger.Info("persona runner: workflow resumed",
			slog.String("correlation", corrID),
			slog.Int("replaying", len(replay)),
		)

		// Replay blocked dispatches — clear dedup entries first.
		for _, b := range replay {
			r.seen.Remove(b.handler.Name(), string(b.env.ID))
			wrapped := r.wrap(b.handler)
			go func(fn eventbus.HandlerFunc, env event.Envelope) {
				_ = fn(r.ctx, env)
			}(wrapped, b.env)
		}
		return nil
	}, eventbus.WithName("persona-runner:resume"))

	unsub3 := r.bus.Subscribe(event.WorkflowCancelled, func(_ context.Context, env event.Envelope) error {
		corrID := env.CorrelationID
		if corrID == "" {
			corrID = env.AggregateID
		}
		r.pausedMu.Lock()
		r.paused[corrID] = true // treat cancelled as permanently paused
		delete(r.blocked, corrID)
		r.pausedMu.Unlock()
		r.logger.Info("persona runner: workflow cancelled", slog.String("correlation", corrID))
		return nil
	}, eventbus.WithName("persona-runner:cancel"))

	r.unsubs = append(r.unsubs, unsub1, unsub2, unsub3)
}

// =============================================================================
// Hint support
// =============================================================================

// subscribeHintApproved subscribes to HintApproved events. When received, it
// loads the original triggering event and dispatches the full Handle().
func (r *PersonaRunner) subscribeHintApproved() {
	unsub := r.bus.Subscribe(event.HintApproved, func(_ context.Context, env event.Envelope) error {
		var p event.HintApprovedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil
		}

		// Load the original triggering event from the correlation chain.
		events, err := r.store.LoadByCorrelation(r.ctx, env.CorrelationID)
		if err != nil {
			r.logger.Error("persona runner: hint approved but failed to load events",
				slog.String("persona", p.Persona),
				slog.String("error", err.Error()),
			)
			return nil
		}

		var originalEnv event.Envelope
		found := false
		for _, e := range events {
			if string(e.ID) == p.TriggerID {
				originalEnv = e
				found = true
				break
			}
		}
		if !found {
			r.logger.Warn("persona runner: hint approved but trigger event not found",
				slog.String("persona", p.Persona),
				slog.String("trigger_id", p.TriggerID),
			)
			return nil
		}

		// Clear dedup so the replay isn't suppressed.
		r.seen.Remove(p.Persona, string(originalEnv.ID))

		replayEnv := env
		replayEnv.CorrelationID = originalEnv.CorrelationID

		r.queuesMu.Lock()
		key := p.Persona + "|" + env.CorrelationID
		q, exists := r.queues[key]
		if !exists {
			q = &dispatchQueue{}
			r.queues[key] = q
		}
		r.queuesMu.Unlock()

		q.mu.Lock()
		q.push(dispatchItem{
			priority:   PriorityOperatorGuidance,
			env:        replayEnv,
			chainDepth: 0,
		})
		if q.draining {
			q.mu.Unlock()
			return nil
		}
		q.draining = true
		q.mu.Unlock()

		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			for {
				q.mu.Lock()
				item, ok := q.pop()
				if !ok {
					q.draining = false
					q.mu.Unlock()
					return
				}
				q.mu.Unlock()

				r.executeHintApprovedDispatch(p.Persona, item.env, item.chainDepth)
			}
		}()

		return nil
	}, eventbus.WithName("persona-runner:hint-approved"))
	r.unsubs = append(r.unsubs, unsub)
}

// executeHint runs the hint phase for a Hinter handler.
func (r *PersonaRunner) executeHint(hinter handler.Hinter, h handler.Handler, env event.Envelope, chainDepth int) {
	start := time.Now()
	hintEvents, err := hinter.Hint(r.ctx, env)
	durationMS := time.Since(start).Milliseconds()

	aggregateID := env.CorrelationID + ":persona:" + h.Name()

	var allEvents []event.Envelope
	if err != nil {
		allEvents = append(allEvents, event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
			Persona:      h.Name(),
			TriggerEvent: string(env.Type),
			TriggerID:    string(env.ID),
			Reactive:     true,
			Error:        fmt.Sprintf("hint failed: %s", err.Error()),
			DurationMS:   durationMS,
			ChainDepth:   chainDepth,
		})).
			WithCausation(env.ID).
			WithCorrelation(env.CorrelationID).
			WithSource("persona-runner:"+h.Name()))
	} else {
		for _, he := range hintEvents {
			allEvents = append(allEvents, he.
				WithCorrelation(env.CorrelationID).
				WithCausation(env.ID))
		}
	}

	const maxRetries = 3
	for attempt := range maxRetries {
		currentVersion := 0
		if existing, loadErr := r.store.Load(r.ctx, aggregateID); loadErr == nil && len(existing) > 0 {
			currentVersion = existing[len(existing)-1].Version
		}
		for i := range allEvents {
			allEvents[i] = allEvents[i].WithAggregate(aggregateID, currentVersion+i+1)
		}
		if persistErr := r.store.Append(r.ctx, aggregateID, currentVersion, allEvents); persistErr == nil {
			break
		} else if attempt == maxRetries-1 {
			r.logger.Error("persona runner: hint persist failed",
				slog.String("handler", h.Name()),
				slog.String("error", persistErr.Error()),
			)
		}
	}

	for _, ne := range allEvents {
		if pubErr := r.bus.Publish(r.ctx, ne); pubErr != nil {
			r.logger.Error("persona runner: hint publish failed",
				slog.String("event_type", string(ne.Type)),
				slog.String("error", pubErr.Error()),
			)
		}
	}
}

// executeHintApprovedDispatch runs full Handle() for a handler after hint approval.
func (r *PersonaRunner) executeHintApprovedDispatch(handlerName string, env event.Envelope, chainDepth int) {
	r.active.Add(1)
	defer r.active.Add(-1)

	start := time.Now()
	result, dispatchErr := r.dispatcher.Dispatch(r.ctx, handlerName, env)
	durationMS := time.Since(start).Milliseconds()

	if errors.Is(dispatchErr, handler.ErrIncomplete) {
		r.persistAndPublishResultOnly(handlerName, env, result)
		r.logger.Info("persona runner: handler incomplete (hint-approved), awaiting future events",
			slog.String("handler", handlerName),
			slog.String("correlation", env.CorrelationID),
			slog.Int64("duration_ms", durationMS),
		)
		return
	}

	aggregateID := env.CorrelationID + ":persona:" + handlerName

	var allEvents []event.Envelope
	if result != nil {
		for _, re := range result.Events {
			allEvents = append(allEvents, re.
				WithCorrelation(env.CorrelationID).
				WithCausation(env.ID))
		}
	}

	if dispatchErr != nil {
		allEvents = append(allEvents, event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
			Persona:      handlerName,
			TriggerEvent: string(env.Type),
			TriggerID:    string(env.ID),
			Reactive:     true,
			Error:        dispatchErr.Error(),
			DurationMS:   durationMS,
			ChainDepth:   chainDepth,
		})).
			WithCausation(env.ID).
			WithCorrelation(env.CorrelationID).
			WithSource("persona-runner:"+handlerName))
	} else {
		outputRef := ""
		if result != nil {
			for _, re := range result.Events {
				if re.Type == event.AIResponseReceived {
					outputRef = string(re.ID)
				}
			}
		}
		allEvents = append(allEvents, event.New(event.PersonaCompleted, 1, event.MustMarshal(event.PersonaCompletedPayload{
			Persona:      handlerName,
			TriggerEvent: string(env.Type),
			TriggerID:    string(env.ID),
			Reactive:     true,
			OutputRef:    outputRef,
			DurationMS:   durationMS,
			ChainDepth:   chainDepth,
		})).
			WithCausation(env.ID).
			WithCorrelation(env.CorrelationID).
			WithSource("persona-runner:"+handlerName))
	}

	const maxRetries = 3
	for attempt := range maxRetries {
		currentVersion := 0
		if existing, loadErr := r.store.Load(r.ctx, aggregateID); loadErr == nil && len(existing) > 0 {
			currentVersion = existing[len(existing)-1].Version
		}
		for i := range allEvents {
			allEvents[i] = allEvents[i].WithAggregate(aggregateID, currentVersion+i+1)
		}
		if persistErr := r.store.Append(r.ctx, aggregateID, currentVersion, allEvents); persistErr == nil {
			break
		} else if attempt == maxRetries-1 {
			r.logger.Error("persona runner: hint-approved persist failed",
				slog.String("handler", handlerName),
				slog.String("error", persistErr.Error()),
			)
		}
	}

	for _, ne := range allEvents {
		if pubErr := r.bus.Publish(r.ctx, ne); pubErr != nil {
			r.logger.Error("persona runner: publish failed",
				slog.String("event_type", string(ne.Type)),
				slog.String("error", pubErr.Error()),
			)
		}
	}
}

// persistAndPublishResultOnly persists handler result events without
// PersonaCompleted/PersonaFailed. Used for ErrIncomplete handlers.
func (r *PersonaRunner) persistAndPublishResultOnly(handlerName string, env event.Envelope, result *DispatchResult) {
	aggregateID := env.CorrelationID + ":persona:" + handlerName

	var allEvents []event.Envelope
	if result != nil {
		for _, re := range result.Events {
			allEvents = append(allEvents, re.
				WithCorrelation(env.CorrelationID).
				WithCausation(env.ID))
		}
	}
	if len(allEvents) == 0 {
		return
	}

	const maxPersistRetries = 3
	for attempt := range maxPersistRetries {
		currentVersion := 0
		if existing, loadErr := r.store.Load(r.ctx, aggregateID); loadErr == nil && len(existing) > 0 {
			currentVersion = existing[len(existing)-1].Version
		}
		for i := range allEvents {
			allEvents[i] = allEvents[i].WithAggregate(aggregateID, currentVersion+i+1)
		}
		if persistErr := r.store.Append(r.ctx, aggregateID, currentVersion, allEvents); persistErr == nil {
			break
		} else if attempt == maxPersistRetries-1 {
			r.logger.Error("persona runner: incomplete persist failed",
				slog.String("handler", handlerName),
				slog.String("error", persistErr.Error()),
			)
		}
	}

	for _, ne := range allEvents {
		if pubErr := r.bus.Publish(r.ctx, ne); pubErr != nil {
			r.logger.Error("persona runner: incomplete publish failed",
				slog.String("event_type", string(ne.Type)),
				slog.String("error", pubErr.Error()),
			)
		}
	}
}

// =============================================================================
// Dispatch Queue — per-(handler, correlation) priority queue
// =============================================================================

// dispatchItem is a single queued event with its dispatch priority.
type dispatchItem struct {
	priority   int // lower = higher priority
	env        event.Envelope
	chainDepth int
}

// dispatchQueue is a per-(handler, correlation) priority queue.
type dispatchQueue struct {
	mu       sync.Mutex
	items    []dispatchItem
	draining bool
}

// push adds an item to the queue in priority order (stable: FIFO within same priority).
func (q *dispatchQueue) push(item dispatchItem) {
	pos := len(q.items)
	for i, existing := range q.items {
		if item.priority < existing.priority {
			pos = i
			break
		}
	}
	q.items = append(q.items, dispatchItem{})
	copy(q.items[pos+1:], q.items[pos:])
	q.items[pos] = item
}

// pop removes and returns the highest-priority (lowest value) item.
func (q *dispatchQueue) pop() (dispatchItem, bool) {
	if len(q.items) == 0 {
		return dispatchItem{}, false
	}
	item := q.items[0]
	q.items = q.items[1:]
	return item, true
}

// =============================================================================
// Support types
// =============================================================================

// idempotencyCache is a bounded LRU-style dedup cache for (handlerName, eventID) pairs.
type idempotencyCache struct {
	mu      sync.Mutex
	entries map[string]struct{}
	order   []string
	maxSize int
}

func newIdempotencyCache(maxSize int) *idempotencyCache {
	return &idempotencyCache{
		entries: make(map[string]struct{}, maxSize),
		order:   make([]string, 0, maxSize),
		maxSize: maxSize,
	}
}

// Remove deletes a key from the cache, allowing it to be re-added.
func (c *idempotencyCache) Remove(handlerName, eventID string) {
	key := handlerName + "|" + eventID
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// Add returns true if the key was new, false if already seen.
func (c *idempotencyCache) Add(handlerName, eventID string) bool {
	key := handlerName + "|" + eventID
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; exists {
		return false
	}
	if len(c.order) >= c.maxSize {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
	c.entries[key] = struct{}{}
	c.order = append(c.order, key)
	return true
}

// blockedDispatch records a handler dispatch that was blocked due to pause.
type blockedDispatch struct {
	handler handler.Handler
	env     event.Envelope
}

