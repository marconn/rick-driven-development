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

func makeTestEnvelope(eventType event.Type) event.Envelope {
	return event.Envelope{
		ID:            event.ID("test-id"),
		Type:          eventType,
		AggregateID:   "agg-1",
		Version:       1,
		SchemaVersion: 1,
		Timestamp:     time.Now(),
		CorrelationID: "corr-1",
		Source:        "test",
		Payload:       json.RawMessage(`{}`),
	}
}

func TestPublishSubscribe(t *testing.T) {
	bus := NewChannelBus()
	defer func() { _ = bus.Close() }()

	var received atomic.Bool
	bus.Subscribe("test.event", func(ctx context.Context, env event.Envelope) error {
		received.Store(true)
		return nil
	})

	ctx := context.Background()
	err := bus.Publish(ctx, makeTestEnvelope("test.event"))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Wait for async dispatch
	time.Sleep(50 * time.Millisecond)
	if !received.Load() {
		t.Error("handler was not called")
	}
}

func TestSubscribeFiltering(t *testing.T) {
	bus := NewChannelBus()
	defer func() { _ = bus.Close() }()

	var count atomic.Int32
	bus.Subscribe("target.event", func(ctx context.Context, env event.Envelope) error {
		count.Add(1)
		return nil
	})

	ctx := context.Background()
	_ = bus.Publish(ctx, makeTestEnvelope("other.event"))
	_ = bus.Publish(ctx, makeTestEnvelope("target.event"))
	_ = bus.Publish(ctx, makeTestEnvelope("another.event"))

	time.Sleep(50 * time.Millisecond)
	if count.Load() != 1 {
		t.Errorf("expected 1 call, got %d", count.Load())
	}
}

func TestSubscribeAll(t *testing.T) {
	bus := NewChannelBus()
	defer func() { _ = bus.Close() }()

	var count atomic.Int32
	bus.SubscribeAll(func(ctx context.Context, env event.Envelope) error {
		count.Add(1)
		return nil
	})

	ctx := context.Background()
	_ = bus.Publish(ctx, makeTestEnvelope("event.1"))
	_ = bus.Publish(ctx, makeTestEnvelope("event.2"))
	_ = bus.Publish(ctx, makeTestEnvelope("event.3"))

	time.Sleep(50 * time.Millisecond)
	if count.Load() != 3 {
		t.Errorf("expected 3 calls, got %d", count.Load())
	}
}

func TestUnsubscribe(t *testing.T) {
	bus := NewChannelBus()
	defer func() { _ = bus.Close() }()

	var count atomic.Int32
	unsub := bus.Subscribe("test.event", func(ctx context.Context, env event.Envelope) error {
		count.Add(1)
		return nil
	})

	ctx := context.Background()
	_ = bus.Publish(ctx, makeTestEnvelope("test.event"))
	time.Sleep(50 * time.Millisecond)

	unsub()

	_ = bus.Publish(ctx, makeTestEnvelope("test.event"))
	time.Sleep(50 * time.Millisecond)

	if count.Load() != 1 {
		t.Errorf("expected 1 call after unsubscribe, got %d", count.Load())
	}
}

func TestUnsubscribeAll(t *testing.T) {
	bus := NewChannelBus()
	defer func() { _ = bus.Close() }()

	var count atomic.Int32
	unsub := bus.SubscribeAll(func(ctx context.Context, env event.Envelope) error {
		count.Add(1)
		return nil
	})

	ctx := context.Background()
	_ = bus.Publish(ctx, makeTestEnvelope("test.event"))
	time.Sleep(50 * time.Millisecond)

	unsub()

	_ = bus.Publish(ctx, makeTestEnvelope("test.event"))
	time.Sleep(50 * time.Millisecond)

	if count.Load() != 1 {
		t.Errorf("expected 1 call after unsubscribe, got %d", count.Load())
	}
}

func TestPublishAfterClose(t *testing.T) {
	bus := NewChannelBus()
	_ = bus.Close()

	err := bus.Publish(context.Background(), makeTestEnvelope("test.event"))
	if !errors.Is(err, ErrBusClosed) {
		t.Errorf("expected ErrBusClosed, got: %v", err)
	}
}

func TestMultipleSubscribers(t *testing.T) {
	bus := NewChannelBus()
	defer func() { _ = bus.Close() }()

	var count atomic.Int32
	for range 5 {
		bus.Subscribe("test.event", func(ctx context.Context, env event.Envelope) error {
			count.Add(1)
			return nil
		})
	}

	_ = bus.Publish(context.Background(), makeTestEnvelope("test.event"))
	time.Sleep(50 * time.Millisecond)

	if count.Load() != 5 {
		t.Errorf("expected 5 calls, got %d", count.Load())
	}
}

