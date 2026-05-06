package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// Engine is the workflow lifecycle manager. It subscribes to lifecycle events,
// loads the aggregate from the store, runs Decide(), and persists + publishes
// resulting events. Zero dispatch logic — PersonaRunner is the sole dispatcher.
type Engine struct {
	store       eventstore.Store
	bus         eventbus.Bus
	logger      *slog.Logger
	workflowsMu sync.RWMutex
	workflows   map[string]WorkflowDef // registered workflow definitions by ID
	unsubs      []func()

	// FIFO event channel: serializes all lifecycle events into a single
	// goroutine. This prevents ordering races (e.g., VerdictRendered must
	// process before PersonaCompleted for the same workflow) caused by
	// ChannelBus dispatching each subscriber in a separate goroutine.
	eventCh chan event.Envelope
	stopCh  chan struct{} // signals processLoop to drain and exit
	done    chan struct{} // closed when processLoop exits

	// Workflow concurrency throttle. Limits how many workflows can be
	// running simultaneously. Owned exclusively by the processLoop goroutine.
	throttle *workflowThrottle

	// Throttle stall watchdog. When enabled, a separate goroutine ticks at
	// watchdogInterval and posts onto watchdogTick; processLoop drains the
	// tick on the same goroutine that owns the throttle so the sweep is
	// race-free without a mutex. Auto-fails Running workflows with no
	// engine-visible activity for stallTimeout, freeing their slot via
	// FailureKindStalled. Paused aggregates are explicitly excluded —
	// HintEmitted-pause is intentional and may legitimately exceed any
	// timeout while waiting for an operator.
	watchdogEnabled  bool
	watchdogInterval time.Duration
	stallTimeout     time.Duration
	watchdogTick     chan time.Time
	watchdogStop     chan struct{}
	watchdogDone     chan struct{}
}

// NewEngine creates a new workflow lifecycle engine.
func NewEngine(store eventstore.Store, bus eventbus.Bus, logger *slog.Logger) *Engine {
	e := &Engine{
		store:     store,
		bus:       bus,
		logger:    logger,
		workflows: make(map[string]WorkflowDef),
	}
	e.initThrottleFromEnv()
	e.initWatchdogFromEnv()
	return e
}

// Default tuning for the throttle stall watchdog. Threshold sits comfortably
// above RICK_BACKEND_TIMEOUT (default 20m) so a legitimate long-running
// developer call has slack before being misclassified.
const (
	defaultStallTimeout     = 30 * time.Minute
	defaultWatchdogInterval = 5 * time.Minute
)

// initWatchdogFromEnv reads RICK_THROTTLE_WATCHDOG / _STALL_TIMEOUT /
// _WATCHDOG_INTERVAL. Defaults: enabled with a 30m timeout swept every 5m.
// Set RICK_THROTTLE_WATCHDOG=0 to disable as a kill switch.
func (e *Engine) initWatchdogFromEnv() {
	e.watchdogEnabled = true
	if raw := os.Getenv("RICK_THROTTLE_WATCHDOG"); raw != "" {
		switch raw {
		case "0", "false", "FALSE", "off", "OFF":
			e.watchdogEnabled = false
		}
	}
	e.stallTimeout = defaultStallTimeout
	if d := parseDurationEnv("RICK_THROTTLE_STALL_TIMEOUT", e.logger); d > 0 {
		e.stallTimeout = d
	}
	e.watchdogInterval = defaultWatchdogInterval
	if d := parseDurationEnv("RICK_THROTTLE_WATCHDOG_INTERVAL", e.logger); d > 0 {
		e.watchdogInterval = d
	}
}

// parseDurationEnv reads a Go-format duration (e.g. "20m", "1h") from the
// given env var. Returns 0 when unset, empty, or invalid (with a warn log).
func parseDurationEnv(name string, logger *slog.Logger) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		logger.Warn("engine: ignoring invalid duration env var",
			slog.String("name", name),
			slog.String("value", raw),
			slog.String("error", err.Error()),
		)
		return 0
	}
	return d
}

