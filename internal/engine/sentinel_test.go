package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

func TestSentinel_DetectsUnhandledEvent(t *testing.T) {
	bus := eventbus.NewChannelBus()
	defer func() { _ = bus.Close() }()

	reg := handler.NewRegistry()
	sentinel := NewSentinel(bus, reg, slog.Default())

	detected := make(chan event.Envelope, 1)
	sentinel.onUnhandled = func(_ context.Context, env event.Envelope) {
		detected <- env
	}
	sentinel.Start()
	defer sentinel.Stop()

	// Publish a custom event type that no handler subscribes to.
	unknownEvt := event.New("custom.unknown", 1, []byte(`{}`)).
		WithCorrelation("wf-1").
		WithSource("test")

	ctx := context.Background()
	if err := bus.Publish(ctx, unknownEvt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case env := <-detected:
		if env.Type != "custom.unknown" {
			t.Errorf("expected custom.unknown, got %s", env.Type)
		}
		t.Logf("sentinel detected unhandled: %s", env.Type)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for sentinel detection")
	}
}

func TestSentinel_IgnoresHandledEvent(t *testing.T) {
	bus := eventbus.NewChannelBus()
	defer func() { _ = bus.Close() }()

	reg := handler.NewRegistry()

	// Register a handler that subscribes to "custom.handled".
	_ = reg.Register(&mockSentinelHandler{
		name: "my-handler",
		subs: []event.Type{"custom.handled"},
	})

	sentinel := NewSentinel(bus, reg, slog.Default())

	var detectedCount atomic.Int32
	sentinel.onUnhandled = func(_ context.Context, _ event.Envelope) {
		detectedCount.Add(1)
	}
	sentinel.Start()
	defer sentinel.Stop()

	ctx := context.Background()
	handledEvt := event.New("custom.handled", 1, []byte(`{}`)).
		WithCorrelation("wf-1").
		WithSource("test")
	_ = bus.Publish(ctx, handledEvt)

	time.Sleep(100 * time.Millisecond)
	if detectedCount.Load() != 0 {
		t.Error("sentinel should not flag events that have registered handlers")
	}
}

func TestSentinel_IgnoresInternalEvents(t *testing.T) {
	bus := eventbus.NewChannelBus()
	defer func() { _ = bus.Close() }()

	reg := handler.NewRegistry()
	sentinel := NewSentinel(bus, reg, slog.Default())

	var detectedCount atomic.Int32
	sentinel.onUnhandled = func(_ context.Context, _ event.Envelope) {
		detectedCount.Add(1)
	}
	sentinel.Start()
	defer sentinel.Stop()

	ctx := context.Background()

	// Publish several internal event types — none should trigger sentinel.
	for _, et := range []event.Type{
		event.WorkflowStarted,
		event.PersonaCompleted,
		event.AIResponseReceived,
		event.HintEmitted,
		event.OperatorGuidance,
		event.ContextEnrichment,
		event.ChildWorkflowCompleted,
	} {
		evt := event.New(et, 1, []byte(`{}`)).WithCorrelation("wf-1").WithSource("test")
		_ = bus.Publish(ctx, evt)
	}

	time.Sleep(100 * time.Millisecond)
	if detectedCount.Load() != 0 {
		t.Errorf("sentinel should ignore internal events, detected %d", detectedCount.Load())
	}
}

func TestSentinel_PublishesUnhandledEventDetected(t *testing.T) {
	bus := eventbus.NewChannelBus()
	defer func() { _ = bus.Close() }()

	reg := handler.NewRegistry()
	sentinel := NewSentinel(bus, reg, slog.Default())
	sentinel.Start()
	defer sentinel.Stop()

	// Subscribe to the alert event.
	alertCh := make(chan event.Envelope, 1)
	unsub := bus.Subscribe(event.UnhandledEventDetected, func(_ context.Context, env event.Envelope) error {
		alertCh <- env
		return nil
	}, eventbus.WithName("test:alert"))
	defer unsub()

	ctx := context.Background()
	unknownEvt := event.New("orphan.event", 1, []byte(`{}`)).
		WithCorrelation("wf-orphan").
		WithSource("external-system")
	_ = bus.Publish(ctx, unknownEvt)

	select {
	case alert := <-alertCh:
		var p event.UnhandledEventPayload
		if err := json.Unmarshal(alert.Payload, &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if p.EventType != "orphan.event" {
			t.Errorf("expected event_type orphan.event, got %s", p.EventType)
		}
		if p.CorrelationID != "wf-orphan" {
			t.Errorf("expected correlation wf-orphan, got %s", p.CorrelationID)
		}
		if p.Source != "external-system" {
			t.Errorf("expected source external-system, got %s", p.Source)
		}
		if alert.Source != "sentinel" {
			t.Errorf("expected alert source sentinel, got %s", alert.Source)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for UnhandledEventDetected")
	}
}

func TestSentinel_DynamicHandlerRegistration(t *testing.T) {
	bus := eventbus.NewChannelBus()
	defer func() { _ = bus.Close() }()

	reg := handler.NewRegistry()
	sentinel := NewSentinel(bus, reg, slog.Default())

	var detectedCount atomic.Int32
	sentinel.onUnhandled = func(_ context.Context, _ event.Envelope) {
		detectedCount.Add(1)
	}
	sentinel.Start()
	defer sentinel.Stop()

	ctx := context.Background()

	// First publish: no handler → should detect.
	evt1 := event.New("dynamic.event", 1, []byte(`{}`)).WithCorrelation("wf-1").WithSource("test")
	_ = bus.Publish(ctx, evt1)
	time.Sleep(100 * time.Millisecond)
	if detectedCount.Load() != 1 {
		t.Fatalf("expected 1 detection before handler registration, got %d", detectedCount.Load())
	}

	// Register handler for this event type.
	_ = reg.Register(&mockSentinelHandler{
		name: "late-handler",
		subs: []event.Type{"dynamic.event"},
	})

	// Second publish: handler exists → should NOT detect.
	evt2 := event.New("dynamic.event", 1, []byte(`{}`)).WithCorrelation("wf-2").WithSource("test")
	_ = bus.Publish(ctx, evt2)
	time.Sleep(100 * time.Millisecond)
	if detectedCount.Load() != 1 {
		t.Errorf("expected still 1 detection after handler registration, got %d", detectedCount.Load())
	}
}

// mockSentinelHandler is a minimal handler for sentinel tests.
type mockSentinelHandler struct {
	name string
	subs []event.Type
}

func (h *mockSentinelHandler) Name() string             { return h.name }
func (h *mockSentinelHandler) Subscribes() []event.Type { return h.subs }
func (h *mockSentinelHandler) Handle(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
	return nil, nil
}
