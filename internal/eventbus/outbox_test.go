package eventbus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// testPollSource provides a controllable, thread-safe event source for testing.
type testPollSource struct {
	mu     sync.Mutex
	events []PollResult
	pollFn func() error // optional hook to inject errors
}

func (s *testPollSource) add(env event.Envelope, position int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, PollResult{Position: position, Event: env})
}

func (s *testPollSource) poll(ctx context.Context, afterPosition int64, limit int) ([]PollResult, error) {
	if s.pollFn != nil {
		if err := s.pollFn(); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var results []PollResult
	for _, r := range s.events {
		if r.Position > afterPosition {
			results = append(results, r)
			if limit > 0 && len(results) >= limit {
				break
			}
		}
	}
	return results, nil
}

// startBus creates an OutboxBus, starts it, and returns a cleanup function.
func startBus(t *testing.T, src *testPollSource, opts ...OutboxOption) (*OutboxBus, context.CancelFunc) {
	t.Helper()
	defaults := []OutboxOption{
		WithPollInterval(5 * time.Millisecond),
	}
	bus := NewOutboxBus(src.poll, append(defaults, opts...)...)
	ctx, cancel := context.WithCancel(context.Background())
	bus.Start(ctx)
	t.Cleanup(func() {
		cancel()
		_ = bus.Close()
	})
	return bus, cancel
}

// waitFor polls cond until it returns true or timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

// TestOutboxBus_PollDeliversEvents verifies that events added to the source
// are delivered to a registered subscriber via the poll loop.
func TestOutboxBus_PollDeliversEvents(t *testing.T) {
	src := &testPollSource{}

	var received atomic.Int32
	bus, _ := startBus(t, src)
	bus.Subscribe("test.event", func(_ context.Context, env event.Envelope) error {
		received.Add(1)
		return nil
	})

	src.add(makeTestEnvelope("test.event"), 1)
	src.add(makeTestEnvelope("test.event"), 2)

	waitFor(t, 500*time.Millisecond, func() bool { return received.Load() == 2 })
}

// TestOutboxBus_SubscribeFiltersByType ensures a typed subscriber only receives
// events matching its registered type.
func TestOutboxBus_SubscribeFiltersByType(t *testing.T) {
	src := &testPollSource{}

	var received atomic.Int32
	bus, _ := startBus(t, src)
	bus.Subscribe("target.event", func(_ context.Context, _ event.Envelope) error {
		received.Add(1)
		return nil
	})

	src.add(makeTestEnvelope("other.event"), 1)
	src.add(makeTestEnvelope("target.event"), 2)
	src.add(makeTestEnvelope("another.event"), 3)

	waitFor(t, 500*time.Millisecond, func() bool { return received.Load() == 1 })

	// Give the bus extra time to confirm no spurious deliveries.
	time.Sleep(30 * time.Millisecond)
	if received.Load() != 1 {
		t.Errorf("expected 1 delivery, got %d", received.Load())
	}
}

// TestOutboxBus_SubscribeAll verifies that an all-subscriber receives every event type.
func TestOutboxBus_SubscribeAll(t *testing.T) {
	src := &testPollSource{}

	var received atomic.Int32
	bus, _ := startBus(t, src)
	bus.SubscribeAll(func(_ context.Context, _ event.Envelope) error {
		received.Add(1)
		return nil
	})

	src.add(makeTestEnvelope("event.a"), 1)
	src.add(makeTestEnvelope("event.b"), 2)
	src.add(makeTestEnvelope("event.c"), 3)

	waitFor(t, 500*time.Millisecond, func() bool { return received.Load() == 3 })
}

// TestOutboxBus_Unsubscribe verifies that after unsubscribing, the handler no
// longer receives events.
func TestOutboxBus_Unsubscribe(t *testing.T) {
	src := &testPollSource{}

	var received atomic.Int32
	bus, _ := startBus(t, src)
	unsub := bus.Subscribe("test.event", func(_ context.Context, _ event.Envelope) error {
		received.Add(1)
		return nil
	})

	// First event arrives before unsubscribe.
	src.add(makeTestEnvelope("test.event"), 1)
	waitFor(t, 500*time.Millisecond, func() bool { return received.Load() == 1 })

	unsub()

	// Second event should NOT be delivered.
	src.add(makeTestEnvelope("test.event"), 2)
	time.Sleep(50 * time.Millisecond)

	if received.Load() != 1 {
		t.Errorf("expected 1 delivery after unsubscribe, got %d", received.Load())
	}
}

// TestOutboxBus_PublishTriggersImmediatePoll checks that Publish sends a notify
// signal causing a poll before the next tick.
func TestOutboxBus_PublishTriggersImmediatePoll(t *testing.T) {
	src := &testPollSource{}

	var received atomic.Int32
	// Use a very long poll interval so we can distinguish tick vs. signal delivery.
	bus := NewOutboxBus(src.poll, WithPollInterval(10*time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = bus.Close() })

	bus.Subscribe("test.event", func(_ context.Context, _ event.Envelope) error {
		received.Add(1)
		return nil
	})
	bus.Start(ctx)

	src.add(makeTestEnvelope("test.event"), 1)
	_ = bus.Publish(ctx, makeTestEnvelope("test.event")) // nudge

	// Should arrive well before the 10-second tick.
	waitFor(t, 500*time.Millisecond, func() bool { return received.Load() == 1 })
}

// TestOutboxBus_ClosedBusRejectsPublish verifies that publishing to a closed
// bus returns ErrBusClosed.
func TestOutboxBus_ClosedBusRejectsPublish(t *testing.T) {
	src := &testPollSource{}
	bus := NewOutboxBus(src.poll)
	_ = bus.Close()

	err := bus.Publish(context.Background(), makeTestEnvelope("test.event"))
	if !errors.Is(err, ErrBusClosed) {
		t.Errorf("expected ErrBusClosed, got: %v", err)
	}
}

// TestOutboxBus_MiddlewareApplied verifies that middleware wraps handlers
// applied at subscribe-time.
func TestOutboxBus_MiddlewareApplied(t *testing.T) {
	src := &testPollSource{}

	var middlewareCalled atomic.Bool
	mw := Middleware(func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, env event.Envelope) error {
			middlewareCalled.Store(true)
			return next(ctx, env)
		}
	})

	bus, _ := startBus(t, src, WithOutboxMiddleware(mw))
	bus.Subscribe("test.event", func(_ context.Context, _ event.Envelope) error {
		return nil
	})

	src.add(makeTestEnvelope("test.event"), 1)
	waitFor(t, 500*time.Millisecond, func() bool { return middlewareCalled.Load() })
}