// initThrottleFromEnv reads RICK_MAX_WORKFLOWS and initializes the throttle.
func (e *Engine) initThrottleFromEnv() {
	raw := os.Getenv("RICK_MAX_WORKFLOWS")
	if raw == "" {
		return
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		e.logger.Warn("engine: ignoring invalid RICK_MAX_WORKFLOWS",
			slog.String("value", raw),
		)
		return
	}
	if n > 0 {
		queueStore := e.queueStore()
		e.throttle = newWorkflowThrottle(n, queueStore, e.logger)
		e.logger.Info("engine: workflow throttle enabled",
			slog.Int("max_concurrent", n),
		)
	}
}

// queueStore returns the store as a WorkflowQueueStore if it implements the
// interface; otherwise returns nil (throttle operates in memory-only mode).
func (e *Engine) queueStore() eventstore.WorkflowQueueStore {
	if qs, ok := e.store.(eventstore.WorkflowQueueStore); ok {
		return qs
	}
	return nil
}

// SetMaxConcurrentWorkflows sets the maximum number of concurrently running
// workflows. Must be called before Start(). A value of 0 disables throttling.
func (e *Engine) SetMaxConcurrentWorkflows(n int) {
	if n <= 0 {
		e.throttle = nil
		return
	}
	queueStore := e.queueStore()
	e.throttle = newWorkflowThrottle(n, queueStore, e.logger)
}

// WarmThrottle seeds the throttle's running set with workflow IDs that were
// already running before a restart. Called by RecoveryScanner after projections
// have caught up.
func (e *Engine) WarmThrottle(runningIDs []string) {
	if e.throttle == nil {
		return
	}
	e.throttle.warmRunning(runningIDs)
}

// LoadQueuedWorkflows rehydrates the in-memory throttle queue from the DB.
// Must be called after WarmThrottle and before Start() so the queue is ready
// before processLoop begins consuming events.
//
// For each row in workflow_queue, if the aggregate_id does not correspond to a
// valid WorkflowRequested in the events table (orphan row), it is deleted with
// a warning. This keeps the table consistent across schema evolution.
func (e *Engine) LoadQueuedWorkflows(ctx context.Context) {
	if e.throttle == nil {
		return
	}
	qs, ok := e.store.(eventstore.WorkflowQueueStore)
	if !ok {
		return
	}

	envs, err := qs.LoadQueuedWorkflows(ctx)
	if err != nil {
		e.logger.Error("engine: load queued workflows failed",
			slog.String("error", err.Error()),
		)
		return
	}

	// Validate each envelope: the aggregate must exist in the events table.
	// An orphan row (aggregate_id present but no events) means we crashed after
	// writing the queue row but before Appending the WorkflowRequested event to
	// the events table — extremely unlikely but must not crash.
	valid := make([]event.Envelope, 0, len(envs))
	for _, env := range envs {
		events, loadErr := e.store.Load(ctx, env.AggregateID)
		if loadErr != nil {
			e.logger.Warn("engine: could not verify queued workflow, keeping",
				slog.String("aggregate_id", env.AggregateID),
				slog.String("error", loadErr.Error()),
			)
			valid = append(valid, env) // keep on load error — fail safe
			continue
		}
		if len(events) == 0 {
			e.logger.Warn("engine: orphan queue row (no events), deleting",
				slog.String("aggregate_id", env.AggregateID),
			)
			if delErr := qs.DeleteQueuedWorkflow(ctx, env.AggregateID); delErr != nil {
				e.logger.Error("engine: delete orphan queue row failed",
					slog.String("aggregate_id", env.AggregateID),
					slog.String("error", delErr.Error()),
				)
			}
			continue
		}
		valid = append(valid, env)
	}

	e.throttle.warmQueued(valid)
}

