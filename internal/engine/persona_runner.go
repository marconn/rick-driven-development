package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	pauser *pauseController

	// Per-correlation contexts for cancellation propagation.
	corrCtxs *correlationContexts

	// Before-hooks: persona name → additional personas that must complete first.
	hooks *hookRegistry

	// Shared persist-and-publish logic for dispatch results.
	persister *resultPersister

	// Per-(handler, correlation) dispatch queues for serial execution + priority.
	queues *dispatchQueues

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
	resolver *workflowResolver
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
		r.hooks.register(persona, hookPersonas...)
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
		pauser:       newPauseController(),
		hooks:        newHookRegistry(nil),
		persister:    &resultPersister{store: store, bus: bus, logger: logger},
		queues:       newDispatchQueues(),
		hinters:      make(map[string]handler.Hinter),
		handlers:     make(map[string]handler.Handler),
		dynamicGen:   make(map[string]uint64),
		resolver:     newWorkflowResolver(store, logger),
		corrCtxs:     newCorrelationContexts(),
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
	r.resolver.registerWorkflow(def)
}

// RecoverDispatch directly enqueues a handler for dispatch, bypassing the bus.
// Used by RecoveryScanner to resume handlers that were in-flight before restart.
// Returns an error if the handler is not found or the join condition is not met.
func (r *PersonaRunner) RecoverDispatch(handlerName string, env event.Envelope) error {
	r.handlersMu.RLock()
	h, ok := r.handlers[handlerName]
	r.handlersMu.RUnlock()
	if !ok {
		return fmt.Errorf("persona runner: recover dispatch: handler %q not found", handlerName)
	}

	// Safety: verify join condition against the store.
	afterPersonas := r.resolver.effectiveAfterPersonas(h, env.CorrelationID, r.hooks)
	if len(afterPersonas) > 0 && env.CorrelationID != "" {
		satisfied, _ := r.resolver.checkJoinCondition(r.ctx, afterPersonas, env.CorrelationID)
		if !satisfied {
			return fmt.Errorf("persona runner: recover dispatch: join unsatisfied for %q", handlerName)
		}
	}

	if r.pauser.isPaused(env.CorrelationID) {
		r.pauser.addBlocked(env.CorrelationID, h, env)
		r.logger.Info("persona runner: recovery deferred (workflow paused)",
			slog.String("handler", handlerName),
			slog.String("correlation", env.CorrelationID),
		)
		return nil
	}

	r.logger.Info("persona runner: recovery dispatch",
		slog.String("handler", handlerName),
		slog.String("correlation", env.CorrelationID),
		slog.String("trigger_type", string(env.Type)),
	)
	r.enqueueAndDrain(h, env, 0)
	return nil
}

// WarmCorrelationCache populates the correlationID→workflowID mapping for
// workflows that existed before this server instance started.
func (r *PersonaRunner) WarmCorrelationCache(correlationID, workflowID string) {
	r.resolver.cacheWorkflowID(correlationID, workflowID)
}

