package backend

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
)

// RoundRobin distributes Run calls across a fixed list of backends using
// an atomic counter. Each call picks the next backend in order, wrapping
// around at the end. Used by review-phase handlers so that a wedged or
// rate-limited CLI only affects 1/N of review invocations instead of all.
type RoundRobin struct {
	backends []Backend
	counter  atomic.Uint64
	name     string
}

// NewRoundRobin constructs a RoundRobin over the given backends. The caller
// must provide at least one backend. The Name() of the returned backend is
// "round-robin(a,b,c)" where a,b,c are the underlying backend names.
func NewRoundRobin(backends ...Backend) (*RoundRobin, error) {
	if len(backends) == 0 {
		return nil, fmt.Errorf("round-robin: at least one backend required")
	}
	names := make([]string, 0, len(backends))
	for _, b := range backends {
		names = append(names, b.Name())
	}
	return &RoundRobin{
		backends: backends,
		name:     fmt.Sprintf("round-robin(%s)", strings.Join(names, ",")),
	}, nil
}

// Name returns a composite identifier, e.g. "round-robin(claude,gemini,codex)".
// Per-call inner backend selection is NOT surfaced through the AIHandler event
// stream today — AIRequestSent.Backend records this composite name, not the
// chosen inner backend. Operators needing per-call attribution should rely on
// process listing / CLI subprocess logs until the payload is extended.
func (r *RoundRobin) Name() string { return r.name }

// Run selects a backend and delegates. When the context carries a sticky key
// (see WithStickyKey), selection is deterministic based on the key — same key
// always maps to the same backend. Otherwise selection is per-call via the
// atomic counter: two concurrent review handlers land on different backends
// (provided len(backends) > 1).
//
// Sticky selection exists so that reviewer/qa personas stay on one backend
// across feedback-loop iterations, avoiding the "three iterations, three
// different backends, three different sets of issues, developer never
// converges" failure mode seen in production.
func (r *RoundRobin) Run(ctx context.Context, req Request) (*Response, error) {
	var idx int
	if key := stickyKey(ctx); key != "" {
		idx = stickyIndex(key, len(r.backends))
	} else {
		idx = int((r.counter.Add(1) - 1) % uint64(len(r.backends)))
	}
	return r.backends[idx].Run(ctx, req)
}

// Backends exposes the underlying list for inspection (primarily for logs
// and tests). The returned slice aliases internal state — do not mutate.
func (r *RoundRobin) Backends() []Backend { return r.backends }