// ThrottleSnapshot reports throttle state for observability. Safe to call
// before Start() and while Start() is running. MaxConcurrent=0 means the
// throttle is disabled (unlimited). When disabled, Running and Queued are
// always zero — the engine doesn't track running workflows unless throttling
// is active.
type ThrottleSnapshot struct {
	Enabled       bool
	MaxConcurrent int
	Running       int
	Queued        int
	// Stalled is the cumulative count of slots reclaimed by the throttle
	// watchdog since this process started. Always zero when the throttle
	// is disabled or the watchdog is off.
	Stalled int
}

// ThrottleSnapshot returns a value copy of the current throttle state. The
// caller is the processLoop goroutine or anyone holding no throttle-related
// lock; we read the counters without synchronization because the throttle is
// owned exclusively by the processLoop. Values may be slightly stale but are
// never torn — all fields are simple int reads.
//
// Returns Enabled=false when RICK_MAX_WORKFLOWS is unset/zero.
func (e *Engine) ThrottleSnapshot() ThrottleSnapshot {
	if e.throttle == nil {
		return ThrottleSnapshot{}
	}
	return ThrottleSnapshot{
		Enabled:       true,
		MaxConcurrent: e.throttle.maxConcurrent,
		Running:       e.throttle.runningCount(),
		Queued:        e.throttle.queuedCount(),
		Stalled:       e.throttle.stalledCount(),
	}
}

// RegisterWorkflow registers a workflow definition by ID. Safe to call after
// Start() — concurrent reads in the process loop are protected by a mutex.
//
// Applies env-var overrides (RICK_MAX_ITERATION) before storage so the engine's
// authoritative copy reflects the runtime configuration without requiring
// every workflow factory to consult the env itself.
func (e *Engine) RegisterWorkflow(def WorkflowDef) {
	def = applyEnvOverrides(def)
	e.workflowsMu.Lock()
	e.workflows[def.ID] = def
	e.workflowsMu.Unlock()
}

// GetWorkflowDef returns the registered workflow definition for the given ID.
func (e *Engine) GetWorkflowDef(workflowID string) (WorkflowDef, bool) {
	e.workflowsMu.RLock()
	def, ok := e.workflows[workflowID]
	e.workflowsMu.RUnlock()
	return def, ok
}

// RegisteredWorkflows returns a snapshot of all registered workflow definitions.
// Includes both built-in and dynamically registered (via gRPC) workflows.
func (e *Engine) RegisteredWorkflows() []WorkflowDef {
	e.workflowsMu.RLock()
	defer e.workflowsMu.RUnlock()
	defs := make([]WorkflowDef, 0, len(e.workflows))
	for _, def := range e.workflows {
		defs = append(defs, def)
	}
	return defs
}

// Start subscribes the engine to all lifecycle events it needs to react to.
// Events are enqueued into a FIFO channel and processed by a single goroutine
// to guarantee ordering (e.g., VerdictRendered before PersonaCompleted).
func (e *Engine) Start() {
	e.eventCh = make(chan event.Envelope, 256)
	e.stopCh = make(chan struct{})
	e.done = make(chan struct{})

	if e.throttle != nil && e.watchdogEnabled && e.stallTimeout > 0 && e.watchdogInterval > 0 {
		e.watchdogTick = make(chan time.Time, 1)
		e.watchdogStop = make(chan struct{})
		e.watchdogDone = make(chan struct{})
		go e.watchdogLoop()
		e.logger.Info("engine: throttle watchdog enabled",
			slog.Duration("stall_timeout", e.stallTimeout),
			slog.Duration("interval", e.watchdogInterval),
		)
	}

	go e.processLoop()

	reactTo := []event.Type{
		event.WorkflowRequested,
		event.PersonaCompleted,
		event.PersonaFailed,
		event.VerdictRendered,
		event.TokenBudgetExceeded,
		event.WorkflowCancelled,
		event.WorkflowPaused,
		event.WorkflowResumed,
		event.WorkflowRetried,
		event.HintEmitted,
		event.HintRejected,
	}
	for _, et := range reactTo {
		unsub := e.bus.Subscribe(et, func(_ context.Context, env event.Envelope) error {
			select {
			case e.eventCh <- env:
			case <-e.stopCh:
			}
			return nil
		}, eventbus.WithName("engine"), eventbus.WithSync())
		e.unsubs = append(e.unsubs, unsub)
	}
}