// mockDeadLetterRecorder captures dead letters in memory for testing.
type mockDeadLetterRecorder struct {
	mu      sync.Mutex
	letters []DeadLetter
}

func (m *mockDeadLetterRecorder) RecordDeadLetter(_ context.Context, dl DeadLetter) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.letters = append(m.letters, dl)
	return nil
}

func (m *mockDeadLetterRecorder) get() []DeadLetter {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]DeadLetter, len(m.letters))
	copy(result, m.letters)
	return result
}

func TestDeadLetterOnError(t *testing.T) {
	recorder := &mockDeadLetterRecorder{}
	bus := NewChannelBus(
		WithLogger(slog.Default()),
		WithDeadLetterRecorder(recorder),
	)
	defer func() { _ = bus.Close() }()

	bus.Subscribe("test.event", func(ctx context.Context, env event.Envelope) error {
		return errors.New("handler failed")
	}, WithName("failing-handler"))

	_ = bus.Publish(context.Background(), makeTestEnvelope("test.event"))
	time.Sleep(50 * time.Millisecond)

	dls := recorder.get()
	if len(dls) != 1 {
		t.Fatalf("expected 1 dead letter, got %d", len(dls))
	}
	if dls[0].Handler != "failing-handler" {
		t.Errorf("expected handler failing-handler, got %s", dls[0].Handler)
	}
}

func TestConcurrentPublish(t *testing.T) {
	bus := NewChannelBus()
	defer func() { _ = bus.Close() }()

	var count atomic.Int32
	bus.Subscribe("test.event", func(ctx context.Context, env event.Envelope) error {
		count.Add(1)
		return nil
	})

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = bus.Publish(context.Background(), makeTestEnvelope("test.event"))
		}()
	}
	wg.Wait()
	time.Sleep(100 * time.Millisecond)

	if count.Load() != 100 {
		t.Errorf("expected 100 calls, got %d", count.Load())
	}
}

// TestSyncHandlerDeadLetterRecording verifies that when a WithSync subscriber
// returns an error, a dead letter IS recorded (same code path as async).
func TestSyncHandlerDeadLetterRecording(t *testing.T) {
	recorder := &mockDeadLetterRecorder{}
	bus := NewChannelBus(
		WithLogger(slog.Default()),
		WithDeadLetterRecorder(recorder),
	)
	defer func() { _ = bus.Close() }()

	bus.Subscribe("test.sync-dl", func(_ context.Context, _ event.Envelope) error {
		return errors.New("sync handler failed")
	}, WithName("sync-failing"), WithSync())

	ctx := context.Background()
	_ = bus.Publish(ctx, makeTestEnvelope("test.sync-dl"))

	// Sync handler completes before Publish returns — no sleep needed.
	dls := recorder.get()
	if len(dls) != 1 {
		t.Fatalf("expected 1 dead letter from sync handler failure, got %d", len(dls))
	}
	if dls[0].Handler != "sync-failing" {
		t.Errorf("expected handler 'sync-failing', got %q", dls[0].Handler)
	}
}

// TestClose_GracefulDrain verifies that Close() waits for in-flight async
// handlers to complete before returning (not cut short).
func TestClose_GracefulDrain(t *testing.T) {
	bus := NewChannelBus()

	const handlerDuration = 100 * time.Millisecond
	var completed atomic.Bool

	bus.Subscribe("test.slow", func(_ context.Context, _ event.Envelope) error {
		time.Sleep(handlerDuration)
		completed.Store(true)
		return nil
	})

	ctx := context.Background()
	_ = bus.Publish(ctx, makeTestEnvelope("test.slow"))

	// Close must block until the slow handler finishes.
	if err := bus.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if !completed.Load() {
		t.Error("Close() returned before the in-flight async handler completed")
	}
}

// TestSubscribeAllWithSync verifies that SubscribeAll respects WithSync(),
// running the handler in the publisher's goroutine (not a spawned goroutine).
// Regression test: SubscribeAll previously ignored the sync config field.
func TestSubscribeAllWithSync(t *testing.T) {
	bus := NewChannelBus()
	defer func() { _ = bus.Close() }()

	var handlerGoroutineID atomic.Int64

	bus.SubscribeAll(func(_ context.Context, _ event.Envelope) error {
		// Record that the handler ran. If sync, this completes before Publish returns.
		handlerGoroutineID.Store(1)
		return nil
	}, WithSync())

	ctx := context.Background()
	_ = bus.Publish(ctx, makeTestEnvelope("test.sync"))

	// If sync, the handler already ran — no sleep needed.
	if handlerGoroutineID.Load() != 1 {
		t.Fatal("WithSync() on SubscribeAll did not run handler synchronously — handler not executed before Publish returned")
	}
}