// WarmPauseState marks a correlation as paused in the pause controller.
// Used by the recovery scanner to restore pause state from durable aggregate status.
func (r *PersonaRunner) WarmPauseState(correlationID string) {
	r.pauser.pause(correlationID)
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
		events := r.resolver.resolveEventsFromDAG(h, r.hooks)
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
	events := r.resolver.resolveEventsFromDAG(h, r.hooks)
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
	r.hooks.register(persona, hookPersonas...)

	// If the gated handler is already registered, ensure it's subscribed
	// to persona.completed so it gets re-evaluated when hook handlers complete.
	r.handlersMu.RLock()
	h, exists := r.handlers[persona]
	r.handlersMu.RUnlock()

	if exists {
		// Check if handler is already subscribed to PersonaCompleted via DAG
		// (i.e., it has non-empty predecessors in any workflow Graph).
		alreadySubscribed := r.resolver.handlerInAnyGraphAsNonRoot(persona)
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
	r.hooks.unregister(persona, hookName)
	r.logger.Info("persona runner: dynamic hook unregistered",
		slog.String("target", persona),
		slog.String("hook", hookName),
	)
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
			r.resolver.cacheWorkflowID(corrID, workflowID)
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
				r.resolver.evictCorrelation(corrID)
				r.corrCtxs.cancel(corrID)
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

	// Cancel all per-correlation contexts first so in-flight handlers unblock.
	r.corrCtxs.cancelAll()

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
				if !r.resolver.isDAGRelevant(h, pc.Persona, env.CorrelationID, r.hooks) {
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
			if !r.resolver.isRetriggerable(h.Name(), env.CorrelationID, r.handlerLookup) {
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
			r.logger.Debug("persona runner: dispatch dropped (event dedup)",
				slog.String("handler", h.Name()),
				slog.String("event_type", string(env.Type)),
				slog.String("event_id", string(env.ID)),
				slog.String("correlation", env.CorrelationID),
			)
			return nil
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
		afterPersonas := r.resolver.effectiveAfterPersonas(h, env.CorrelationID, r.hooks)
		if len(afterPersonas) > 0 && env.CorrelationID != "" {
			satisfied, fingerprint := r.resolver.checkJoinCondition(r.ctx, afterPersonas, env.CorrelationID)
			if !satisfied {
				r.logger.Debug("persona runner: dispatch dropped (join unsatisfied)",
					slog.String("handler", h.Name()),
					slog.String("event_type", string(env.Type)),
					slog.String("event_id", string(env.ID)),
					slog.String("correlation", env.CorrelationID),
				)
				return nil
			}
			// Join-gate dedup: when multiple PersonaCompleted events
			// satisfy the same join, dispatch only once per unique set.
			if len(afterPersonas) > 1 && env.Type == event.PersonaCompleted {
				if !r.seen.Add(h.Name()+":join", fingerprint) {
					r.logger.Debug("persona runner: dispatch dropped (join-gate dedup)",
						slog.String("handler", h.Name()),
						slog.String("event_type", string(env.Type)),
						slog.String("correlation", env.CorrelationID),
						slog.String("fingerprint", fingerprint),
					)
					return nil
				}
			}
		}

		// 7. Pause check
		if r.pauser.isPaused(env.CorrelationID) {
			r.pauser.addBlocked(env.CorrelationID, h, env)
			return nil
		}

		// 8. Check runner context
		if r.ctx.Err() != nil {
			r.logger.Debug("persona runner: dispatch dropped (context cancelled)",
				slog.String("handler", h.Name()),
				slog.String("event_type", string(env.Type)),
				slog.String("correlation", env.CorrelationID),
			)
			return nil
		}

		// 9. Enqueue into the per-(handler, correlation) priority queue.
		r.enqueueAndDrain(h, env, chainDepth)
		return nil
	}
}

// handlerLookup returns the handler for the given name, for use by workflowResolver.
func (r *PersonaRunner) handlerLookup(name string) (handler.Handler, bool) {
	r.handlersMu.RLock()
	h, ok := r.handlers[name]
	r.handlersMu.RUnlock()
	return h, ok
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
	q := r.queues.getOrCreate(key)

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

	dispatchCtx := r.corrCtxs.get(r.ctx, env.CorrelationID)

	start := time.Now()
	result, dispatchErr := r.dispatcher.Dispatch(dispatchCtx, h.Name(), env)
	durationMS := time.Since(start).Milliseconds()

	// Workflow was cancelled while handler was running — drop the result
	// silently. The workflow is already terminal; emitting PersonaCompleted/Failed
	// would be orphaned noise.
	if dispatchCtx.Err() != nil && r.pauser.isPaused(env.CorrelationID) {
		r.logger.Info("persona runner: handler cancelled by workflow",
			slog.String("handler", h.Name()),
			slog.String("correlation", env.CorrelationID),
			slog.Int64("duration_ms", durationMS),
		)
		return
	}

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

	r.persister.persistAndPublish(r.ctx, aggregateID, allEvents)
}


// =============================================================================
// Pause / Resume / Cancel
// =============================================================================


// subscribePauseResume wires the PersonaRunner to pause/resume events.
func (r *PersonaRunner) subscribePauseResume() {
	unsub1 := r.bus.Subscribe(event.WorkflowPaused, func(_ context.Context, env event.Envelope) error {
		corrID := env.CorrelationID
		if corrID == "" {
			corrID = env.AggregateID
		}
		r.pauser.pause(corrID)
		r.logger.Info("persona runner: workflow paused", slog.String("correlation", corrID))
		return nil
	}, eventbus.WithName("persona-runner:pause"))

	unsub2 := r.bus.Subscribe(event.WorkflowResumed, func(_ context.Context, env event.Envelope) error {
		corrID := env.CorrelationID
		if corrID == "" {
			corrID = env.AggregateID
		}
		replay := r.pauser.resume(corrID)

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
		r.pauser.markCancelled(corrID)
		r.corrCtxs.cancel(corrID)
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

		key := p.Persona + "|" + env.CorrelationID
		q := r.queues.getOrCreate(key)

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
	hintCtx := r.corrCtxs.get(r.ctx, env.CorrelationID)

	start := time.Now()
	hintEvents, err := hinter.Hint(hintCtx, env)
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

	r.persister.persistAndPublish(r.ctx, aggregateID, allEvents)
}

// executeHintApprovedDispatch runs full Handle() for a handler after hint approval.
func (r *PersonaRunner) executeHintApprovedDispatch(handlerName string, env event.Envelope, chainDepth int) {
	r.active.Add(1)
	defer r.active.Add(-1)

	dispatchCtx := r.corrCtxs.get(r.ctx, env.CorrelationID)

	start := time.Now()
	result, dispatchErr := r.dispatcher.Dispatch(dispatchCtx, handlerName, env)
	durationMS := time.Since(start).Milliseconds()

	if dispatchCtx.Err() != nil && r.pauser.isPaused(env.CorrelationID) {
		r.logger.Info("persona runner: handler cancelled by workflow",
			slog.String("handler", handlerName),
			slog.String("correlation", env.CorrelationID),
			slog.Int64("duration_ms", durationMS),
		)
		return
	}

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

	r.persister.persistAndPublish(r.ctx, aggregateID, allEvents)
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

	r.persister.persistAndPublish(r.ctx, aggregateID, allEvents)
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

// corrCtxEntry holds a per-correlation context and its cancel function.
// Used to propagate workflow cancellation to in-flight handler dispatches.
type corrCtxEntry struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// blockedDispatch records a handler dispatch that was blocked due to pause.
type blockedDispatch struct {
	handler handler.Handler
	env     event.Envelope
}