// Stop unsubscribes the engine from all events and drains the FIFO channel.
func (e *Engine) Stop() {
	for _, unsub := range e.unsubs {
		unsub()
	}
	e.unsubs = nil

	if e.watchdogStop != nil {
		close(e.watchdogStop)
		<-e.watchdogDone
		e.watchdogStop = nil
	}

	if e.stopCh != nil {
		close(e.stopCh)
		<-e.done
		e.stopCh = nil
	}
}

// processLoop is the single goroutine that drains eventCh. All events from
// the bus are serialized here — no concurrent access to the same aggregate.
// Watchdog ticks share the same goroutine so the throttle's single-writer
// invariant holds without locks.
func (e *Engine) processLoop() {
	defer close(e.done)
	for {
		select {
		case env := <-e.eventCh:
			e.processAndLog(env)
		case now := <-e.watchdogTick:
			e.runWatchdogSweep(context.Background(), now)
		case <-e.stopCh:
			// Drain remaining events in the channel before exiting.
			for {
				select {
				case env := <-e.eventCh:
					e.processAndLog(env)
				default:
					return
				}
			}
		}
	}
}

// watchdogLoop ticks at watchdogInterval and forwards the tick onto
// watchdogTick. The actual sweep runs on processLoop's goroutine to preserve
// the throttle's single-writer invariant. Drops a tick when processLoop is
// busy (the channel is buffer-1) — under sustained backpressure the next
// tick still fires, so we don't leak tick goroutines.
func (e *Engine) watchdogLoop() {
	defer close(e.watchdogDone)
	ticker := time.NewTicker(e.watchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.watchdogStop:
			return
		case t := <-ticker.C:
			select {
			case e.watchdogTick <- t:
			default:
				// processLoop hasn't drained the previous tick yet —
				// drop this one rather than block the watchdog goroutine.
			}
		}
	}
}

// runWatchdogSweep auto-fails Running workflows whose last engine-visible
// activity is older than stallTimeout. Paused / Cancelled / terminal
// aggregates are filtered by reloading the aggregate before emitting —
// callers cannot rely on the throttle's running set alone, since the
// recovery scanner seeds paused aggregates into it too.
//
// Each reclaimed slot emits a workflow-aggregate WorkflowFailed
// (FailureKindStalled), removes the entry from the throttle running set,
// publishes the event so projections and notifications stay current, and
// drains the queue.
func (e *Engine) runWatchdogSweep(ctx context.Context, now time.Time) {
	if e.throttle == nil {
		return
	}
	candidates := e.throttle.stalledEntries(e.stallTimeout, now)
	if len(candidates) == 0 {
		return
	}
	for _, corrID := range candidates {
		e.reclaimStalledSlot(ctx, corrID, now)
	}
	if len(candidates) > 0 {
		e.drainThrottleQueue(ctx)
	}
}

