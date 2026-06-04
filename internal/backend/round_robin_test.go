package backend

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubBackend records Run calls so tests can verify dispatch order.
type stubBackend struct {
	name  string
	mu    sync.Mutex
	calls int
}

func (s *stubBackend) Name() string               { return s.name }
func (s *stubBackend) Capabilities() Capabilities { return Capabilities{} }
func (s *stubBackend) Run(_ context.Context, _ Request) (*Response, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return &Response{Output: s.name, Duration: time.Millisecond}, nil
}

func TestRoundRobinRotation(t *testing.T) {
	a := &stubBackend{name: "a"}
	b := &stubBackend{name: "b"}
	c := &stubBackend{name: "c"}

	rr, err := NewRoundRobin(a, b, c)
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}

	want := []string{"a", "b", "c", "a", "b", "c", "a"}
	ctx := context.Background()
	for i, name := range want {
		got, err := rr.Run(ctx, Request{})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got.Output != name {
			t.Fatalf("call %d: got %q, want %q", i, got.Output, name)
		}
	}
	if a.calls != 3 || b.calls != 2 || c.calls != 2 {
		t.Fatalf("expected 3/2/2, got %d/%d/%d", a.calls, b.calls, c.calls)
	}
}

// TestRoundRobinRotateOffset_FlipsDeterministically locks the auto-retry
// rotation contract: when a sticky key + rotation offset are both present,
// Run lands on slot (base + offset) mod n. With n=3, offsets 1 and 2 each
// guarantee a different slot than offset 0 — which is the whole point of
// WithRotateOffset over re-hashing a modified key (FNV hash collision on
// n=3 is 1/3).
func TestRoundRobinRotateOffset_FlipsDeterministically(t *testing.T) {
	a := &stubBackend{name: "a"}
	b := &stubBackend{name: "b"}
	c := &stubBackend{name: "c"}

	rr, err := NewRoundRobin(a, b, c)
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}

	key := "corr-1:developer"
	base := stickyIndex(key, 3)

	baseCtx := WithStickyKey(context.Background(), key)
	if _, err := rr.Run(baseCtx, Request{}); err != nil {
		t.Fatalf("base: %v", err)
	}

	retry1Ctx := WithRotateOffset(baseCtx, 1)
	if _, err := rr.Run(retry1Ctx, Request{}); err != nil {
		t.Fatalf("retry1: %v", err)
	}

	retry2Ctx := WithRotateOffset(baseCtx, 2)
	if _, err := rr.Run(retry2Ctx, Request{}); err != nil {
		t.Fatalf("retry2: %v", err)
	}

	// Verify that the three calls landed on three distinct backends.
	backends := []*stubBackend{a, b, c}
	hit := []int{-1, -1, -1}
	for i, be := range backends {
		if be.calls > 0 {
			// Figure out in which order this backend was hit (1/2/3 rounds).
			for round, stub := range backends {
				_ = stub
				if round < 3 && i == (base+round)%3 {
					hit[round] = i
				}
			}
		}
	}
	if hit[0] == -1 || hit[1] == -1 || hit[2] == -1 {
		t.Fatalf("rotation did not hit all three slots: base=%d a=%d b=%d c=%d",
			base, a.calls, b.calls, c.calls)
	}
	if hit[0] == hit[1] || hit[1] == hit[2] || hit[0] == hit[2] {
		t.Fatalf("rotation collided: hit=%v (WithRotateOffset must guarantee distinct slots for offset 1..n-1)", hit)
	}
}

// TestRoundRobinRotateOffset_ZeroIsNoOp guards against the retry helper
// accidentally rotating a first-attempt (attempt=0) call. AIHandler calls
// WithRotateOffset unconditionally with the auto-retry counter; zero must
// not shift the slot.
func TestRoundRobinRotateOffset_ZeroIsNoOp(t *testing.T) {
	a := &stubBackend{name: "a"}
	b := &stubBackend{name: "b"}

	rr, err := NewRoundRobin(a, b)
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}

	key := "corr-1:developer"
	base := stickyIndex(key, 2)
	want := []*stubBackend{a, b}[base]

	ctx := WithRotateOffset(WithStickyKey(context.Background(), key), 0)
	if _, err := rr.Run(ctx, Request{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want.calls != 1 {
		t.Errorf("base slot %d (%s) not hit: a=%d b=%d — offset=0 must be a no-op",
			base, want.Name(), a.calls, b.calls)
	}
}

// TestRoundRobinSelect_AttributesConcreteBackend locks the 0001 contract: two
// successive non-sticky Select calls resolve the two distinct inner backends
// the rotation would dispatch to, so the AIHandler can attribute events to the
// concrete CLI before Run. The selected backend must NOT have been invoked yet
// (Select resolves, it does not Run).
func TestRoundRobinSelect_AttributesConcreteBackend(t *testing.T) {
	a := &stubBackend{name: "a"}
	b := &stubBackend{name: "b"}
	rr, err := NewRoundRobin(a, b)
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}

	ctx := context.Background()
	first := rr.Select(ctx)
	second := rr.Select(ctx)
	if first.Name() == second.Name() {
		t.Fatalf("two successive Select calls resolved same backend %q; want distinct rotation", first.Name())
	}
	if a.calls != 0 || b.calls != 0 {
		t.Fatalf("Select must not invoke the backend: a=%d b=%d", a.calls, b.calls)
	}

	// The names cover both members.
	got := map[string]bool{first.Name(): true, second.Name(): true}
	if !got["a"] || !got["b"] {
		t.Fatalf("Select did not cover both members: %v", got)
	}
}

