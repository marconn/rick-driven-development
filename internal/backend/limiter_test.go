package backend

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBackend struct {
	name  string
	inUse atomic.Int32
	peak  atomic.Int32
	delay time.Duration
	runs  atomic.Int32
}

func (f *fakeBackend) Name() string               { return f.name }
func (f *fakeBackend) Capabilities() Capabilities { return Capabilities{} }

func (f *fakeBackend) Run(ctx context.Context, _ Request) (*Response, error) {
	cur := f.inUse.Add(1)
	defer f.inUse.Add(-1)
	for {
		peak := f.peak.Load()
		if cur <= peak || f.peak.CompareAndSwap(peak, cur) {
			break
		}
	}
	f.runs.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &Response{Output: "ok"}, nil
}

func TestNewLimitedBackend_UnlimitedWhenLimitZero(t *testing.T) {
	f := &fakeBackend{name: "claude"}
	b := NewLimitedBackend(f, 0, nil)
	if b != f {
		t.Fatalf("limit<=0 must return the inner backend unchanged")
	}
}

func TestLimitedBackend_EnforcesCap(t *testing.T) {
	f := &fakeBackend{name: "claude", delay: 20 * time.Millisecond}
	b := NewLimitedBackend(f, 2, nil)

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			_, _ = b.Run(context.Background(), Request{})
		}()
	}
	wg.Wait()

	if f.runs.Load() != callers {
		t.Fatalf("expected %d runs, got %d", callers, f.runs.Load())
	}
	if peak := f.peak.Load(); peak > 2 {
		t.Fatalf("peak concurrency %d exceeded cap of 2", peak)
	}
}

func TestLimitedBackend_ContextCancelledWhileWaiting(t *testing.T) {
	f := &fakeBackend{name: "claude", delay: 500 * time.Millisecond}
	b := NewLimitedBackend(f, 1, nil)

	// Occupy the only slot.
	ready := make(chan struct{})
	go func() {
		close(ready)
		_, _ = b.Run(context.Background(), Request{})
	}()
	<-ready
	// Give the first goroutine time to acquire the slot.
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := b.Run(ctx, Request{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

type captureRecorder struct {
	mu       sync.Mutex
	acquired []time.Duration
	released int
}

func (c *captureRecorder) Acquired(_ string, waited time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acquired = append(c.acquired, waited)
}

func (c *captureRecorder) Released(_ string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released++
}

func TestLimitedBackend_RecorderObserves(t *testing.T) {
	rec := &captureRecorder{}
	f := &fakeBackend{name: "claude", delay: 10 * time.Millisecond}
	b := NewLimitedBackend(f, 1, rec)

	const calls = 3
	for i := range calls {
		if _, err := b.Run(context.Background(), Request{}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if len(rec.acquired) != calls {
		t.Fatalf("acquired fires = %d, want %d", len(rec.acquired), calls)
	}
	if rec.released != calls {
		t.Fatalf("released fires = %d, want %d", rec.released, calls)
	}
}

func TestConcurrencyLimitFor_EnvParsing(t *testing.T) {
	cases := []struct {
		value string
		want  int
	}{
		{"", 0},
		{"0", 0},
		{"-5", 0},
		{"not-a-number", 0},
		{"3", 3},
		{"100", 100},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("RICK_BACKEND_CONCURRENCY_CLAUDE", tc.value)
			got := concurrencyLimitFor("claude")
			if got != tc.want {
				t.Fatalf("concurrencyLimitFor(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestNewWithRecorder_WrapsWhenEnvSet(t *testing.T) {
	t.Setenv("RICK_BACKEND_CONCURRENCY_CLAUDE", "2")
	rec := &captureRecorder{}
	b, err := NewWithRecorder("claude", rec)
	if err != nil {
		t.Fatalf("NewWithRecorder: %v", err)
	}
	if _, ok := b.(*limitedBackend); !ok {
		t.Fatalf("expected *limitedBackend wrap, got %T", b)
	}
}

func TestNewWithRecorder_NoWrapWhenEnvUnset(t *testing.T) {
	// t.Setenv guarantees a clean value; setting empty forces unset-like behavior.
	t.Setenv("RICK_BACKEND_CONCURRENCY_CLAUDE", "")
	b, err := NewWithRecorder("claude", nil)
	if err != nil {
		t.Fatalf("NewWithRecorder: %v", err)
	}
	if _, ok := b.(*limitedBackend); ok {
		t.Fatalf("expected raw backend, got wrapped *limitedBackend")
	}
}
