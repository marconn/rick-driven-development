package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"

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
	return e
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

	if e.stopCh != nil {
		close(e.stopCh)
		<-e.done
		e.stopCh = nil
	}
}

// processLoop is the single goroutine that drains eventCh. All events from
// the bus are serialized here — no concurrent access to the same aggregate.
func (e *Engine) processLoop() {
	defer close(e.done)
	for {
		select {
		case env := <-e.eventCh:
			e.processAndLog(env)
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

	for _, env := range events {
		agg.Apply(env)
	}

	// Attach WorkflowDef from registered workflows based on the workflow ID
	// set by WorkflowRequested. MaxIterations on the aggregate is authoritative
	// for Decide(), so sync it from the registered definition.
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