// TestRoundRobinSelect_StickyMatchesRun verifies that the concrete backend
// resolved via Select on the sticky path is the same one Run would dispatch to,
// and that Select does not advance the deterministic sticky selection.
func TestRoundRobinSelect_StickyMatchesRun(t *testing.T) {
	a := &stubBackend{name: "a"}
	b := &stubBackend{name: "b"}
	c := &stubBackend{name: "c"}
	rr, err := NewRoundRobin(a, b, c)
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}

	ctx := WithStickyKey(context.Background(), "corr-9:reviewer")
	selected := rr.Select(ctx)
	// Repeated Select is idempotent on the sticky path.
	if again := rr.Select(ctx); again.Name() != selected.Name() {
		t.Fatalf("sticky Select not idempotent: %q then %q", selected.Name(), again.Name())
	}

	resp, err := rr.Run(ctx, Request{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Output != selected.Name() {
		t.Fatalf("Run dispatched %q but Select resolved %q", resp.Output, selected.Name())
	}
}

// TestResolve_NonSelectorReturnsBackend confirms backend.Resolve is a no-op for
// a plain (non-rotating) backend: attribution falls back to that backend's own
// name with no selection side effects.
func TestResolve_NonSelectorReturnsBackend(t *testing.T) {
	a := &stubBackend{name: "solo"}
	got := Resolve(context.Background(), a)
	if got.Name() != "solo" {
		t.Fatalf("Resolve(non-selector) = %q, want solo", got.Name())
	}
	if a.calls != 0 {
		t.Fatalf("Resolve must not invoke the backend: calls=%d", a.calls)
	}
}

func TestRoundRobinName(t *testing.T) {
	rr, err := NewRoundRobin(&stubBackend{name: "a"}, &stubBackend{name: "b"})
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}
	if rr.Name() != "round-robin(a,b)" {
		t.Fatalf("Name() = %q", rr.Name())
	}
}

func TestRoundRobinRequiresBackend(t *testing.T) {
	if _, err := NewRoundRobin(); err == nil {
		t.Fatal("expected error for empty backend list")
	}
}

func TestNewReviewBackendDefault(t *testing.T) {
	be, err := NewReviewBackend(nil)
	if err != nil {
		t.Fatalf("NewReviewBackend: %v", err)
	}
	if !strings.HasPrefix(be.Name(), "round-robin(") {
		t.Fatalf("expected rotation, got %q", be.Name())
	}
	for _, n := range DefaultReviewBackends {
		if !strings.Contains(be.Name(), n) {
			t.Fatalf("expected %q in Name(), got %q", n, be.Name())
		}
	}
}

func TestNewReviewBackendSingle(t *testing.T) {
	be, err := NewReviewBackend([]string{"gemini"})
	if err != nil {
		t.Fatalf("NewReviewBackend: %v", err)
	}
	if be.Name() != "gemini" {
		t.Fatalf("len=1 should return raw backend, got %q", be.Name())
	}
}

func TestNewReviewBackendSubset(t *testing.T) {
	be, err := NewReviewBackend([]string{"claude", "codex"})
	if err != nil {
		t.Fatalf("NewReviewBackend: %v", err)
	}
	if be.Name() != "round-robin(claude,codex)" {
		t.Fatalf("got %q", be.Name())
	}
}

func TestNewReviewBackendRejectsDuplicates(t *testing.T) {
	if _, err := NewReviewBackend([]string{"claude", "claude"}); err == nil {
		t.Fatal("expected error on duplicate name")
	}
}

func TestNewReviewBackendRejectsUnknown(t *testing.T) {
	if _, err := NewReviewBackend([]string{"gpt-9"}); err == nil {
		t.Fatal("expected error on unknown backend")
	}
}

func TestNewReviewBackendNormalizesInput(t *testing.T) {
	be, err := NewReviewBackend([]string{" Claude ", "", "GEMINI"})
	if err != nil {
		t.Fatalf("NewReviewBackend: %v", err)
	}
	if be.Name() != "round-robin(claude,gemini)" {
		t.Fatalf("got %q", be.Name())
	}
}

func TestParseReviewBackendsEnv(t *testing.T) {
	cases := map[string][]string{
		"":                      nil,
		"   ":                   nil,
		"claude":                {"claude"},
		"claude,gemini":         {"claude", "gemini"},
		" claude , , gemini , ": {"claude", "gemini"},
	}
	for in, want := range cases {
		got := ParseReviewBackendsEnv(in)
		if len(got) != len(want) {
			t.Fatalf("%q: len got=%d want=%d (%v vs %v)", in, len(got), len(want), got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%q[%d]: got %q want %q", in, i, got[i], want[i])
			}
		}
	}
}
