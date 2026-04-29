package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/buildinfo"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

const (
	defaultDrainTimeout = 30 * time.Second
	defaultMaxChain     = 5
	// defaultMaxActive caps concurrent handler execution. Sized well above
	// the widest fan-out in a single workflow (pr-review's parallel
	// category-reviewer dispatch) so a single workflow can never fully
	// saturate the pool on its own. Beyond the cap, dispatches block at the
	// semaphore rather than being dropped. Revisit this if the reviewer list
	// ever grows past ~24.
	defaultMaxActive = 32
	defaultDedup     = 10000
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

// Dispatch-drop reason taxonomy. Every drop path emits a DispatchDropped
// event to the {correlationID}:drops aggregate and a structured log line
// tagged with drop_reason. Log levels are calibrated: reasons that fire
// during normal parallel fan-out stay at Debug (join_unsatisfied,
// join_gate_dedup) so live log tails don't drown in expected chatter,
// while anomalous reasons (event_dedup, ctx_cancelled, store_error) log
// at Warn/Error. The DispatchDropped event is always persisted regardless
// of log level, so post-hoc SQL analysis is unaffected.
const (
	// dropReasonEventDedup: the same (handler, event.ID) pair was already
	// admitted. Expected on retriggers that hit the same event twice.
	dropReasonEventDedup = "event_dedup"
	// dropReasonJoinUnsatisfied: handler has DAG predecessors that haven't
	// yet completed. Expected — handler will re-evaluate when each missing
	// predecessor completes. If this fires repeatedly for the same handler
	// after all predecessors HAVE completed, that's a wedge.
	dropReasonJoinUnsatisfied = "join_unsatisfied"
	// dropReasonJoinGateDedup: the same set of predecessor completions
	// already triggered this handler. Expected on parallel fan-out (N-1
	// drops per N-way join). A single dispatch is guaranteed.
	dropReasonJoinGateDedup = "join_gate_dedup"
	// dropReasonCtxCancelled: the runner's context was cancelled
	// (shutdown, workflow cancel). No more dispatches will be admitted.
	dropReasonCtxCancelled = "ctx_cancelled"
	// dropReasonStoreError: checkJoinCondition's LoadByCorrelation failed.
	// The dispatch is retried once; if the retry also fails, this reason
	// fires and the dispatch is dropped. Transient store errors look
	// identical to "join unsatisfied" without this distinction.
	dropReasonStoreError = "store_error"
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
	// defaultChainDepth is the fallback chain-depth cap used when no workflow
	// definition is found for a correlation (e.g., unregistered correlation or
	// event with no correlation ID). Per-workflow limits are looked up via the
	// resolver using WorkflowDef.EffectiveMaxChainDepth().
	defaultChainDepth int
	maxActive         int32 // max concurrent reactive handlers (total cap)
	// fair throttles concurrent handler execution with per-correlation fairness.
	// It replaces the old flat channel semaphore. Each dispatch acquires a slot
	// keyed by correlationID before running and releases it after. When the total
	// cap is reached, drain goroutines block rather than dropping events —
	// preserving per-(handler, correlation) queue ordering. Unlike the old
	// semaphore, the fairDispatcher allocates slots proportionally across active
	// correlations so one loud workflow (e.g., pr-review with its parallel
	// category-reviewer fan-out) cannot starve another workflow's critical-path handlers.
	fair *fairDispatcher
	seen *idempotencyCache

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

// WithMaxChainDepth sets the package-level fallback chain-depth cap used when
// no workflow definition can be resolved for a correlation (e.g., unknown
// correlation or event with no correlation ID). Per-workflow limits are
// derived from WorkflowDef.EffectiveMaxChainDepth() and always take
// precedence over this value.
func WithMaxChainDepth(n int) PersonaRunnerOption {
	return func(r *PersonaRunner) { r.defaultChainDepth = n }
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
		store:             store,
		bus:               bus,
		dispatcher:        dispatcher,
		logger:            logger,
		drainTimeout:      defaultDrainTimeout,
		defaultChainDepth: defaultMaxChain,
		maxActive:         int32(defaultMaxActive),
		seen:              newIdempotencyCache(defaultDedup),
		pauser:            newPauseController(),
		hooks:             newHookRegistry(nil),
		persister:         &resultPersister{store: store, bus: bus, logger: logger},
		queues:            newDispatchQueues(),
		hinters:           make(map[string]handler.Hinter),
		handlers:          make(map[string]handler.Handler),
		dynamicGen:        make(map[string]uint64),
		resolver:          newWorkflowResolver(store, logger),
		corrCtxs:          newCorrelationContexts(),
	}
	for _, opt := range opts {
		opt(r)
	}
	// Build the fair dispatcher after options so WithMaxActive is honored.
	r.fair = newFairDispatcher(int(r.maxActive))
	return r
}

// RunnerSnapshot reports dispatcher saturation for observability. Active is
// the current number of handlers holding a slot; MaxActive is the configured
// cap. InflightByCorrelation provides a per-correlation breakdown to help
// diagnose which workflows are consuming the most concurrency budget.
type RunnerSnapshot struct {
	Active                int32
	MaxActive             int32
	InflightByCorrelation map[string]int
}

// Snapshot returns a point-in-time read of runner load. Active and MaxActive
// are suitable for alerting on saturation; InflightByCorrelation is suitable
// for debugging which workflow is dominating the concurrency pool.
func (r *PersonaRunner) Snapshot() RunnerSnapshot {
	byCorr := r.fair.activeCorrelations()
	return RunnerSnapshot{
		Active:                int32(r.fair.inflightTotal()),
		MaxActive:             r.maxActive,
		InflightByCorrelation: byCorr,
	}
}

// acquireSlot blocks until a fair-share concurrency slot is available for this
// correlation, or the runner context is cancelled. Returns false when the caller
// should abort (shutdown). The fair dispatcher ensures no single correlation
// monopolises the total cap when multiple workflows run concurrently.
func (r *PersonaRunner) acquireSlot(env event.Envelope, handlerName string) bool {
	// Log saturation when the total cap is full. We check before blocking so the
	// log reflects the load at admission time. fairDispatcher.acquire has its own
	// fast path (no cond.Wait when below cap and under fair share), so this check
	// does not add overhead on the uncontested path.
	if r.fair.inflightTotal() >= int(r.maxActive) {
		r.logger.Warn("persona runner: concurrency cap reached, dispatch waiting",
			slog.String("handler", handlerName),
			slog.Int("active", r.fair.inflightTotal()),
			slog.Int("cap", int(r.maxActive)),
			slog.String("correlation", env.CorrelationID),
		)
	}
	return r.fair.acquire(r.ctx, env.CorrelationID)
}

// releaseSlot frees the concurrency slot acquired by acquireSlot for this
// correlation, allowing a waiting goroutine (preferring under-share correlations)
// to proceed.
func (r *PersonaRunner) releaseSlot(corrID string) {
	r.fair.release(corrID)
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
		satisfied, _, _, err := r.resolver.checkJoinCondition(r.ctx, afterPersonas, env.CorrelationID)
		if err != nil {
			return fmt.Errorf("persona runner: recover dispatch: join check store error: %w", err)
		}
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
	r.subscribeWorkflowRetried()
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
			r.drainTimeout, r.fair.inflightTotal())
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

		// 3. Chain depth check (per-workflow limit, falls back to package default).
		if chainDepth >= r.chainDepthLimit(env.CorrelationID) {
			r.logger.Warn("persona runner: max chain depth reached",
				slog.String("handler", h.Name()),
				slog.Int("depth", chainDepth),
				slog.String("event_id", string(env.ID)),
				slog.String("correlation", env.CorrelationID),
			)
			return nil
		}

		// 4. Event dedup
		if !r.seen.Add(h.Name(), string(env.ID)) {
			r.logger.Warn("persona runner: dispatch dropped",
				slog.String("drop_reason", dropReasonEventDedup),
				slog.String("handler", h.Name()),
				slog.String("event_type", string(env.Type)),
				slog.String("event_id", string(env.ID)),
				slog.String("correlation", env.CorrelationID),
			)
			r.emitDispatchDropped(h.Name(), env, dropReasonEventDedup, nil, "", "")
			return nil
		}

		// 5. Width limit is no longer enforced at admission. Events are always
		// queued and the drain goroutine blocks on the semaphore in
		// executeDispatch / executeHintApprovedDispatch. This preserves
		// per-(handler, correlation) ordering and prevents lost dispatches
		// when many workflows with parallel fan-out (e.g., pr-review) run
		// concurrently.

		// 6. Join condition check (DAG deps + hooks)
		afterPersonas := r.resolver.effectiveAfterPersonas(h, env.CorrelationID, r.hooks)
		if len(afterPersonas) > 0 && env.CorrelationID != "" {
			satisfied, fingerprint, missing, joinErr := r.resolver.checkJoinCondition(r.ctx, afterPersonas, env.CorrelationID)
			if joinErr != nil {
				// Transient store error. Unlatch the idempotency entry so a
				// subsequent event (e.g., the next PersonaCompleted from a
				// sibling predecessor) can re-admit this same event ID and
				// retry naturally. Without the Remove, the dispatch is lost
				// forever — dedup at step 4 already registered the eventID.
				r.seen.Remove(h.Name(), string(env.ID))
				r.logger.Error("persona runner: dispatch dropped",
					slog.String("drop_reason", dropReasonStoreError),
					slog.String("handler", h.Name()),
					slog.String("event_type", string(env.Type)),
					slog.String("event_id", string(env.ID)),
					slog.String("correlation", env.CorrelationID),
					slog.String("error", joinErr.Error()),
				)
				r.emitDispatchDropped(h.Name(), env, dropReasonStoreError, nil, "", joinErr.Error())
				return nil
			}
			if !satisfied {
				// join_unsatisfied is expected on parallel fan-out (pr-review's
				// N-way consolidator sees up to N-1 of these per dispatch before
				// the final join completes). Debug-level keeps log noise down;
				// the DispatchDropped event persists for post-hoc queries.
				r.logger.Debug("persona runner: dispatch dropped",
					slog.String("drop_reason", dropReasonJoinUnsatisfied),
					slog.String("handler", h.Name()),
					slog.String("event_type", string(env.Type)),
					slog.String("event_id", string(env.ID)),
					slog.String("correlation", env.CorrelationID),
					slog.Any("required_after", afterPersonas),
					slog.Any("missing_predecessors", missing),
				)
				r.emitDispatchDropped(h.Name(), env, dropReasonJoinUnsatisfied, missing, "", "")
				return nil
			}
			// Join-gate dedup: when multiple PersonaCompleted events
			// satisfy the same join, dispatch only once per unique set.
			if len(afterPersonas) > 1 && env.Type == event.PersonaCompleted {
				if !r.seen.Add(h.Name()+":join", fingerprint) {
					// join_gate_dedup is the expected second-wave noise from
					// parallel fan-out — N-1 drops per N-way join by design.
					// Debug keeps log volume sane; the DispatchDropped event
					// still persists for post-hoc queries.
					r.logger.Debug("persona runner: dispatch dropped",
						slog.String("drop_reason", dropReasonJoinGateDedup),
						slog.String("handler", h.Name()),
						slog.String("event_type", string(env.Type)),
						slog.String("correlation", env.CorrelationID),
						slog.String("fingerprint", fingerprint),
					)
					r.emitDispatchDropped(h.Name(), env, dropReasonJoinGateDedup, nil, fingerprint, "")
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
			r.logger.Warn("persona runner: dispatch dropped",
				slog.String("drop_reason", dropReasonCtxCancelled),
				slog.String("handler", h.Name()),
				slog.String("event_type", string(env.Type)),
				slog.String("correlation", env.CorrelationID),
			)
			r.emitDispatchDropped(h.Name(), env, dropReasonCtxCancelled, nil, "", r.ctx.Err().Error())
			return nil
		}

		// 9. Enqueue into the per-(handler, correlation) priority queue.
		r.enqueueAndDrain(h, env, chainDepth)
		return nil
	}
}

// chainDepthLimit returns the effective max chain depth for the given
// correlationID. It resolves the workflow definition via the runner's
// resolver (O(1) cache + map lookup) and delegates to
// WorkflowDef.EffectiveMaxChainDepth(). Falls back to r.defaultChainDepth
// when the correlation is unknown or has no registered workflow.
func (r *PersonaRunner) chainDepthLimit(correlationID string) int {
	if correlationID != "" {
		if wfID, ok := r.resolver.resolveWorkflowID(correlationID); ok {
			if def, ok := r.resolver.getWorkflowDef(wfID); ok {
				return def.EffectiveMaxChainDepth()
			}
		}
	}
	return r.defaultChainDepth
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
	if !r.acquireSlot(env, h.Name()) {
		return
	}
	defer r.releaseSlot(env.CorrelationID)

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
		failureKind, stderr, backendName := classifyDispatchFailure(dispatchCtx, dispatchErr)
		allEvents = append(allEvents, event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
			Persona:        h.Name(),
			TriggerEvent:   string(env.Type),
			TriggerID:      string(env.ID),
			Reactive:       true,
			Error:          dispatchErr.Error(),
			FailureKind:    failureKind,
			Backend:        backendName,
			Stderr:         stderr,
			DurationMS:     durationMS,
			ChainDepth:     chainDepth,
			HandlerVersion: buildinfo.Version(),
		})).
			WithCausation(env.ID).
			WithCorrelation(env.CorrelationID).
			WithSource("persona-runner:"+h.Name()))
	} else {
		outputRef := ""
		outputTextLen := -1 // -1 = no AIResponseReceived seen → guard does not apply
		if result != nil {
			for _, re := range result.Events {
				if re.Type == event.AIResponseReceived {
					outputRef = string(re.ID)
					outputTextLen = aiResponseTextLen(re.Payload)
				}
			}
		}

		// Developer guard rail (2026-04-29 incident, workflow d0c82058):
		// the model wrote 4 files / 356 insertions correctly, but Rick captured
		// the literal string `["sub"]` (7 bytes) as its persona output. The
		// reviewer downstream got that fragment as "Implementation to Review"
		// and FAILed every iteration. The strict ExtractJSON + sawToolUse fixes
		// in internal/backend address known upstream causes; this guard is the
		// catch-all defending the next variant. If the developer's textual
		// output is suspiciously short AND the workspace has uncommitted
		// changes (the model DID work but Rick lost the description of it),
		// fail loudly so the operator can take over instead of feeding garbage
		// downstream.
		if r.developerOutputGuardTrips(h, env.CorrelationID, outputTextLen) {
			allEvents = append(allEvents, event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
				Persona:      h.Name(),
				TriggerEvent: string(env.Type),
				TriggerID:    string(env.ID),
				Reactive:     true,
				Error: fmt.Sprintf(
					"developer output guard: captured %d-byte response while workspace has uncommitted changes — model produced files but Rick captured no description",
					outputTextLen),
				FailureKind:    event.FailureKindOutputTruncated,
				DurationMS:     durationMS,
				ChainDepth:     chainDepth,
				HandlerVersion: buildinfo.Version(),
			})).
				WithCausation(env.ID).
				WithCorrelation(env.CorrelationID).
				WithSource("persona-runner:"+h.Name()))
		} else {
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
		// Surface source + reason so operators can tell an MCP/CLI-initiated
		// cancel from an engine-initiated one (e.g., token-budget exceeded).
		// Without these fields, "workflow cancelled" looks identical to
		// "workflow silently stuck" in logs.
		attrs := []slog.Attr{slog.String("correlation", corrID)}
		var p event.WorkflowCancelledPayload
		if err := json.Unmarshal(env.Payload, &p); err == nil {
			if p.Source != "" {
				attrs = append(attrs, slog.String("source", p.Source))
			}
			if p.Reason != "" {
				attrs = append(attrs, slog.String("reason", p.Reason))
			}
		}
		r.logger.LogAttrs(r.ctx, slog.LevelInfo, "persona runner: workflow cancelled", attrs...)
		return nil
	}, eventbus.WithName("persona-runner:cancel"))

	r.unsubs = append(r.unsubs, unsub1, unsub2, unsub3)
}

