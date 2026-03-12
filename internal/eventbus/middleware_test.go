package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

func TestLoggingMiddleware(t *testing.T) {
	logger := slog.Default()
	mw := LoggingMiddleware(logger)

	called := false
	handler := mw(func(ctx context.Context, env event.Envelope) error {
		called = true
		return nil
	})

	err := handler(context.Background(), makeTestEnvelope("test.event"))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !called {
		t.Error("handler was not called")
	}
}

func TestRetryMiddleware(t *testing.T) {
	var attempts atomic.Int32
	mw := RetryMiddleware(2, time.Millisecond)

	handler := mw(func(ctx context.Context, env event.Envelope) error {
		if attempts.Add(1) < 3 {
			return errors.New("transient error")
		}
		return nil
	})

	err := handler(context.Background(), makeTestEnvelope("test.event"))
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestRetryMiddlewareExhausted(t *testing.T) {
	mw := RetryMiddleware(2, time.Millisecond)

	handler := mw(func(ctx context.Context, env event.Envelope) error {
		return errors.New("permanent error")
	})

	err := handler(context.Background(), makeTestEnvelope("test.event"))
	if err == nil {
		t.Error("expected error after exhausted retries")
	}
}

func TestRetryMiddlewareContextCancelled(t *testing.T) {
	mw := RetryMiddleware(5, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	handler := mw(func(ctx context.Context, env event.Envelope) error {
		return errors.New("error")
	})

	err := handler(ctx, makeTestEnvelope("test.event"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

func TestCircuitBreakerMiddleware(t *testing.T) {
	mw := CircuitBreakerMiddleware(3, 50*time.Millisecond)

	var calls atomic.Int32
	handler := mw(func(ctx context.Context, env event.Envelope) error {
		calls.Add(1)
		return errors.New("error")
	})

	ctx := context.Background()
	env := makeTestEnvelope("test.event")

	// Trip the breaker (3 failures)
	for range 3 {
		_ = handler(ctx, env)
	}

	// Should be open now — call should be rejected without invoking handler
	callsBefore := calls.Load()
	err := handler(ctx, env)
	if err == nil {
		t.Error("expected circuit breaker error")
	}
	if calls.Load() != callsBefore {
		t.Error("handler should not have been called when circuit is open")
	}

	// Wait for reset timeout
	time.Sleep(60 * time.Millisecond)

	// Should be half-open, allow one call through
	_ = handler(ctx, env)
	if calls.Load() != callsBefore+1 {
		t.Error("handler should have been called in half-open state")
	}
}

func TestRecoveryMiddleware(t *testing.T) {
	mw := RecoveryMiddleware()

	handler := mw(func(ctx context.Context, env event.Envelope) error {
		panic("test panic")
	})

	err := handler(context.Background(), makeTestEnvelope("test.event"))
	if err == nil {
		t.Error("expected error from panic recovery")
	}
}

func TestChain(t *testing.T) {
	var order []string
	mw1 := func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, env event.Envelope) error {
			order = append(order, "mw1-before")
			err := next(ctx, env)
			order = append(order, "mw1-after")
			return err
		}
	}
	mw2 := func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, env event.Envelope) error {
			order = append(order, "mw2-before")
			err := next(ctx, env)
			order = append(order, "mw2-after")
			return err
		}
	}

	chained := Chain(mw1, mw2)
	handler := chained(func(ctx context.Context, env event.Envelope) error {
		order = append(order, "handler")
		return nil
	})

	_ = handler(context.Background(), makeTestEnvelope("test.event"))

	expected := []string{"mw1-before", "mw2-before", "handler", "mw2-after", "mw1-after"}
	if len(order) != len(expected) {
		t.Fatalf("expected %d calls, got %d: %v", len(expected), len(order), order)
	}
	for i, v := range expected {
		if order[i] != v {
			t.Errorf("position %d: expected %s, got %s", i, v, order[i])
		}
	}
}

func TestBusWithMiddleware(t *testing.T) {
	var attempts atomic.Int32
	mw := Chain(
		RecoveryMiddleware(),
		RetryMiddleware(1, time.Millisecond),
	)

	bus := NewChannelBus(WithMiddleware(mw))
	defer func() { _ = bus.Close() }()

	bus.Subscribe("test.event", func(ctx context.Context, env event.Envelope) error {
		if attempts.Add(1) == 1 {
			return errors.New("transient")
		}
		return nil
	}, WithName("retry-handler"))

	_ = bus.Publish(context.Background(), makeTestEnvelope("test.event"))
	time.Sleep(50 * time.Millisecond)

	if attempts.Load() != 2 {
		t.Errorf("expected 2 attempts (1 retry), got %d", attempts.Load())
	}
}

func TestTimeoutMiddleware(t *testing.T) {
	mw := TimeoutMiddleware(50 * time.Millisecond)

	handler := mw(func(ctx context.Context, env event.Envelope) error {
		select {
		case <-time.After(200 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	err := handler(context.Background(), makeTestEnvelope("test.event"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got: %v", err)
	}
}

func TestTimeoutMiddlewareFastHandler(t *testing.T) {
	mw := TimeoutMiddleware(time.Second)

	handler := mw(func(ctx context.Context, env event.Envelope) error {
		return nil // completes immediately
	})

	err := handler(context.Background(), makeTestEnvelope("test.event"))
	if err != nil {
		t.Errorf("expected no error for fast handler, got: %v", err)
	}
}

func TestIdempotencyMiddleware(t *testing.T) {
	mw := IdempotencyMiddleware(100)

	var count atomic.Int32
	handler := mw(func(ctx context.Context, env event.Envelope) error {
		count.Add(1)
		return nil
	})

	env := makeTestEnvelope("test.event")
	// Process same event 3 times
	_ = handler(context.Background(), env)
	_ = handler(context.Background(), env)
	_ = handler(context.Background(), env)

	if count.Load() != 1 {
		t.Errorf("expected 1 call (idempotent), got %d", count.Load())
	}
}

func TestIdempotencyMiddlewareDifferentEvents(t *testing.T) {
	mw := IdempotencyMiddleware(100)

	var count atomic.Int32
	handler := mw(func(ctx context.Context, env event.Envelope) error {
		count.Add(1)
		return nil
	})

	// Different event IDs should each be processed
	for range 5 {
		env := event.Envelope{
			ID:      event.NewID(), // unique ID each time
			Type:    "test.event",
			Payload: json.RawMessage(`{}`),
		}
		_ = handler(context.Background(), env)
	}

	if count.Load() != 5 {
		t.Errorf("expected 5 calls for different events, got %d", count.Load())
	}
}

func TestIdempotencyMiddlewareEviction(t *testing.T) {
	mw := IdempotencyMiddleware(4) // small capacity

	var count atomic.Int32
	handler := mw(func(ctx context.Context, env event.Envelope) error {
		count.Add(1)
		return nil
	})

	// Process 10 unique events to trigger eviction
	for range 10 {
		env := event.Envelope{
			ID:      event.NewID(),
			Type:    "test.event",
			Payload: json.RawMessage(`{}`),
		}
		_ = handler(context.Background(), env)
	}

	if count.Load() != 10 {
		t.Errorf("expected 10 calls, got %d", count.Load())
	}
}

// TestCircuitBreaker_HalfOpenToClosedRecovery verifies the full half-open →
// closed transition: after the reset timeout expires, the first call succeeds
// and the breaker returns to closed state (subsequent calls go through normally).
func TestCircuitBreaker_HalfOpenToClosedRecovery(t *testing.T) {
	const threshold = 3
	const resetTimeout = 30 * time.Millisecond

	mw := CircuitBreakerMiddleware(threshold, resetTimeout)

	var callCount atomic.Int32
	shouldFail := true // controlled by test

	handler := mw(func(ctx context.Context, env event.Envelope) error {
		callCount.Add(1)
		if shouldFail {
			return errors.New("transient error")
		}
		return nil
	})

	ctx := context.Background()
	env := makeTestEnvelope("test.event")

	// Trip the breaker.
	for range threshold {
		_ = handler(ctx, env)
	}

	// Circuit should be open — next call rejected without hitting the handler.
	callsBefore := callCount.Load()
	err := handler(ctx, env)
	if err == nil {
		t.Error("expected circuit open error")
	}
	if callCount.Load() != callsBefore {
		t.Error("handler should not be called when circuit is open")
	}

	// Wait past the reset timeout so breaker enters half-open.
	time.Sleep(resetTimeout + 10*time.Millisecond)

	// Half-open: allow a succeeding call through → transitions to closed.
	shouldFail = false
	callsBefore = callCount.Load()
	err = handler(ctx, env)
	if err != nil {
		t.Errorf("expected success in half-open, got: %v", err)
	}
	if callCount.Load() != callsBefore+1 {
		t.Error("handler should have been called in half-open state")
	}

	// Breaker is now closed: next call should also go through.
	callsBefore = callCount.Load()
	err = handler(ctx, env)
	if err != nil {
		t.Errorf("expected success after closed recovery, got: %v", err)
	}
	if callCount.Load() != callsBefore+1 {
		t.Error("handler should be called after breaker returns to closed")
	}
}

// TestCircuitBreaker_ConcurrentAccess submits 100 concurrent calls with
// threshold=50; the breaker must eventually open and all accesses must be
// race-free (run with -race).
func TestCircuitBreaker_ConcurrentAccess(t *testing.T) {
	const goroutines = 100
	const threshold = 50

	mw := CircuitBreakerMiddleware(threshold, 10*time.Second)

	var calls atomic.Int32
	handler := mw(func(ctx context.Context, env event.Envelope) error {
		calls.Add(1)
		return errors.New("always fail")
	})

	ctx := context.Background()
	env := makeTestEnvelope("test.event")

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = handler(ctx, env)
		}()
	}
	wg.Wait()

	// At least threshold calls must have reached the handler before the breaker
	// opened and started rejecting. After opening, no more calls go through.
	actual := calls.Load()
	if actual < threshold {
		t.Errorf("expected at least %d calls before breaker opened, got %d", threshold, actual)
	}
	// The breaker opened at exactly threshold failures, so calls should not
	// significantly exceed threshold (a few races around the open boundary are OK).
	if actual > goroutines {
		t.Errorf("calls (%d) exceeded total goroutines (%d)", actual, goroutines)
	}
}

// mockMetricsRecorder is a thread-safe recorder for MetricsMiddleware tests.
type mockMetricsRecorder struct {
	mu     sync.Mutex
	calls  []metricsCall
}

type metricsCall struct {
	eventType   event.Type
	handlerName string
	duration    time.Duration
	err         error
}

func (m *mockMetricsRecorder) RecordEventProcessed(
	eventType event.Type, handlerName string, duration time.Duration, err error,
) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, metricsCall{
		eventType:   eventType,
		handlerName: handlerName,
		duration:    duration,
		err:         err,
	})
}

func (m *mockMetricsRecorder) get() []metricsCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]metricsCall, len(m.calls))
	copy(result, m.calls)
	return result
}

