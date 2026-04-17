package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
	"github.com/marconn/rick-event-driven-development/internal/observe"
)

// captureLogger builds a slog logger whose JSON output we can inspect.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func newTestEngine(t *testing.T) *engine.Engine {
	t.Helper()
	store, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	bus := eventbus.NewChannelBus()
	t.Cleanup(func() { _ = bus.Close() })
	return engine.NewEngine(store, bus, slog.Default())
}

func newTestRunner(t *testing.T) *engine.PersonaRunner {
	t.Helper()
	store, err := eventstore.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	bus := eventbus.NewChannelBus()
	t.Cleanup(func() { _ = bus.Close() })
	return engine.NewPersonaRunner(store, bus, nil, slog.Default())
}

func TestEmitSaturationSnapshot_SuppressesIdleTicks(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := captureLogger(buf)
	sat := observe.NewSaturation()

	emitSaturationSnapshot(logger, sat, newTestEngine(t), newTestRunner(t))

	if buf.Len() != 0 {
		t.Errorf("idle tick must emit nothing, got %q", buf.String())
	}
}

func TestEmitSaturationSnapshot_LogsWhenBackendActive(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := captureLogger(buf)
	sat := observe.NewSaturation()
	sat.Acquired("claude", 50*time.Millisecond)

	emitSaturationSnapshot(logger, sat, newTestEngine(t), newTestRunner(t))

	out := buf.String()
	if out == "" {
		t.Fatal("expected a log line when a backend has activity")
	}
	if !strings.Contains(out, `"msg":"saturation"`) {
		t.Errorf("expected saturation message, got %s", out)
	}
	if !strings.Contains(out, "backend_claude") {
		t.Errorf("expected backend_claude group, got %s", out)
	}
}

func TestEmitSaturationSnapshot_LogsWhenThrottleActive(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := captureLogger(buf)
	sat := observe.NewSaturation()

	eng := newTestEngine(t)
	eng.SetMaxConcurrentWorkflows(2)
	eng.WarmThrottle([]string{"wf-running"})

	emitSaturationSnapshot(logger, sat, eng, newTestRunner(t))

	out := buf.String()
	if !strings.Contains(out, `"throttle_running":1`) {
		t.Errorf("expected throttle_running=1, got %s", out)
	}
}
