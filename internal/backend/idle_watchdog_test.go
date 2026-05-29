package backend

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestWithIdleTimeout_DisabledWhenZero(t *testing.T) {
	parent := context.Background()
	ctx, progress, stop := WithIdleTimeout(parent, 0)
	defer stop()
	if ctx != parent {
		t.Fatal("idle=0 must return the parent ctx unchanged")
	}
	// progress and stop must both be no-ops — just make sure they don't panic.
	progress()
	progress()
}

func TestWithIdleTimeout_CancelsAfterIdle(t *testing.T) {
	ctx, _, stop := WithIdleTimeout(context.Background(), 30*time.Millisecond)
	defer stop()

	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrIdleTimeout) {
			t.Fatalf("expected ErrIdleTimeout cause, got %v", context.Cause(ctx))
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("context did not cancel within 500ms despite 30ms idle")
	}
}

func TestWithIdleTimeout_ProgressResetsTimer(t *testing.T) {
	ctx, progress, stop := WithIdleTimeout(context.Background(), 80*time.Millisecond)
	defer stop()

	// Keep kicking the watchdog for 300ms — much longer than the idle window.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		progress()
		time.Sleep(20 * time.Millisecond)
	}

	if err := ctx.Err(); err != nil {
		t.Fatalf("context cancelled despite continuous progress: %v (cause=%v)", err, context.Cause(ctx))
	}
}

func TestWithIdleTimeout_StopPreventsLaterFire(t *testing.T) {
	ctx, _, stop := WithIdleTimeout(context.Background(), 20*time.Millisecond)
	stop()

	// After stop, ctx should be done but NOT because of ErrIdleTimeout.
	select {
	case <-ctx.Done():
		if errors.Is(context.Cause(ctx), ErrIdleTimeout) {
			t.Fatal("stop should cancel without ErrIdleTimeout cause")
		}
	default:
		t.Fatal("stop should cancel the derived context")
	}
}

func TestWithIdleTimeout_ParentCancellationPropagates(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, _, stop := WithIdleTimeout(parent, time.Hour) // idle so long it won't fire
	defer stop()

	cancelParent()
	select {
	case <-ctx.Done():
		// Good — parent cancellation must propagate.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("parent cancellation did not propagate")
	}
}

func TestWithProgressTimeout_DisabledWhenZero(t *testing.T) {
	parent := context.Background()
	ctx, progress, stop := WithProgressTimeout(parent, 0)
	defer stop()
	if ctx != parent {
		t.Fatal("window=0 must return the parent ctx unchanged (default-off)")
	}
	progress()
	progress()
}

func TestWithProgressTimeout_CancelsWithProgressCause(t *testing.T) {
	ctx, _, stop := WithProgressTimeout(context.Background(), 30*time.Millisecond)
	defer stop()

	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrProgressTimeout) {
			t.Fatalf("expected ErrProgressTimeout cause, got %v", context.Cause(ctx))
		}
		if errors.Is(context.Cause(ctx), ErrIdleTimeout) {
			t.Fatal("progress watchdog must NOT report ErrIdleTimeout — the two causes must be distinguishable")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("context did not cancel within 500ms despite 30ms window")
	}
}

func TestWithProgressTimeout_ProgressResetsTimer(t *testing.T) {
	ctx, progress, stop := WithProgressTimeout(context.Background(), 80*time.Millisecond)
	defer stop()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		progress()
		time.Sleep(20 * time.Millisecond)
	}

	if err := ctx.Err(); err != nil {
		t.Fatalf("context cancelled despite continuous progress: %v (cause=%v)", err, context.Cause(ctx))
	}
}

// TestWithProgressTimeout_StackedOnIdle is the production wiring shape: the
// progress watchdog is a child of the idle watchdog. Raw bytes keep the idle
// watchdog alive (byteProgress) while NO substantive progress arrives, so the
// inner progress watchdog must still fire ErrProgressTimeout. This is the
// chatty-but-wedged tool-loop case in miniature.
func TestWithProgressTimeout_StackedOnIdle(t *testing.T) {
	idleCtx, byteProgress, stopIdle := WithIdleTimeout(context.Background(), 200*time.Millisecond)
	defer stopIdle()
	progCtx, _, stopProg := WithProgressTimeout(idleCtx, 60*time.Millisecond)
	defer stopProg()

	// Kick ONLY the byte watchdog — simulating stdout chatter with no text.
	go func() {
		for i := 0; i < 20; i++ {
			byteProgress()
			time.Sleep(20 * time.Millisecond)
		}
	}()

	select {
	case <-progCtx.Done():
		if !errors.Is(context.Cause(progCtx), ErrProgressTimeout) {
			t.Fatalf("expected ErrProgressTimeout while bytes flow but no progress, got %v", context.Cause(progCtx))
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("progress watchdog did not fire despite 60ms window with no substantive progress")
	}
}

func TestProgressTimeoutFromEnv(t *testing.T) {
	cases := []struct {
		value string
		want  time.Duration
	}{
		{"", 0}, // default-off, unlike the stall watchdog
		{"garbage", 0},
		{"-5s", 0},
		{"0", 0},
		{"45s", 45 * time.Second},
		{"15m", 15 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("RICK_BACKEND_PROGRESS_TIMEOUT", tc.value)
			got := progressTimeoutFromEnv()
			if got != tc.want {
				t.Errorf("progressTimeoutFromEnv(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestProgressWriter_FiresOnWrite(t *testing.T) {
	var count atomic.Int32
	var buf bytes.Buffer
	w := newProgressWriter(&buf, func() { count.Add(1) })

	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := w.Write([]byte("world")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := count.Load(); got != 2 {
		t.Errorf("progress fired %d times, want 2", got)
	}
	if buf.String() != "helloworld" {
		t.Errorf("forwarded output = %q, want helloworld", buf.String())
	}
}

func TestProgressWriter_EmptyWriteDoesNotFireProgress(t *testing.T) {
	var count atomic.Int32
	var buf bytes.Buffer
	w := newProgressWriter(&buf, func() { count.Add(1) })

	if _, err := w.Write(nil); err != nil {
		t.Fatalf("write nil: %v", err)
	}
	if _, err := w.Write([]byte{}); err != nil {
		t.Fatalf("write empty: %v", err)
	}

	if got := count.Load(); got != 0 {
		t.Errorf("empty writes must not fire progress, got %d", got)
	}
}

func TestProgressWriter_NilProgressReturnsInner(t *testing.T) {
	var buf bytes.Buffer
	w := newProgressWriter(&buf, nil)
	if w != &buf {
		t.Fatal("nil progress must return the inner writer unchanged (zero-overhead default)")
	}
}

func TestStallTimeoutFromEnv(t *testing.T) {
	cases := []struct {
		value string
		want  time.Duration
	}{
		{"", defaultStallTimeout},
		{"garbage", defaultStallTimeout},
		{"-5s", 0},
		{"0", 0},
		{"45s", 45 * time.Second},
		{"3m", 3 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("RICK_BACKEND_STALL_TIMEOUT", tc.value)
			got := stallTimeoutFromEnv()
			if got != tc.want {
				t.Errorf("stallTimeoutFromEnv(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