// TestOutboxBus_DeadLetterOnHandlerError verifies that a handler returning an
// error triggers the dead-letter recorder.
func TestOutboxBus_DeadLetterOnHandlerError(t *testing.T) {
	src := &testPollSource{}
	recorder := &mockDeadLetterRecorder{}

	bus, _ := startBus(t, src, WithOutboxDeadLetterRecorder(recorder))
	bus.Subscribe("test.event", func(_ context.Context, _ event.Envelope) error {
		return errors.New("intentional failure")
	}, WithName("failing-handler"))

	src.add(makeTestEnvelope("test.event"), 1)

	waitFor(t, 500*time.Millisecond, func() bool { return len(recorder.get()) == 1 })

	dls := recorder.get()
	if dls[0].Handler != "failing-handler" {
		t.Errorf("expected handler 'failing-handler', got %q", dls[0].Handler)
	}
	if dls[0].Error != "intentional failure" {
		t.Errorf("expected error 'intentional failure', got %q", dls[0].Error)
	}
}

// TestOutboxBus_PositionTracking verifies that lastPosition advances with each
// batch and that events are not redelivered on subsequent polls.
func TestOutboxBus_PositionTracking(t *testing.T) {
	src := &testPollSource{}

	var received atomic.Int32
	bus, _ := startBus(t, src)
	bus.Subscribe("test.event", func(_ context.Context, _ event.Envelope) error {
		received.Add(1)
		return nil
	})

	src.add(makeTestEnvelope("test.event"), 1)
	src.add(makeTestEnvelope("test.event"), 2)
	src.add(makeTestEnvelope("test.event"), 3)

	waitFor(t, 500*time.Millisecond, func() bool { return received.Load() == 3 })

	if bus.LastPosition() != 3 {
		t.Errorf("expected lastPosition 3, got %d", bus.LastPosition())
	}

	// Allow several more poll passes; count must stay at 3.
	time.Sleep(40 * time.Millisecond)
	if received.Load() != 3 {
		t.Errorf("events redelivered: expected 3, got %d", received.Load())
	}
}

