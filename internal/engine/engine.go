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
	workflows          map[string]WorkflowDef // registered workflow definitions by ID
	onWorkflowRegister func(def WorkflowDef)  // callback fired on RegisterWorkflow
	unsubs             []func()

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
		e.throttle = newWorkflowThrottle(n, e.logger)
		e.logger.Info("engine: workflow throttle enabled",
			slog.Int("max_concurrent", n),
		)
	}
}

// SetMaxConcurrentWorkflows sets the maximum number of concurrently running
// workflows. Must be called before Start(). A value of 0 disables throttling.
func (e *Engine) SetMaxConcurrentWorkflows(n int) {
	if n <= 0 {
		e.throttle = nil
		return
	}
	e.throttle = newWorkflowThrottle(n, e.logger)
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

// OnWorkflowRegistered sets a callback that fires whenever a workflow
// definition is registered. Used by PersonaRunner to auto-scale chain depth.
func (e *Engine) OnWorkflowRegistered(fn func(def WorkflowDef)) {
	e.onWorkflowRegister = fn
}

// RegisterWorkflow registers a workflow definition by ID. Safe to call after
// Start() — concurrent reads in the process loop are protected by a mutex.
func (e *Engine) RegisterWorkflow(def WorkflowDef) {
	e.workflowsMu.Lock()
	e.workflows[def.ID] = def
	e.workflowsMu.Unlock()
	if e.onWorkflowRegister != nil {
		e.onWorkflowRegister(def)
	}
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
		e.throttle.enqueue(env)
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
		e.throttle.removeQueued(corrID)
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
		next, ok := e.throttle.dequeue()
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
		event.HintEmitted, event.HintRejected:
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
