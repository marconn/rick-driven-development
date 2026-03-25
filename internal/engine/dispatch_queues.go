package engine

import (
	"sync"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// dispatchItem is a single queued event with its dispatch priority.
type dispatchItem struct {
	priority   int // lower = higher priority
	env        event.Envelope
	chainDepth int
}

// dispatchQueue is a per-(handler, correlation) priority queue.
type dispatchQueue struct {
	mu       sync.Mutex
	items    []dispatchItem
	draining bool
}

// push adds an item to the queue in priority order (stable: FIFO within same priority).
func (q *dispatchQueue) push(item dispatchItem) {
	pos := len(q.items)
	for i, existing := range q.items {
		if item.priority < existing.priority {
			pos = i
			break
		}
	}
	q.items = append(q.items, dispatchItem{})
	copy(q.items[pos+1:], q.items[pos:])
	q.items[pos] = item
}

// pop removes and returns the highest-priority (lowest value) item.
func (q *dispatchQueue) pop() (dispatchItem, bool) {
	if len(q.items) == 0 {
		return dispatchItem{}, false
	}
	item := q.items[0]
	q.items = q.items[1:]
	return item, true
}

// dispatchQueues manages per-(handler, correlation) dispatch queues. Thread-safe.
type dispatchQueues struct {
	mu     sync.Mutex
	queues map[string]*dispatchQueue
}

func newDispatchQueues() *dispatchQueues {
	return &dispatchQueues{
		queues: make(map[string]*dispatchQueue),
	}
}

// getOrCreate returns the queue for the given key, creating it if needed.
func (dq *dispatchQueues) getOrCreate(key string) *dispatchQueue {
	dq.mu.Lock()
	q, exists := dq.queues[key]
	if !exists {
		q = &dispatchQueue{}
		dq.queues[key] = q
	}
	dq.mu.Unlock()
	return q
}
