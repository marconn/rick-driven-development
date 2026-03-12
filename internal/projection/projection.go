package projection

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// Projector processes events to maintain a read model.
type Projector interface {
	// Name returns the projector's identifier.
	Name() string

	// Handle processes a single event and updates the projection state.
	Handle(ctx context.Context, env event.Envelope) error
}

// Runner manages projections: catches up from the store then subscribes to live events.
type Runner struct {
	store      eventstore.Store
	bus        eventbus.Bus
	projectors []Projector
	logger     *slog.Logger

	mu           sync.Mutex
	lastPosition int64
	unsub        func()
}

// NewRunner creates a projection runner.
func NewRunner(store eventstore.Store, bus eventbus.Bus, logger *slog.Logger) *Runner {
	return &Runner{
		store:  store,
		bus:    bus,
		logger: logger,
	}
}

// Register adds a projector to the runner. Must be called before Start.
func (r *Runner) Register(p Projector) {
	r.projectors = append(r.projectors, p)
}

const catchUpBatchSize = 500

// Start performs catch-up from the event store then subscribes to live events.
// The catch-up phase runs synchronously; live subscription starts after catch-up completes.
func (r *Runner) Start(ctx context.Context) error {
	if err := r.catchUp(ctx); err != nil {
		return fmt.Errorf("projection: catch-up failed: %w", err)
	}
	r.subscribeLive()
	return nil
}

func (r *Runner) catchUp(ctx context.Context) error {
	for {
		batch, err := r.store.LoadAll(ctx, r.lastPosition, catchUpBatchSize)
		if err != nil {
			return fmt.Errorf("load batch after position %d: %w", r.lastPosition, err)
		}
		if len(batch) == 0 {
			return nil
		}
		for _, pe := range batch {
			if err := r.fanOut(ctx, pe.Event); err != nil {
				return fmt.Errorf("project event %s at position %d: %w",
					pe.Event.ID, pe.Position, err)
			}
			r.mu.Lock()
			r.lastPosition = pe.Position
			r.mu.Unlock()
		}
		if len(batch) < catchUpBatchSize {
			return nil
		}
	}
}

func (r *Runner) subscribeLive() {
	// Sync ensures projections are updated before Publish returns, so MCP
	// tools see consistent state immediately after write operations.
	// All projectors are in-memory map updates — safe to run inline.
	r.unsub = r.bus.SubscribeAll(func(ctx context.Context, env event.Envelope) error {
		return r.fanOut(ctx, env)
	}, eventbus.WithName("projection-runner"), eventbus.WithSync())
}

func (r *Runner) fanOut(ctx context.Context, env event.Envelope) error {
	for _, p := range r.projectors {
		if err := p.Handle(ctx, env); err != nil {
			r.logger.Error("projector failed",
				slog.String("projector", p.Name()),
				slog.String("event_type", string(env.Type)),
				slog.String("event_id", string(env.ID)),
				slog.String("error", err.Error()),
			)
			// Log but don't fail the entire fan-out for one projector's error.
			// Individual projectors are responsible for their own error handling.
		}
	}
	return nil
}

// Stop unsubscribes from live events.
func (r *Runner) Stop() {
	if r.unsub != nil {
		r.unsub()
		r.unsub = nil
	}
}

// Position returns the last processed global position.
func (r *Runner) Position() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastPosition
}

// WorkflowStatus represents the current state of a workflow.
type WorkflowStatus struct {
	AggregateID string
	WorkflowID  string
	Status      string // requested, running, completed, failed, cancelled
	Prompt      string
	Source      string
	Ticket      string
	Phases      []string
	StartedAt   time.Time
	CompletedAt time.Time
	FailReason  string
}

// TokenUsage tracks token consumption for a workflow.
type TokenUsage struct {
	AggregateID string
	Total       int
	ByPhase     map[string]int
	ByBackend   map[string]int
}

// PhaseTimeline tracks timing and iteration count for a phase execution.
type PhaseTimeline struct {
	AggregateID string
	Phase       string
	Iterations  int
	StartedAt   time.Time
	CompletedAt time.Time
	Duration    time.Duration
	Status      string // running, done, failed
}

// VerdictRecord captures a single review verdict for a workflow phase.
type VerdictRecord struct {
	Phase       string
	SourcePhase string
	Outcome     string // "pass", "fail", "unknown"
	Summary     string
	Issues      []VerdictIssue
}

// VerdictIssue is a single finding from a review verdict.
type VerdictIssue struct {
	Severity    string
	Category    string
	Description string
	File        string
	Line        int
}
