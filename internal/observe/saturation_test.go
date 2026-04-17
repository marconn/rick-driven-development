package observe

import (
	"sync"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
)

// Compile-time assertion: *Saturation satisfies backend.Recorder.
var _ backend.Recorder = (*Saturation)(nil)

func TestSaturation_FastPathAcquiresDoNotInflateWait(t *testing.T) {
	s := NewSaturation()
	s.Acquired("claude", 0)
	s.Acquired("claude", 0)

	got, ok := s.Get("claude")
	if !ok {
		t.Fatal("expected claude entry after two acquires")
	}
	if got.Acquires != 2 {
		t.Errorf("acquires = %d, want 2", got.Acquires)
	}
	if got.Waited != 0 {
		t.Errorf("waited = %d, want 0 (fast path shouldn't count)", got.Waited)
	}
	if got.TotalWaitNanos != 0 || got.MaxWaitNanos != 0 {
		t.Errorf("fast-path acquires must not touch wait totals, got %+v", got)
	}
}

func TestSaturation_RecordsWaitTime(t *testing.T) {
	s := NewSaturation()
	s.Acquired("gemini", 10*time.Millisecond)
	s.Acquired("gemini", 30*time.Millisecond)

	got, _ := s.Get("gemini")
	if got.Waited != 2 {
		t.Errorf("waited = %d, want 2", got.Waited)
	}
	if got.AvgWait() != 20*time.Millisecond {
		t.Errorf("avg wait = %v, want 20ms", got.AvgWait())
	}
	if got.MaxWait() != 30*time.Millisecond {
		t.Errorf("max wait = %v, want 30ms", got.MaxWait())
	}
}

func TestSaturation_InflightGaugeBalances(t *testing.T) {
	s := NewSaturation()
	s.Acquired("claude", 0)
	s.Acquired("claude", 0)
	s.Acquired("claude", 0)
	s.Released("claude")

	got, _ := s.Get("claude")
	if got.Inflight != 2 {
		t.Errorf("inflight = %d, want 2", got.Inflight)
	}
	if got.Acquires != 3 {
		t.Errorf("acquires = %d, want 3 (lifetime total)", got.Acquires)
	}
}

func TestSaturation_ReleaseUnderflowIsSafe(t *testing.T) {
	s := NewSaturation()
	s.Released("claude") // no matching acquire
	if got := s.Snapshot(); len(got) != 0 {
		t.Errorf("release on unknown backend should not create entry, got %+v", got)
	}

	s.Acquired("claude", 0)
	s.Released("claude")
	s.Released("claude") // extra release — must not go negative
	got, _ := s.Get("claude")
	if got.Inflight < 0 {
		t.Errorf("inflight went negative: %d", got.Inflight)
	}
}

func TestSaturation_ConcurrentSafe(t *testing.T) {
	s := NewSaturation()
	var wg sync.WaitGroup
	const workers = 32
	const ops = 200

	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for range ops {
				s.Acquired("claude", time.Microsecond)
				s.Released("claude")
			}
		}()
	}
	wg.Wait()

	got, _ := s.Get("claude")
	if got.Acquires != workers*ops {
		t.Errorf("acquires = %d, want %d", got.Acquires, workers*ops)
	}
	if got.Inflight != 0 {
		t.Errorf("inflight = %d, want 0 after balanced acquire/release", got.Inflight)
	}
}