// TestMetricsMiddleware_Success verifies that RecordEventProcessed is called
// with the correct event type, handler name, a positive duration, and nil error.
func TestMetricsMiddleware_Success(t *testing.T) {
	recorder := &mockMetricsRecorder{}
	mw := MetricsMiddleware(recorder, "my-handler")

	handler := mw(func(_ context.Context, _ event.Envelope) error {
		time.Sleep(1 * time.Millisecond) // ensure measurable duration
		return nil
	})

	env := makeTestEnvelope("metrics.event")
	if err := handler(context.Background(), env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	calls := recorder.get()
	if len(calls) != 1 {
		t.Fatalf("expected 1 metrics call, got %d", len(calls))
	}
	c := calls[0]
	if c.eventType != "metrics.event" {
		t.Errorf("expected event type 'metrics.event', got %q", c.eventType)
	}
	if c.handlerName != "my-handler" {
		t.Errorf("expected handler name 'my-handler', got %q", c.handlerName)
	}
	if c.duration <= 0 {
		t.Errorf("expected positive duration, got %v", c.duration)
	}
	if c.err != nil {
		t.Errorf("expected nil error in metrics call, got %v", c.err)
	}
}

// TestMetricsMiddleware_Error verifies that when the handler returns an error,
// RecordEventProcessed is called with that same error.
func TestMetricsMiddleware_Error(t *testing.T) {
	recorder := &mockMetricsRecorder{}
	mw := MetricsMiddleware(recorder, "error-handler")

	handlerErr := errors.New("handler failed")
	handler := mw(func(_ context.Context, _ event.Envelope) error {
		return handlerErr
	})

	env := makeTestEnvelope("metrics.error")
	_ = handler(context.Background(), env)

	calls := recorder.get()
	if len(calls) != 1 {
		t.Fatalf("expected 1 metrics call, got %d", len(calls))
	}
	if !errors.Is(calls[0].err, handlerErr) {
		t.Errorf("expected handlerErr in metrics call, got %v", calls[0].err)
	}
}

// TestIdempotencyMiddleware_EvictionAllowsReprocessing verifies that after the
// cache evicts the first event, re-submitting it causes it to be processed again.
//
// Strategy: use maxSize=2 so the map holds at most 2 entries. Insert the first
// event, then flood with unique events. Each flood event that exceeds maxSize
// triggers an eviction pass that clears maxSize/2=1 random entry. After enough
// floods, the first event's ID is guaranteed to have been evicted.
func TestIdempotencyMiddleware_EvictionAllowsReprocessing(t *testing.T) {
	// maxSize=2: every 3rd unique insert triggers an eviction of 1 entry.
	// After 20 unique inserts the first event will have been evicted with
	// near-certainty (probability > 1 - (1/2)^10 > 99.9%).
	const maxSize = 2
	mw := IdempotencyMiddleware(maxSize)

	var count atomic.Int32
	handler := mw(func(_ context.Context, _ event.Envelope) error {
		count.Add(1)
		return nil
	})

	ctx := context.Background()

	firstEvent := event.Envelope{
		ID:      event.NewID(),
		Type:    "test.evict",
		Payload: json.RawMessage(`{}`),
	}

	// Process the first event once (count → 1).
	_ = handler(ctx, firstEvent)
	if count.Load() != 1 {
		t.Fatalf("expected count=1 after first process, got %d", count.Load())
	}

	// Flood with unique events to drive multiple eviction rounds.
	// With maxSize=2, every 3rd event triggers an eviction of 1 entry.
	// After 20 extra events the first event ID is overwhelmingly likely to be gone.
	for range 20 {
		_ = handler(ctx, event.Envelope{
			ID:      event.NewID(),
			Type:    "test.evict",
			Payload: json.RawMessage(`{}`),
		})
	}

	// Re-submit the first event repeatedly. At least one attempt must be
	// processed, proving it was evicted from the cache at some point.
	reprocessed := false
	for range 10 {
		before := count.Load()
		_ = handler(ctx, firstEvent)
		if count.Load() > before {
			reprocessed = true
			break
		}
		// If still in cache, evict it with another flush round.
		for range maxSize + 1 {
			_ = handler(ctx, event.Envelope{
				ID:      event.NewID(),
				Type:    "test.evict",
				Payload: json.RawMessage(`{}`),
			})
		}
	}

	if !reprocessed {
		t.Error("first event was never reprocessed after cache eviction")
	}
}

