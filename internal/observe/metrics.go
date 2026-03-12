package observe

import (
	"sync"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// metricKey groups metrics by event type and handler.
type metricKey struct {
	EventType event.Type
	Handler   string
}

// EventMetric holds aggregated metrics for a specific event type + handler pair.
type EventMetric struct {
	EventType  event.Type
	Handler    string
	Count      int64
	Errors     int64
	TotalNanos int64
	MinNanos   int64
	MaxNanos   int64
}

// Metrics is a thread-safe in-memory metrics collector.
// Implements eventbus.MetricsRecorder.
type Metrics struct {
	mu      sync.RWMutex
	metrics map[metricKey]*EventMetric
}

// NewMetrics creates a new in-memory metrics collector.
func NewMetrics() *Metrics {
	return &Metrics{
		metrics: make(map[metricKey]*EventMetric),
	}
}

// RecordEventProcessed records a single event processing observation.
func (m *Metrics) RecordEventProcessed(eventType event.Type, handlerName string, duration time.Duration, err error) {
	nanos := duration.Nanoseconds()

	m.mu.Lock()
	defer m.mu.Unlock()

	key := metricKey{EventType: eventType, Handler: handlerName}
	em, ok := m.metrics[key]
	if !ok {
		em = &EventMetric{
			EventType: eventType,
			Handler:   handlerName,
			MinNanos:  nanos,
			MaxNanos:  nanos,
		}
		m.metrics[key] = em
	}

	em.Count++
	em.TotalNanos += nanos
	if err != nil {
		em.Errors++
	}
	if nanos < em.MinNanos {
		em.MinNanos = nanos
	}
	if nanos > em.MaxNanos {
		em.MaxNanos = nanos
	}
}

// Get returns the metric for a specific event type and handler.
func (m *Metrics) Get(eventType event.Type, handler string) (EventMetric, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	em, ok := m.metrics[metricKey{EventType: eventType, Handler: handler}]
	if !ok {
		return EventMetric{}, false
	}
	return *em, true
}

// All returns all collected metrics.
func (m *Metrics) All() []EventMetric {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]EventMetric, 0, len(m.metrics))
	for _, em := range m.metrics {
		result = append(result, *em)
	}
	return result
}

// AvgDuration returns the average processing duration for a metric.
func (em EventMetric) AvgDuration() time.Duration {
	if em.Count == 0 {
		return 0
	}
	return time.Duration(em.TotalNanos / em.Count)
}

// ErrorRate returns the fraction of calls that resulted in errors.
func (em EventMetric) ErrorRate() float64 {
	if em.Count == 0 {
		return 0
	}
	return float64(em.Errors) / float64(em.Count)
}