// subscribeWorkflowRetried handles automatic and operator-initiated retries
// from a specific phase. The aggregate has already cleared CompletedPersonas
// for FromPhase + downstream (via Apply) by the time we see the event on the
// bus, so we only need to re-dispatch FromPhase — downstream handlers will
// re-fire naturally once it re-completes. Using RecoverDispatch bypasses the
// idempotency cache, which already holds the prior trigger eventID from the
// first failed run.
//
// If any of the four guard conditions cannot be satisfied (unresolvable
// workflow, unregistered def, missing trigger, handler not found / join
// unsatisfied), we publish a synthetic PersonaFailed with
// FailureKindHandlerError so the aggregate — whose AutoRetries counter is
// already at cap after Apply(WorkflowRetried{Automatic:true}) — transitions
// to WorkflowFailed instead of silently staying in the Running state forever.
func (r *PersonaRunner) subscribeWorkflowRetried() {
	unsub := r.bus.Subscribe(event.WorkflowRetried, func(_ context.Context, env event.Envelope) error {
		var p event.WorkflowRetriedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			r.logger.Error("persona runner: workflow retried payload decode failed",
				slog.String("error", err.Error()),
			)
			return nil
		}
		if p.FromPhase == "" {
			return nil
		}

		corrID := env.CorrelationID
		if corrID == "" {
			corrID = env.AggregateID
		}

		// publishDispatchWedge emits a synthetic PersonaFailed so the aggregate
		// can transition to WorkflowFailed. We use FailureKindHandlerError because
		// the root cause is a dispatch infrastructure problem, not a transient
		// backend timeout — the aggregate must NOT auto-retry again.
		publishDispatchWedge := func(cause string) {
			r.logger.Warn("persona runner: auto-retry dispatch wedged, emitting synthetic PersonaFailed",
				slog.String("correlation", corrID),
				slog.String("from_phase", p.FromPhase),
				slog.String("cause", cause),
			)
			reason := "auto-retry dispatch failed: " + cause
			failEvt := event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
				Persona:        p.FromPhase,
				Phase:          p.FromPhase,
				TriggerEvent:   string(env.Type),
				TriggerID:      string(env.ID),
				Reactive:       false,
				Error:          reason,
				FailureKind:    event.FailureKindHandlerError,
				HandlerVersion: buildinfo.Version(),
			})).
				WithCorrelation(corrID).
				WithCausation(env.ID).
				WithSource("engine:auto-retry")
			if pubErr := r.bus.Publish(r.ctx, failEvt); pubErr != nil {
				r.logger.Error("persona runner: failed to publish synthetic PersonaFailed",
					slog.String("correlation", corrID),
					slog.String("error", pubErr.Error()),
				)
			}
		}

		// Load events up front — we need them for the trigger lookup anyway,
		// and they let us warm the correlation cache if RecoveryScanner skipped
		// this workflow (Failed/Cancelled aren't eligible for startup recovery).
		events, err := r.store.LoadByCorrelation(r.ctx, corrID)
		if err != nil {
			r.logger.Error("persona runner: retry load events failed",
				slog.String("correlation", corrID),
				slog.String("error", err.Error()),
			)
			return nil
		}

		workflowID, ok := r.resolver.resolveWorkflowID(corrID)
		if !ok {
			workflowID = findWorkflowIDFromEvents(events)
			if workflowID == "" {
				publishDispatchWedge("workflow id unresolvable")
				return nil
			}
			r.resolver.cacheWorkflowID(corrID, workflowID)
		}
		def, ok := r.resolver.getWorkflowDef(workflowID)
		if !ok {
			publishDispatchWedge(fmt.Sprintf("workflow def %q not registered", workflowID))
			return nil
		}

		trigger := r.findRetryTrigger(events, def, p.FromPhase)
		if trigger == nil {
			publishDispatchWedge("predecessor PersonaCompleted not found in store")
			return nil
		}

		// Retries un-pause too — the aggregate is back in Running.
		r.pauser.resume(corrID)

		r.logger.Info("persona runner: retry dispatch",
			slog.String("correlation", corrID),
			slog.String("from_phase", p.FromPhase),
			slog.String("trigger_type", string(trigger.Type)),
		)
		if dispatchErr := r.RecoverDispatch(p.FromPhase, *trigger); dispatchErr != nil {
			r.logger.Error("persona runner: retry dispatch failed",
				slog.String("correlation", corrID),
				slog.String("from_phase", p.FromPhase),
				slog.String("error", dispatchErr.Error()),
			)
			publishDispatchWedge(dispatchErr.Error())
		}
		return nil
	}, eventbus.WithName("persona-runner:retry"))
	r.unsubs = append(r.unsubs, unsub)
}

