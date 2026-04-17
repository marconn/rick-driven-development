package observe

import (
	"sync"
	"time"
)

// Saturation tracks per-backend concurrency pressure so operators can see when
// RICK_BACKEND_CONCURRENCY_* caps are being hit. It implements the
// backend.Recorder interface (structurally — no import dep, avoids a cycle).
//
// Counters are lifetime totals; Inflight is the live gauge. Wait stats capture
// only acquires that actually blocked on the semaphore — fast-path acquires
// with zero wait increment Acquires but neither Waited nor any *WaitNanos
// field.
type Saturation struct {
	mu       sync.RWMutex
	backends map[string]*BackendSat
}

// BackendSat is the per-backend aggregate surfaced by Saturation.Snapshot.
// Values are plain fields (no methods) so callers can JSON-serialize or
// compare freely.
type BackendSat struct {
	Name           string
	Inflight       int64
	Acquires       int64
	Waited         int64
	TotalWaitNanos int64
	MaxWaitNanos   int64
}

// NewSaturation creates an empty tracker. Safe for concurrent use.
func NewSaturation() *Saturation {
	return &Saturation{backends: make(map[string]*BackendSat)}
}

// Acquired records a semaphore acquisition. Called by backend.Limiter.
func (s *Saturation) Acquired(name string, waited time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sat := s.getOrCreate(name)
	sat.Inflight++
	sat.Acquires++
	if waited > 0 {
		sat.Waited++
		nanos := waited.Nanoseconds()
		sat.TotalWaitNanos += nanos
		if nanos > sat.MaxWaitNanos {
			sat.MaxWaitNanos = nanos
		}
	}
}

// Released records a semaphore release. Called by backend.Limiter after the
// underlying Run returns.
func (s *Saturation) Released(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sat, ok := s.backends[name]; ok && sat.Inflight > 0 {
		sat.Inflight--
	}
}

// Snapshot returns a value copy of every tracked backend. Safe to mutate.
func (s *Saturation) Snapshot() []BackendSat {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]BackendSat, 0, len(s.backends))
	for _, sat := range s.backends {
		out = append(out, *sat)
	}
	return out
}

// Get returns a value copy of one backend's stats. ok=false when the backend
// has never recorded an acquire.
func (s *Saturation) Get(name string) (BackendSat, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sat, ok := s.backends[name]
	if !ok {
		return BackendSat{}, false
	}
	return *sat, true
}

// AvgWait returns the mean wait for acquires that blocked. Zero when nothing
// has blocked yet (guards divide-by-zero).
func (b BackendSat) AvgWait() time.Duration {
	if b.Waited == 0 {
		return 0
	}
	return time.Duration(b.TotalWaitNanos / b.Waited)
}

// MaxWait returns the longest single wait observed on this backend.
func (b BackendSat) MaxWait() time.Duration {
	return time.Duration(b.MaxWaitNanos)
}

func (s *Saturation) getOrCreate(name string) *BackendSat {
	sat, ok := s.backends[name]
	if !ok {
		sat = &BackendSat{Name: name}
		s.backends[name] = sat
	}
	return sat
}