// reclaimStalledSlot processes a single watchdog candidate. Loads the
// workflow aggregate to verify it is still Running, then appends and
// publishes a WorkflowFailed{FailureKindStalled} event and frees the slot.
// On any disqualifying state (paused, terminal, missing) the throttle entry
// is left in place so the legitimate decrement path can reclaim it.
func (e *Engine) reclaimStalledSlot(ctx context.Context, corrID string, now time.Time) {
	agg, err := e.loadAggregate(ctx, corrID)
	if err != nil {
		e.logger.Warn("engine: watchdog load aggregate failed",
			slog.String("correlation", corrID),
			slog.String("error", err.Error()),
		)
		return
	}
	if agg.Status != StatusRunning {
		// Paused-by-design or already terminal. Leave the running entry
		// intact when paused (operator may resume); it'll get re-evaluated
		// on the next sweep with refreshed activity once the workflow
		// resumes. For terminal states the legitimate path will remove it.
		return
	}

	reason := fmt.Sprintf("throttle watchdog: no engine activity for %s", e.stallTimeout)
	failed := event.New(event.WorkflowFailed, 1, event.MustMarshal(event.WorkflowFailedPayload{
		Reason:      reason,
		FailureKind: event.FailureKindStalled,
	})).
		WithAggregate(corrID, agg.Version+1).
		WithCorrelation(corrID).
		WithSource("engine:throttle_watchdog")

	if err := e.store.Append(ctx, corrID, agg.Version, []event.Envelope{failed}); err != nil {
		e.logger.Error("engine: watchdog append WorkflowFailed failed",
			slog.String("correlation", corrID),
			slog.String("error", err.Error()),
		)
		return
	}
	silence := e.throttle.markStalled(corrID, now)
	e.logger.Warn("engine: throttle watchdog reclaimed stalled slot",
		slog.String("correlation", corrID),
		slog.String("workflow_id", agg.WorkflowID),
		slog.Duration("silence", silence),
		slog.Duration("threshold", e.stallTimeout),
		slog.Int("running", e.throttle.runningCount()),
	)
	if err := e.bus.Publish(ctx, failed); err != nil {
		e.logger.Error("engine: watchdog publish WorkflowFailed failed",
			slog.String("correlation", corrID),
			slog.String("error", err.Error()),
		)
	}
}

func (e *Engine) processAndLog(env event.Envelope) {
	if err := e.processDecision(context.Background(), env); err != nil {
		e.logger.Error("engine: process decision failed",
			slog.String("event_type", string(env.Type)),
			slog.String("event_id", string(env.ID)),
			slog.String("error", err.Error()),
		)
	}
}

func (e *Engine) processDecision(ctx context.Context, env event.Envelope) error {
	// Refresh watchdog activity for any event tied to a known running
	// workflow. Touch is a no-op when the correlation isn't in the throttle's
	// running set, so unconditional call is cheap and keeps the watchdog
	// signal fresh for every lifecycle / verdict / hint event the engine
	// already subscribes to.
	if e.throttle != nil && env.CorrelationID != "" {
		e.throttle.touchActivity(env.CorrelationID)
	}

	// Throttle: queue WorkflowRequested if at capacity.
	if env.Type == event.WorkflowRequested && e.throttle != nil && e.throttle.shouldQueue() {
		e.throttle.enqueue(ctx, env)
		return nil
	}

	// Track incoming terminal events that are external (WorkflowCancelled).
	// These arrive from operator actions, not from Decide.
	if env.Type == event.WorkflowCancelled && e.throttle != nil {
		corrID := env.CorrelationID
		if corrID == "" {
			corrID = env.AggregateID
		}
		e.throttle.removeRunning(corrID)
		e.throttle.removeQueued(ctx, corrID)
	}

	aggID := e.resolveWorkflowAggregateID(env)

	newEvents, err := e.tryProcessDecision(ctx, aggID, env)
	if err != nil {
		return err
	}

	for _, ne := range newEvents {
		// Track state transitions for throttle.
		if e.throttle != nil {
			if event.IsWorkflowStarted(ne.Type) {
				e.throttle.addRunning(ne.CorrelationID)
			}
			if ne.Type == event.WorkflowCompleted || ne.Type == event.WorkflowFailed {
				e.throttle.removeRunning(ne.CorrelationID)
			}
		}

		if pubErr := e.bus.Publish(ctx, ne); pubErr != nil {
			e.logger.Error("engine: publish failed",
				slog.String("event_type", string(ne.Type)),
				slog.String("error", pubErr.Error()),
			)
		}
	}

	// Drain queued workflows into freed slots.
	e.drainThrottleQueue(ctx)

	return nil
}