// findWorkflowIDFromEvents scans the correlation chain for the original
// WorkflowRequested and returns its WorkflowID. Used when the runner's
// correlation cache is cold (e.g., server restart between a workflow's
// failure and the operator-triggered retry — RecoveryScanner only warms
// Running/Paused workflows, so Failed ones need lazy resolution).
func findWorkflowIDFromEvents(events []event.Envelope) string {
	for _, e := range events {
		if e.Type != event.WorkflowRequested {
			continue
		}
		var p event.WorkflowRequestedPayload
		if err := json.Unmarshal(e.Payload, &p); err == nil && p.WorkflowID != "" {
			return p.WorkflowID
		}
	}
	return ""
}

// findRetryTrigger picks the envelope to replay into FromPhase's handler:
// the most recent PersonaCompleted from a predecessor, or — for a root phase —
// the workflow.started.<id> envelope from the original run.
func (r *PersonaRunner) findRetryTrigger(events []event.Envelope, def WorkflowDef, fromPhase string) *event.Envelope {
	predecessors := def.Graph[fromPhase]
	if len(predecessors) == 0 {
		for i := range events {
			if event.IsWorkflowStarted(events[i].Type) {
				return &events[i]
			}
		}
		return nil
	}
	predSet := make(map[string]bool, len(predecessors))
	for _, pred := range predecessors {
		predSet[pred] = true
	}
	var best *event.Envelope
	for i := range events {
		if events[i].Type != event.PersonaCompleted {
			continue
		}
		var pc event.PersonaCompletedPayload
		if err := json.Unmarshal(events[i].Payload, &pc); err != nil {
			continue
		}
		if predSet[pc.Persona] {
			best = &events[i]
		}
	}
	return best
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
		failureKind, stderr, backendName := classifyDispatchFailure(hintCtx, err)
		allEvents = append(allEvents, event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
			Persona:        h.Name(),
			TriggerEvent:   string(env.Type),
			TriggerID:      string(env.ID),
			Reactive:       true,
			Error:          fmt.Sprintf("hint failed: %s", err.Error()),
			FailureKind:    failureKind,
			Backend:        backendName,
			Stderr:         stderr,
			DurationMS:     durationMS,
			ChainDepth:     chainDepth,
			HandlerVersion: buildinfo.Version(),
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
	if !r.acquireSlot(env, handlerName) {
		return
	}
	defer r.releaseSlot(env.CorrelationID)

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
		failureKind, stderr, backendName := classifyDispatchFailure(dispatchCtx, dispatchErr)
		allEvents = append(allEvents, event.New(event.PersonaFailed, 1, event.MustMarshal(event.PersonaFailedPayload{
			Persona:        handlerName,
			TriggerEvent:   string(env.Type),
			TriggerID:      string(env.ID),
			Reactive:       true,
			Error:          dispatchErr.Error(),
			FailureKind:    failureKind,
			Backend:        backendName,
			Stderr:         stderr,
			DurationMS:     durationMS,
			ChainDepth:     chainDepth,
			HandlerVersion: buildinfo.Version(),
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

// emitDispatchDropped persists a DispatchDropped event to the dedicated
// diagnostic aggregate {correlationID}:drops. Operators can count/query
// drops per workflow via SQL without replaying the correlation chain or
// grepping log output. Writing to a separate aggregate avoids version
// contention on the main workflow aggregate — critical for pr-review's
// N-way parallel fan-out (N-1 join-gate dedup drops per consolidator).
//
// Never published on the bus: no handler subscribes to DispatchDropped
// (registered in internalEvents). Best-effort — errors are swallowed since
// observability must not fail a dispatch.
//
// Uses a short-lived background context (NOT r.ctx) so the diagnostic
// record still persists when the runner is shutting down. The ctx_cancelled
// drop reason is exactly the case where operators most need a durable
// record, and using r.ctx here would burn all retries on context errors
// before landing a single byte.
func (r *PersonaRunner) emitDispatchDropped(handlerName string, trigger event.Envelope, reason string, missing []string, fingerprint, detail string) {
	if trigger.CorrelationID == "" {
		return // no correlation → no diagnostic aggregate to write to
	}
	if r.store == nil {
		return
	}
	payload := event.DispatchDroppedPayload{
		Handler:             handlerName,
		DroppedEventID:      string(trigger.ID),
		DroppedEventType:    string(trigger.Type),
		DropReason:          reason,
		MissingPredecessors: missing,
		Fingerprint:         fingerprint,
		Detail:              detail,
	}
	evt := event.New(event.DispatchDropped, 1, event.MustMarshal(payload)).
		WithCorrelation(trigger.CorrelationID).
		WithCausation(trigger.ID).
		WithSource("persona-runner")

	// Short-lived background context so this write survives runner shutdown
	// and context cancellation — the diagnostic aggregate must record
	// ctx_cancelled drops even when r.ctx is already done.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	aggregateID := trigger.CorrelationID + ":drops"
	const maxAttempts = 3
	for range maxAttempts {
		currentVersion := 0
		if existing, err := r.store.Load(ctx, aggregateID); err == nil && len(existing) > 0 {
			currentVersion = existing[len(existing)-1].Version
		}
		versioned := evt.WithAggregate(aggregateID, currentVersion+1)
		if err := r.store.Append(ctx, aggregateID, currentVersion, []event.Envelope{versioned}); err == nil {
			return
		}
	}
	// Persistence failed after retries — log but don't fail the dispatch.
	r.logger.Debug("persona runner: failed to persist DispatchDropped",
		slog.String("handler", handlerName),
		slog.String("correlation", trigger.CorrelationID),
	)
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

