package eventbus

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// DeadLetterRecorder persists failed event deliveries.
// Implemented by eventstore.SQLiteStore.
type DeadLetterRecorder interface {
	RecordDeadLetter(ctx context.Context, dl DeadLetter) error
}

// DeadLetter records a failed event delivery.
type DeadLetter struct {
	ID       string `json:"id"`
	EventID  string `json:"event_id"`
	Handler  string `json:"handler"`
	Error    string `json:"error"`
	Attempts int    `json:"attempts"`
	FailedAt string `json:"failed_at"`
}

// ChannelBus implements Bus using Go channels for in-process pub/sub.
type ChannelBus struct {
	mu          sync.RWMutex
	subscribers map[event.Type][]*subscription
	allSubs     []*subscription
	middleware  Middleware
	logger      *slog.Logger
	closed      atomic.Bool
	wg          sync.WaitGroup
	dlRecorder  DeadLetterRecorder
}

type subscription struct {
	id      uint64
	name    string
	handler HandlerFunc
	sync    bool // run in publisher's goroutine (preserves event ordering)
}

var subCounter atomic.Uint64

// Option configures a ChannelBus.
type Option func(*ChannelBus)

// WithMiddleware sets the middleware chain for all handlers.
func WithMiddleware(mw Middleware) Option {
	return func(b *ChannelBus) {
		b.middleware = mw
	}
}

// WithLogger sets the logger for the bus.
func WithLogger(logger *slog.Logger) Option {
	return func(b *ChannelBus) {
		b.logger = logger
	}
}

// WithDeadLetterRecorder sets the dead letter recorder for persistent failure tracking.
func WithDeadLetterRecorder(recorder DeadLetterRecorder) Option {
	return func(b *ChannelBus) {
		b.dlRecorder = recorder
	}
}

// NewChannelBus creates a new channel-based event bus.
func NewChannelBus(opts ...Option) *ChannelBus {
	b := &ChannelBus{
		subscribers: make(map[event.Type][]*subscription),
		logger:      slog.Default(),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Publish sends an event to all matching subscribers asynchronously.
func (b *ChannelBus) Publish(ctx context.Context, env event.Envelope) error {
	if b.closed.Load() {
		return ErrBusClosed
	}

	b.mu.RLock()
	// Collect typed subscribers
	typed := make([]*subscription, len(b.subscribers[env.Type]))
	copy(typed, b.subscribers[env.Type])
	// Collect all-subscribers
	all := make([]*subscription, len(b.allSubs))
	copy(all, b.allSubs)
	b.mu.RUnlock()

	// Dispatch to all matching subscribers.
	// Sync subscribers run in the caller's goroutine first (preserving order),
	// then async subscribers are spawned.
	for _, sub := range append(typed, all...) {
		if sub.sync {
			b.dispatchSync(ctx, env, sub)
		} else {
			b.dispatch(ctx, env, sub)
		}
	}
	return nil
}

func (b *ChannelBus) dispatchSync(ctx context.Context, env event.Envelope, sub *subscription) {
	if err := sub.handler(ctx, env); err != nil {
		b.logger.Error("event delivery failed (sync)",
			slog.String("event_type", string(env.Type)),
			slog.String("event_id", string(env.ID)),
			slog.String("subscriber", sub.name),
			slog.String("error", err.Error()),
		)
		b.recordDeadLetter(ctx, env, sub.name, err)
	}
}

func (b *ChannelBus) dispatch(ctx context.Context, env event.Envelope, sub *subscription) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		if err := sub.handler(ctx, env); err != nil {
			b.logger.Error("event delivery failed",
				slog.String("event_type", string(env.Type)),
				slog.String("event_id", string(env.ID)),
				slog.String("subscriber", sub.name),
				slog.String("error", err.Error()),
			)
			b.recordDeadLetter(ctx, env, sub.name, err)
		}
	}()
}

func (b *ChannelBus) recordDeadLetter(ctx context.Context, env event.Envelope, handler string, handlerErr error) {
	if b.dlRecorder == nil {
		return
	}
	dl := DeadLetter{
		ID:       string(event.NewID()),
		EventID:  string(env.ID),
		Handler:  handler,
		Error:    handlerErr.Error(),
		Attempts: 1,
		FailedAt: env.Timestamp.String(),
	}
	if err := b.dlRecorder.RecordDeadLetter(ctx, dl); err != nil {
		b.logger.Error("failed to record dead letter",
			slog.String("event_id", string(env.ID)),
			slog.String("error", err.Error()),
		)
	}
}

// Subscribe registers a handler for a specific event type.
// Returns an unsubscribe function.
func (b *ChannelBus) Subscribe(eventType event.Type, handler HandlerFunc, opts ...SubscribeOption) func() {
	cfg := &subscribeConfig{name: "anonymous"}
	for _, opt := range opts {
		opt(cfg)
	}

	// Wrap middleware at subscribe-time, not per-dispatch
	wrapped := handler
	if b.middleware != nil {
		wrapped = b.middleware(handler)
	}

	sub := &subscription{
		id:      subCounter.Add(1),
		name:    cfg.name,
		handler: wrapped,
		sync:    cfg.sync,
	}

	b.mu.Lock()
	b.subscribers[eventType] = append(b.subscribers[eventType], sub)
	b.mu.Unlock()

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		subs := b.subscribers[eventType]
		for i, s := range subs {
			if s.id == sub.id {
				b.subscribers[eventType] = append(subs[:i], subs[i+1:]...)
				return
			}
		}
	}
}

// SubscribeAll registers a handler for all event types (projections, logging).
// Returns an unsubscribe function.
func (b *ChannelBus) SubscribeAll(handler HandlerFunc, opts ...SubscribeOption) func() {
	cfg := &subscribeConfig{name: "anonymous-all"}
	for _, opt := range opts {
		opt(cfg)
	}

	// Wrap middleware at subscribe-time, not per-dispatch
	wrapped := handler
	if b.middleware != nil {
		wrapped = b.middleware(handler)
	}

	sub := &subscription{
		id:      subCounter.Add(1),
		name:    cfg.name,
		handler: wrapped,
		sync:    cfg.sync,
	}

	b.mu.Lock()
	b.allSubs = append(b.allSubs, sub)
	b.mu.Unlock()

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, s := range b.allSubs {
			if s.id == sub.id {
				b.allSubs = append(b.allSubs[:i], b.allSubs[i+1:]...)
				return
			}
		}
	}
}

// Close shuts down the bus and waits for in-flight events to complete.
func (b *ChannelBus) Close() error {
	b.closed.Store(true)
	b.wg.Wait()
	return nil
}
