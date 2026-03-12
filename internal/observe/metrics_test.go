package observe

import (
	"fmt"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventbus"
)

// Verify Metrics implements MetricsRecorder at compile time.
var _ eventbus.MetricsRecorder = (*Metrics)(nil)

func TestMetrics_RecordsSuccess(t *testing.T) {
	m := NewMetrics()

	m.RecordEventProcessed(event.WorkflowStarted, "engine", 100*time.Millisecond, nil)
	m.RecordEventProcessed(event.WorkflowStarted, "engine", 200*time.Millisecond, nil)

	em, ok := m.Get(event.WorkflowStarted, "engine")
	if !ok {
		t.Fatal("metric not found")
	}
	if em.Count != 2 {
		t.Errorf("expected count 2, got %d", em.Count)
	}
	if em.Errors != 0 {
		t.Errorf("expected 0 errors, got %d", em.Errors)
	}
	if em.ErrorRate() != 0 {
		t.Errorf("expected error rate 0, got %f", em.ErrorRate())
	}
}

func TestMetrics_RecordsErrors(t *testing.T) {
	m := NewMetrics()
	err := fmt.Errorf("boom")

	m.RecordEventProcessed(event.PersonaFailed, "handler", 50*time.Millisecond, err)
	m.RecordEventProcessed(event.PersonaFailed, "handler", 60*time.Millisecond, nil)

	em, ok := m.Get(event.PersonaFailed, "handler")
	if !ok {
		t.Fatal("metric not found")
	}
	if em.Count != 2 {
		t.Errorf("expected count 2, got %d", em.Count)
	}
	if em.Errors != 1 {
		t.Errorf("expected 1 error, got %d", em.Errors)
	}
	if em.ErrorRate() != 0.5 {
		t.Errorf("expected error rate 0.5, got %f", em.ErrorRate())
	}
}

func TestMetrics_MinMaxDuration(t *testing.T) {
	m := NewMetrics()

	m.RecordEventProcessed(event.AIResponseReceived, "ai", 100*time.Millisecond, nil)
	m.RecordEventProcessed(event.AIResponseReceived, "ai", 500*time.Millisecond, nil)
	m.RecordEventProcessed(event.AIResponseReceived, "ai", 300*time.Millisecond, nil)

	em, ok := m.Get(event.AIResponseReceived, "ai")
	if !ok {
		t.Fatal("metric not found")
	}
	if time.Duration(em.MinNanos) != 100*time.Millisecond {
		t.Errorf("expected min 100ms, got %v", time.Duration(em.MinNanos))
	}
	if time.Duration(em.MaxNanos) != 500*time.Millisecond {
		t.Errorf("expected max 500ms, got %v", time.Duration(em.MaxNanos))
	}
	if em.AvgDuration() != 300*time.Millisecond {
		t.Errorf("expected avg 300ms, got %v", em.AvgDuration())
	}
}

func TestMetrics_SeparatesByKey(t *testing.T) {
	m := NewMetrics()

	m.RecordEventProcessed(event.PersonaCompleted, "handler-a", 10*time.Millisecond, nil)
	m.RecordEventProcessed(event.PersonaCompleted, "handler-b", 20*time.Millisecond, nil)
	m.RecordEventProcessed(event.VerdictRendered, "handler-a", 30*time.Millisecond, nil)

	all := m.All()
	if len(all) != 3 {
		t.Errorf("expected 3 distinct metrics, got %d", len(all))
	}
}

func TestMetrics_GetNotFound(t *testing.T) {
	m := NewMetrics()

	_, ok := m.Get(event.WorkflowStarted, "nonexistent")
	if ok {
		t.Error("expected not found")
	}
}

func TestMetrics_AvgDurationZero(t *testing.T) {
	em := EventMetric{Count: 0}
	if em.AvgDuration() != 0 {
		t.Error("avg duration should be 0 for 0 count")
	}
}
