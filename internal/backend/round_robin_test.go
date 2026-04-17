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

func (s *stubBackend) Name() string { return s.name }
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
