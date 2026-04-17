package backend

import (
	"context"
	"testing"
)

func TestRoundRobinStickyKey_DeterministicAndStable(t *testing.T) {
	a := &stubBackend{name: "a"}
	b := &stubBackend{name: "b"}
	c := &stubBackend{name: "c"}

	rr, err := NewRoundRobin(a, b, c)
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}

	ctx := WithStickyKey(context.Background(), "corr-1:reviewer")

	var first string
	for i := range 5 {
		resp, err := rr.Run(ctx, Request{})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if i == 0 {
			first = resp.Output
			continue
		}
		if resp.Output != first {
			t.Fatalf("sticky key drift: call %d returned %q, first returned %q", i, resp.Output, first)
		}
	}

	total := a.calls + b.calls + c.calls
	if total != 5 {
		t.Fatalf("expected 5 total calls, got %d", total)
	}
	// Exactly one backend should have handled all of them.
	hit := 0
	for _, n := range []int{a.calls, b.calls, c.calls} {
		if n > 0 {
			hit++
		}
	}
	if hit != 1 {
		t.Fatalf("sticky key did not pin to a single backend: a=%d b=%d c=%d", a.calls, b.calls, c.calls)
	}
}

func TestRoundRobinStickyKey_DifferentKeysCanSplit(t *testing.T) {
	a := &stubBackend{name: "a"}
	b := &stubBackend{name: "b"}
	c := &stubBackend{name: "c"}

	rr, err := NewRoundRobin(a, b, c)
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}

	// Collect which backend each key pins to. Keys chosen to cover all three
	// buckets under FNV-1a — if the hash were biased this would fail and
	// surface the regression quickly.
	keys := []string{
		"corr-alpha:reviewer",
		"corr-beta:reviewer",
		"corr-gamma:reviewer",
		"corr-delta:reviewer",
		"corr-epsilon:reviewer",
		"corr-zeta:reviewer",
	}
	hits := map[string]int{}
	for _, k := range keys {
		ctx := WithStickyKey(context.Background(), k)
		resp, err := rr.Run(ctx, Request{})
		if err != nil {
			t.Fatalf("run %q: %v", k, err)
		}
		hits[resp.Output]++
	}
	if len(hits) < 2 {
		t.Fatalf("expected sticky keys to span at least 2 backends, got %v", hits)
	}
}

func TestRoundRobinStickyKey_EmptyFallsBackToCounter(t *testing.T) {
	a := &stubBackend{name: "a"}
	b := &stubBackend{name: "b"}

	rr, err := NewRoundRobin(a, b)
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}

	// No sticky key → counter rotation → strict alternation.
	want := []string{"a", "b", "a", "b"}
	for i, expect := range want {
		resp, err := rr.Run(context.Background(), Request{})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if resp.Output != expect {
			t.Fatalf("call %d: got %q, want %q", i, resp.Output, expect)
		}
	}
}

func TestWithStickyKey_EmptyKeyNoOp(t *testing.T) {
	parent := context.Background()
	got := WithStickyKey(parent, "")
	if got != parent {
		t.Fatalf("empty key should return the same ctx, got a wrapped one")
	}
	if stickyKey(parent) != "" {
		t.Fatalf("empty parent ctx should have no sticky key")
	}
}

func TestStickyIndex_DegenerateBackendCount(t *testing.T) {
	if got := stickyIndex("any", 0); got != 0 {
		t.Fatalf("n=0 must return 0, got %d", got)
	}
	if got := stickyIndex("any", 1); got != 0 {
		t.Fatalf("n=1 must return 0, got %d", got)
	}
}
