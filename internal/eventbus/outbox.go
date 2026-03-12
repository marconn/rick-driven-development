package eventbus

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// PollResult pairs an event with its global position in the store.
type PollResult struct {
	Position int64
	Event    event.Envelope
}

// PollFunc reads events from persistent storage starting after afterPosition.
// Provided by the caller (typically wrapping eventstore.Store.LoadAll) to keep
// this package free of eventstore import cycles.
type PollFunc func(ctx context.Context, afterPosition int64, limit int) ([]PollResult, error)

// OutboxBus implements Bus by polling the event store rather than using in-memory channels.
// Events survive process crashes because they're already persisted via Store.Append()
// before Publish() is called. Publish() is therefore a no-op hint: it nudges the
// poll loop to check immediately rather than waiting for the next tick.
type OutboxBus struct {
	mu          sync.RWMutex
	subscribers map[event.Type][]*subscription
	allSubs     []*subscription
	middleware  Middleware
	logger      *slog.Logger
	closed      atomic.Bool
	wg          sync.WaitGroup
	dlRecorder  DeadLetterRecorder

	poll         PollFunc
	pollInterval time.Duration
	batchSize    int
	lastPosition atomic.Int64 // last processed global position; written by poll goroutine, read externally

	cancel context.CancelFunc
	notify chan struct{} // buffered(1): signal an immediate poll pass
}

// OutboxOption configures an OutboxBus.
type OutboxOption func(*OutboxBus)

// WithOutboxMiddleware sets the middleware chain applied at subscribe-time.
func WithOutboxMiddleware(mw Middleware) OutboxOption {
	return func(b *OutboxBus) { b.middleware = mw }
}

// WithOutboxLogger sets the structured logger.
func WithOutboxLogger(logger *slog.Logger) OutboxOption {
	return func(b *OutboxBus) { b.logger = logger }
}

// WithOutboxDeadLetterRecorder sets the dead-letter recorder.
func WithOutboxDeadLetterRecorder(recorder DeadLetterRecorder) OutboxOption {
	return func(b *OutboxBus) { b.dlRecorder = recorder }
}

// WithPollInterval sets how frequently to poll for new events. Default: 100ms.
func WithPollInterval(d time.Duration) OutboxOption {
	return func(b *OutboxBus) { b.pollInterval = d }
}

// WithBatchSize sets the maximum number of events fetched per poll pass. Default: 100.
func WithBatchSize(size int) OutboxOption {
	return func(b *OutboxBus) { b.batchSize = size }
}

// WithStartPosition sets the global position to begin polling from on first start.
// Use this to resume from a known checkpoint rather than replaying the full history.
func WithStartPosition(pos int64) OutboxOption {
	return func(b *OutboxBus) { b.lastPosition.Store(pos) }
}

// NewOutboxBus creates a polling-based event bus. Call Start to begin polling.
func NewOutboxBus(poll PollFunc, opts ...OutboxOption) *OutboxBus {
	b := &OutboxBus{
		subscribers:  make(map[event.Type][]*subscription),
		logger:       slog.Default(),
		poll:         poll,
		pollInterval: 100 * time.Millisecond,
		batchSize:    100,
		notify:       make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Start begins the background polling loop. The provided ctx controls the loop's
// lifetime; cancelling ctx is equivalent to calling Close.
func (b *OutboxBus) Start(ctx context.Context) {
	ctx, b.cancel = context.WithCancel(ctx)
	b.wg.Add(1)
	go b.pollLoop(ctx)
}

func (b *OutboxBus) pollLoop(ctx context.Context) {
	defer b.wg.Done()
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.pollOnce(ctx)
		case <-b.notify:
			b.pollOnce(ctx)
		}
	}
}

func (b *OutboxBus) pollOnce(ctx context.Context) {
	results, err := b.poll(ctx, b.lastPosition.Load(), b.batchSize)
	if err != nil {
		if ctx.Err() != nil {
			return // context cancelled; suppress error noise
		}
		b.logger.Error("outbox: poll failed", slog.String("error", err.Error()))
		return
	}

	for _, r := range results {
		b.dispatchEvent(ctx, r.Event)
		b.lastPosition.Store(r.Position)
	}
}

// dispatchEvent delivers env to all matching subscribers synchronously.
// Running within the single poll goroutine guarantees per-subscriber ordering.
func (b *OutboxBus) dispatchEvent(ctx context.Context, env event.Envelope) {
	b.mu.RLock()
	typed := make([]*subscription, len(b.subscribers[env.Type]))
	copy(typed, b.subscribers[env.Type])
	all := make([]*subscription, len(b.allSubs))
	copy(all, b.allSubs)
	b.mu.RUnlock()

	for _, sub := range append(typed, all...) {
		if err := sub.handler(ctx, env); err != nil {
			b.logger.Error("outbox: event delivery failed",
				slog.String("event_type", string(env.Type)),
				slog.String("event_id", string(env.ID)),
				slog.String("subscriber", sub.name),
				slog.String("error", err.Error()),
			)
			b.recordDeadLetter(ctx, env, sub.name, err)
		}
	}
}

func (b *OutboxBus) recordDeadLetter(ctx context.Context, env event.Envelope, handler string, handlerErr error) {
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
		b.logger.Error("outbox: failed to record dead letter",
			slog.String("event_id", string(env.ID)),
			slog.String("error", err.Error()),
		)
	}
}

// Publish nudges the poll loop to check for new events immediately.
// The event itself must already be persisted via Store.Append() before this call.
// If the bus has not been started yet, the signal is silently dropped — the next
// ticker tick will pick up the event.
func (b *OutboxBus) Publish(_ context.Context, _ event.Envelope) error {
	if b.closed.Load() {
		return ErrBusClosed
	}
	// Non-blocking: if a notification is already pending, skip.
	select {
	case b.notify <- struct{}{}:
	default:
	}
	return nil
}

// Subscribe registers a handler for a specific event type.
// Returns an unsubscribe function that is safe to call multiple times.
func (b *OutboxBus) Subscribe(eventType event.Type, handler HandlerFunc, opts ...SubscribeOption) func() {
	cfg := &subscribeConfig{name: "anonymous"}
	for _, opt := range opts {
		opt(cfg)
	}

	wrapped := handler
	if b.middleware != nil {
		wrapped = b.middleware(handler)
	}

	sub := &subscription{
		id:      subCounter.Add(1),
		name:    cfg.name,
		handler: wrapped,
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

// SubscribeAll registers a handler that receives every event type.
// Returns an unsubscribe function.
func (b *OutboxBus) SubscribeAll(handler HandlerFunc, opts ...SubscribeOption) func() {
	cfg := &subscribeConfig{name: "anonymous-all"}
	for _, opt := range opts {
		opt(cfg)
	}

	wrapped := handler
	if b.middleware != nil {
		wrapped = b.middleware(handler)
	}

	sub := &subscription{
		id:      subCounter.Add(1),
		name:    cfg.name,
		handler: wrapped,
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

// Close stops the polling loop and waits for the goroutine to exit.
func (b *OutboxBus) Close() error {
	b.closed.Store(true)
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()
	return nil
}

// LastPosition returns the last global position successfully dispatched.
// Useful for checkpointing: store this value and pass it via WithStartPosition
// on the next process start to avoid replaying already-handled events.
func (b *OutboxBus) LastPosition() int64 {
	return b.lastPosition.Load()
}