// TestOutboxBus_PollErrorDoesNotCrash verifies that a transient poll error is
// logged but does not crash or stop the polling loop.
func TestOutboxBus_PollErrorDoesNotCrash(t *testing.T) {
	var callCount atomic.Int32
	errSrc := &testPollSource{}

	// First N calls return an error; subsequent calls succeed.
	errSrc.pollFn = func() error {
		if callCount.Add(1) <= 3 {
			return errors.New("transient DB error")
		}
		errSrc.pollFn = nil // stop injecting errors
		return nil
	}

	var received atomic.Int32
	bus, _ := startBus(t, errSrc)
	bus.Subscribe("test.event", func(_ context.Context, _ event.Envelope) error {
		received.Add(1)
		return nil
	})

	// Add the event after the error injection clears itself.
	// The poll loop must still be alive by then.
	errSrc.add(makeTestEnvelope("test.event"), 1)

	waitFor(t, 500*time.Millisecond, func() bool { return received.Load() == 1 })
}

// TestOutboxBus_CloseStopsPollLoop verifies that Close cancels the goroutine
// and wg.Wait() returns promptly.
func TestOutboxBus_CloseStopsPollLoop(t *testing.T) {
	src := &testPollSource{}
	bus := NewOutboxBus(src.poll, WithPollInterval(5*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus.Start(ctx)

	done := make(chan struct{})
	go func() {
		_ = bus.Close()
		close(done)
	}()

	select {
	case <-done:
		// closed cleanly
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not return within 2s")
	}
}

// TestOutboxBus_MultipleSubscribers_PartialFailure verifies that with two
// subscribers on the same event type, one succeeding and one failing:
//   - the successful subscriber processes the event
//   - a dead letter is recorded for the failing subscriber only
func TestOutboxBus_MultipleSubscribers_PartialFailure(t *testing.T) {
	src := &testPollSource{}
	recorder := &mockDeadLetterRecorder{}

	var successCount atomic.Int32
	bus, _ := startBus(t, src, WithOutboxDeadLetterRecorder(recorder))

	// Subscriber 1: succeeds
	bus.Subscribe("test.event", func(_ context.Context, _ event.Envelope) error {
		successCount.Add(1)
		return nil
	}, WithName("good-handler"))

	// Subscriber 2: always fails
	bus.Subscribe("test.event", func(_ context.Context, _ event.Envelope) error {
		return errors.New("bad handler error")
	}, WithName("bad-handler"))

	src.add(makeTestEnvelope("test.event"), 1)

	// Wait for both subscribers to be called.
	waitFor(t, 500*time.Millisecond, func() bool {
		return successCount.Load() == 1 && len(recorder.get()) == 1
	})

	// Successful subscriber received the event.
	if successCount.Load() != 1 {
		t.Errorf("expected good handler to process event once, got %d", successCount.Load())
	}

	// Dead letter recorded only for the failing subscriber.
	dls := recorder.get()
	if len(dls) != 1 {
		t.Fatalf("expected 1 dead letter (bad handler), got %d", len(dls))
	}
	if dls[0].Handler != "bad-handler" {
		t.Errorf("expected dead letter handler 'bad-handler', got %q", dls[0].Handler)
	}
}

// TestOutboxBus_StartPosition verifies that WithStartPosition skips events at
// or before the given position.
func TestOutboxBus_StartPosition(t *testing.T) {
	src := &testPollSource{}
	src.add(makeTestEnvelope("test.event"), 1)
	src.add(makeTestEnvelope("test.event"), 2)
	src.add(makeTestEnvelope("test.event"), 3)

	var received atomic.Int32
	bus, _ := startBus(t, src, WithStartPosition(2))
	bus.Subscribe("test.event", func(_ context.Context, _ event.Envelope) error {
		received.Add(1)
		return nil
	})

	// Only position 3 is after the start position.
	waitFor(t, 500*time.Millisecond, func() bool { return received.Load() == 1 })

	time.Sleep(30 * time.Millisecond)
	if received.Load() != 1 {
		t.Errorf("expected 1 delivery (position > 2), got %d", received.Load())
	}
}