// drainThrottleQueue processes queued WorkflowRequested events until the
// throttle is at capacity again or the queue is empty.
func (e *Engine) drainThrottleQueue(ctx context.Context) {
	if e.throttle == nil {
		return
	}
	for !e.throttle.shouldQueue() {
		next, ok := e.throttle.dequeue(ctx)
		if !ok {
			return
		}
		e.logger.Info("engine: dequeuing throttled workflow",
			slog.String("aggregate_id", next.AggregateID),
			slog.Int("running", e.throttle.runningCount()),
			slog.Int("queued", e.throttle.queuedCount()),
		)
		if err := e.processDecision(ctx, next); err != nil {
			e.logger.Error("engine: process queued workflow failed",
				slog.String("aggregate_id", next.AggregateID),
				slog.String("error", err.Error()),
			)
		}
	}
}

// tryProcessDecision performs a single load-track-decide-append cycle.
// Tracking and decide events are combined into a single atomic Append to
// avoid transaction races between concurrent processDecision goroutines.
func (e *Engine) tryProcessDecision(ctx context.Context, aggID string, env event.Envelope) ([]event.Envelope, error) {
	agg, err := e.loadAggregate(ctx, aggID)
	if err != nil {
		return nil, fmt.Errorf("engine: load aggregate: %w", err)
	}

	var allEvents []event.Envelope
	baseVersion := agg.Version

	// Track persona completions on the workflow aggregate so CompletedPersonas
	// survives aggregate replay without re-querying persona-scoped aggregates.
	// Uses PersonaTracked (not PersonaCompleted) to avoid polluting the bus
	// with a second event of the same type — PersonaRunner already published
	// the original PersonaCompleted; this copy is storage-only.
	if env.Type == event.PersonaCompleted && aggID != env.AggregateID {
		var p event.PersonaCompletedPayload
		if err := json.Unmarshal(env.Payload, &p); err == nil && !agg.CompletedPersonas[p.Persona] &&
			!agg.isStaleAfterFeedback(p.Persona) {
			trackEvt := event.New(event.PersonaTracked, 1, env.Payload).
				WithAggregate(agg.ID, agg.Version+1).
				WithCausation(env.ID).
				WithCorrelation(env.CorrelationID).
				WithSource("engine:tracking")
			allEvents = append(allEvents, trackEvt)
			agg.Apply(trackEvt) // update in-memory state for Decide
		}
	}

	// Mirror PersonaFailed onto the workflow aggregate as PersonaFailedTracked
	// so `rick events <workflow_agg>` shows the failure breadcrumb alongside
	// the PersonaTracked successes. Without this, operators inspecting the
	// workflow aggregate (the default view in MCP / agent UI) see the last
	// PersonaTracked and then a bare WorkflowRetried or WorkflowFailed — the
	// failure payload lives on the persona-scoped aggregate and is invisible
	// to the workflow-scoped view.
	//
	// Gating: only mirror when the persona is required AND the workflow is
	// still Running. Non-required hook/enricher failures are consciously
	// ignored by decidePersonaFailed (returning nil, nil) and writing a
	// mirror anyway would imply a required-persona failure where none
	// occurred. Similarly, a PersonaFailed arriving for a cancelled / paused
	// workflow is a dangling side-effect from a pre-terminal dispatch —
	// mirroring it would re-open the event trail on a workflow the operator
	// already signed off on.
	//
	// Storage-only; not published to the bus, since PersonaRunner already
	// published PersonaFailed for dispatch consumers. Applying the envelope
	// in-memory keeps the aggregate Version monotonic so the subsequent
	// Decide() call numbers its output events consistently (same pattern as
	// PersonaTracked above).
	if env.Type == event.PersonaFailed && aggID != env.AggregateID {
		var failPayload event.PersonaFailedPayload
		decodeErr := json.Unmarshal(env.Payload, &failPayload)
		if decodeErr == nil && agg.Status == StatusRunning && agg.isRequiredPersona(failPayload.Persona) {
			mirror := event.New(event.PersonaFailedTracked, 1, env.Payload).
				WithAggregate(agg.ID, agg.Version+1).
				WithCausation(env.ID).
				WithCorrelation(env.CorrelationID).
				WithSource("engine:tracking")
			allEvents = append(allEvents, mirror)
			agg.Apply(mirror)
		}
	}

	newEvents, err := agg.Decide(env)
	if err != nil {
		return nil, fmt.Errorf("engine: decide: %w", err)
	}
	allEvents = append(allEvents, newEvents...)

	// Mirror VerdictRendered onto the workflow aggregate as VerdictTracked so
	// the byte-identical fingerprint dedup guard (decideVerdictRendered) has
	// prior-verdict state to compare against on subsequent verdicts. Without
	// this mirror, VerdictRendered events live only on the persona-scoped
	// aggregate (<corr>:persona:<handler>) which loadAggregate(workflow_agg)
	// never sees, and LastVerdictFingerprint stays empty across loads — the
	// guard is dead code in production. Mirror is intentionally placed AFTER
	// Decide: if it ran before, Apply would set LastVerdictFingerprint from
	// the *current* verdict, which the *current* Decide would then read and
	// falsely self-match. After-Decide means current Decide reads only PRIOR
	// verdicts' fingerprints (rebuilt from earlier VerdictTracked replays),
	// and the current verdict is recorded only for FUTURE Decides.
	//
	// Storage-only; not published on the bus, since VerdictRendered was
	// already published by the handler. Version is computed as the next slot
	// after all Decide outputs, since Decide numbers its events from
	// agg.Version+1 without applying them in-place.
	if env.Type == event.VerdictRendered && aggID != env.AggregateID {
		nextVersion := agg.Version + len(newEvents) + 1
		mirror := event.New(event.VerdictTracked, 1, env.Payload).
			WithAggregate(agg.ID, nextVersion).
			WithCausation(env.ID).
			WithCorrelation(env.CorrelationID).
			WithSource("engine:tracking")
		allEvents = append(allEvents, mirror)
	}

	if len(allEvents) == 0 {
		return nil, nil
	}

	if err := e.store.Append(ctx, aggID, baseVersion, allEvents); err != nil {
		return nil, fmt.Errorf("engine: append events: %w", err)
	}

	// Index business keys as tags for external system lookup.
	if env.Type == event.WorkflowRequested {
		e.indexWorkflowTags(ctx, aggID, env)
	}

	// Only publish Decide events (not tracking events) to avoid double-dispatch.
	return newEvents, nil
}

// resolveWorkflowAggregateID returns the workflow aggregate ID for the given event.
// Persona lifecycle events arrive from persona-scoped aggregates (e.g.,
// "corr-1:persona:developer") but the Engine needs to load the workflow aggregate.
// By convention, CorrelationID == workflow aggregate ID.
func (e *Engine) resolveWorkflowAggregateID(env event.Envelope) string {
	switch env.Type {
	case event.PersonaCompleted, event.PersonaFailed, event.VerdictRendered,
		event.WorkflowCancelled, event.WorkflowPaused, event.WorkflowResumed,
		event.WorkflowRetried, event.HintEmitted, event.HintRejected:
		if env.CorrelationID != "" {
			return env.CorrelationID
		}
	}
	return env.AggregateID
}

func (e *Engine) loadAggregate(ctx context.Context, aggregateID string) (*WorkflowAggregate, error) {
	agg := NewWorkflowAggregate(aggregateID)

	// Try snapshot first to avoid full replay on long-running workflows.
	snap, snapErr := e.store.LoadSnapshot(ctx, aggregateID)
	var events []event.Envelope
	var err error

	if snapErr == nil {
		if unmarshalErr := json.Unmarshal(snap.State, agg); unmarshalErr != nil {
			return nil, fmt.Errorf("engine: unmarshal snapshot: %w", unmarshalErr)
		}
		events, err = e.store.LoadFrom(ctx, aggregateID, snap.Version+1)
	} else {
		events, err = e.store.Load(ctx, aggregateID)
	}
	if err != nil {
		return nil, fmt.Errorf("engine: load events for %s: %w", aggregateID, err)
	}

	// Pre-attach WorkflowDef BEFORE the Apply loop so Apply branches that
	// depend on WorkflowDef.PhaseMap (VerdictTracked → fingerprint dedup) or
	// WorkflowDef.PartialReviewOnFailure (PersonaFailedTracked) actually fold
	// state during replay. The original Apply→attach order silently no-op'd
	// those branches on every load, leaving LastVerdictFingerprint state dead
	// across restarts and after the workflow aggregate's first reload — which
	// is the entire production lifetime, since every event is processed
	// against a freshly-loaded aggregate.
	//
	// We scan for the WorkflowRequested envelope to extract WorkflowID
	// without running Apply, then attach the registered def. The post-loop
	// re-attach below stays as a safety net for the rare case where the
	// registry mutates between scan and replay.
	for _, env := range events {
		if env.Type != event.WorkflowRequested {
			continue
		}
		var p event.WorkflowRequestedPayload
		if json.Unmarshal(env.Payload, &p) != nil || p.WorkflowID == "" {
			break
		}
		e.workflowsMu.RLock()
		if def, ok := e.workflows[p.WorkflowID]; ok {
			agg.WorkflowDef = &def
			agg.MaxIterations = def.MaxIterations
		}
		e.workflowsMu.RUnlock()
		break
	}

	for _, env := range events {
		agg.Apply(env)
	}

	// Attach WorkflowDef from registered workflows based on the workflow ID
	// set by WorkflowRequested. MaxIterations on the aggregate is authoritative
	// for Decide(), so sync it from the registered definition. This second
	// attach is a no-op when the pre-Apply attach above succeeded, but stays
	// in place for back-compat with code paths that bypass the pre-scan.
	if agg.WorkflowID != "" {
		e.workflowsMu.RLock()
		def, ok := e.workflows[agg.WorkflowID]
		e.workflowsMu.RUnlock()
		if ok {
			agg.WorkflowDef = &def
			agg.MaxIterations = def.MaxIterations
		}
	}

	return agg, nil
}

// indexWorkflowTags extracts business keys from WorkflowRequested and saves
// them as tags on the event store. External systems can then look up the
// correlation ID by Jira ticket, GitHub repo+branch, source, etc.
func (e *Engine) indexWorkflowTags(ctx context.Context, correlationID string, env event.Envelope) {
	var p event.WorkflowRequestedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}

	tags := make(map[string]string)
	if p.Source != "" {
		tags["source"] = p.Source
	}
	if p.WorkflowID != "" {
		tags["workflow_id"] = p.WorkflowID
	}
	if p.Ticket != "" {
		tags["ticket"] = p.Ticket
	}
	if p.Repo != "" {
		tags["repo"] = p.Repo
	}
	if p.BaseBranch != "" && p.Repo != "" {
		tags["repo_branch"] = p.Repo + ":" + p.BaseBranch
	}
	if len(tags) == 0 {
		return
	}

	if err := e.store.SaveTags(ctx, correlationID, tags); err != nil {
		e.logger.Error("engine: save workflow tags failed",
			slog.String("correlation", correlationID),
			slog.String("error", err.Error()),
		)
	}
}
